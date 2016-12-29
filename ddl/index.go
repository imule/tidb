// Copyright 2015 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package ddl

import (
	"sort"
	"sync"
	"time"

	"github.com/juju/errors"
	"github.com/ngaut/log"
	"github.com/pingcap/tidb/ast"
	"github.com/pingcap/tidb/kv"
	"github.com/pingcap/tidb/meta"
	"github.com/pingcap/tidb/model"
	"github.com/pingcap/tidb/mysql"
	"github.com/pingcap/tidb/table"
	"github.com/pingcap/tidb/table/tables"
	"github.com/pingcap/tidb/tablecodec"
	"github.com/pingcap/tidb/terror"
	"github.com/pingcap/tidb/util"
	"github.com/pingcap/tidb/util/types"
)

const maxPrefixLength = 767

func buildIndexInfo(tblInfo *model.TableInfo, unique bool, indexName model.CIStr,
	idxColNames []*ast.IndexColName) (*model.IndexInfo, error) {
	// build offsets
	idxColumns := make([]*model.IndexColumn, 0, len(idxColNames))
	for _, ic := range idxColNames {
		col := findCol(tblInfo.Columns, ic.Column.Name.O)
		if col == nil {
			return nil, errKeyColumnDoesNotExits.Gen("column does not exist: %s",
				ic.Column.Name)
		}

		// Length must be specified for BLOB and TEXT column indexes.
		if types.IsTypeBlob(col.FieldType.Tp) && ic.Length == types.UnspecifiedLength {
			return nil, errors.Trace(errBlobKeyWithoutLength)
		}

		if ic.Length != types.UnspecifiedLength &&
			!types.IsTypeChar(col.FieldType.Tp) &&
			!types.IsTypeBlob(col.FieldType.Tp) {
			return nil, errors.Trace(errIncorrectPrefixKey)
		}

		if ic.Length > maxPrefixLength {
			return nil, errors.Trace(errTooLongKey)
		}

		idxColumns = append(idxColumns, &model.IndexColumn{
			Name:   col.Name,
			Offset: col.Offset,
			Length: ic.Length,
		})
	}
	// create index info
	idxInfo := &model.IndexInfo{
		Name:    indexName,
		Columns: idxColumns,
		Unique:  unique,
		State:   model.StateNone,
	}
	return idxInfo, nil
}

func addIndexColumnFlag(tblInfo *model.TableInfo, indexInfo *model.IndexInfo) {
	col := indexInfo.Columns[0]

	if indexInfo.Unique && len(indexInfo.Columns) == 1 {
		tblInfo.Columns[col.Offset].Flag |= mysql.UniqueKeyFlag
	} else {
		tblInfo.Columns[col.Offset].Flag |= mysql.MultipleKeyFlag
	}
}

func dropIndexColumnFlag(tblInfo *model.TableInfo, indexInfo *model.IndexInfo) {
	col := indexInfo.Columns[0]

	if indexInfo.Unique && len(indexInfo.Columns) == 1 {
		tblInfo.Columns[col.Offset].Flag &= ^uint(mysql.UniqueKeyFlag)
	} else {
		tblInfo.Columns[col.Offset].Flag &= ^uint(mysql.MultipleKeyFlag)
	}

	// other index may still cover this col
	for _, index := range tblInfo.Indices {
		if index.Name.L == indexInfo.Name.L {
			continue
		}

		if index.Columns[0].Name.L != col.Name.L {
			continue
		}

		addIndexColumnFlag(tblInfo, index)
	}
}

func (d *ddl) onCreateIndex(t *meta.Meta, job *model.Job) error {
	// Handle rollback job.
	if job.State == model.JobRollback {
		err := d.onDropIndex(t, job)
		if err != nil {
			return errors.Trace(err)
		}
		return nil
	}

	// Handle normal job.
	schemaID := job.SchemaID
	tblInfo, err := d.getTableInfo(t, job)
	if err != nil {
		return errors.Trace(err)
	}

	var (
		unique      bool
		indexName   model.CIStr
		idxColNames []*ast.IndexColName
	)
	err = job.DecodeArgs(&unique, &indexName, &idxColNames)
	if err != nil {
		job.State = model.JobCancelled
		return errors.Trace(err)
	}

	indexInfo := findIndexByName(indexName.L, tblInfo.Indices)
	if indexInfo != nil && indexInfo.State == model.StatePublic {
		job.State = model.JobCancelled
		return errDupKeyName.Gen("index already exist %s", indexName)
	}

	if indexInfo == nil {
		indexInfo, err = buildIndexInfo(tblInfo, unique, indexName, idxColNames)
		if err != nil {
			job.State = model.JobCancelled
			return errors.Trace(err)
		}
		indexInfo.ID = allocateIndexID(tblInfo)
		tblInfo.Indices = append(tblInfo.Indices, indexInfo)
	}

	ver, err := updateSchemaVersion(t, job)
	if err != nil {
		return errors.Trace(err)
	}

	switch indexInfo.State {
	case model.StateNone:
		// none -> delete only
		job.SchemaState = model.StateDeleteOnly
		indexInfo.State = model.StateDeleteOnly
		err = t.UpdateTable(schemaID, tblInfo)
		return errors.Trace(err)
	case model.StateDeleteOnly:
		// delete only -> write only
		job.SchemaState = model.StateWriteOnly
		indexInfo.State = model.StateWriteOnly
		err = t.UpdateTable(schemaID, tblInfo)
		return errors.Trace(err)
	case model.StateWriteOnly:
		// write only -> reorganization
		job.SchemaState = model.StateWriteReorganization
		indexInfo.State = model.StateWriteReorganization
		// Initialize SnapshotVer to 0 for later reorganization check.
		job.SnapshotVer = 0
		err = t.UpdateTable(schemaID, tblInfo)
		return errors.Trace(err)
	case model.StateWriteReorganization:
		// reorganization -> public
		reorgInfo, err := d.getReorgInfo(t, job)
		if err != nil || reorgInfo.first {
			// If we run reorg firstly, we should update the job snapshot version
			// and then run the reorg next time.
			return errors.Trace(err)
		}

		var tbl table.Table
		tbl, err = d.getTable(schemaID, tblInfo)
		if err != nil {
			return errors.Trace(err)
		}

		err = d.runReorgJob(func() error {
			return d.addTableIndex(tbl, indexInfo, reorgInfo, job)
		})
		if terror.ErrorEqual(err, errWaitReorgTimeout) {
			// if timeout, we should return, check for the owner and re-wait job done.
			return nil
		}
		if err != nil {
			if terror.ErrorEqual(err, kv.ErrKeyExists) {
				log.Warnf("[ddl] run DDL job %v err %v, convert job to rollback job", job, err)
				err = d.convert2RollbackJob(t, job, tblInfo, indexInfo)
			}
			return errors.Trace(err)
		}

		indexInfo.State = model.StatePublic
		// Set column index flag.
		addIndexColumnFlag(tblInfo, indexInfo)
		if err = t.UpdateTable(schemaID, tblInfo); err != nil {
			return errors.Trace(err)
		}

		// Finish this job.
		job.SchemaState = model.StatePublic
		job.State = model.JobDone
		addTableHistoryInfo(job, ver, tblInfo)
		return nil
	default:
		return ErrInvalidIndexState.Gen("invalid index state %v", tblInfo.State)
	}
}

func (d *ddl) convert2RollbackJob(t *meta.Meta, job *model.Job, tblInfo *model.TableInfo, indexInfo *model.IndexInfo) error {
	job.State = model.JobRollback
	job.Args = []interface{}{indexInfo.Name}
	// If add index job rollbacks in write reorganization state, its need to delete all keys which has been added.
	// Its work is the same as drop index job do.
	// The write reorganization state in add index job that likes write only state in drop index job.
	// So the next state is delete only state.
	indexInfo.State = model.StateDeleteOnly
	job.SchemaState = model.StateDeleteOnly
	err := t.UpdateTable(job.SchemaID, tblInfo)
	if err != nil {
		return errors.Trace(err)
	}
	err = kv.ErrKeyExists.Gen("Duplicate for key %s", indexInfo.Name.O)
	return errors.Trace(err)
}

func (d *ddl) onDropIndex(t *meta.Meta, job *model.Job) error {
	schemaID := job.SchemaID
	tblInfo, err := d.getTableInfo(t, job)
	if err != nil {
		return errors.Trace(err)
	}

	var indexName model.CIStr
	if err = job.DecodeArgs(&indexName); err != nil {
		job.State = model.JobCancelled
		return errors.Trace(err)
	}

	indexInfo := findIndexByName(indexName.L, tblInfo.Indices)
	if indexInfo == nil {
		job.State = model.JobCancelled
		return ErrCantDropFieldOrKey.Gen("index %s doesn't exist", indexName)
	}

	ver, err := updateSchemaVersion(t, job)
	if err != nil {
		return errors.Trace(err)
	}

	switch indexInfo.State {
	case model.StatePublic:
		// public -> write only
		job.SchemaState = model.StateWriteOnly
		indexInfo.State = model.StateWriteOnly
		err = t.UpdateTable(schemaID, tblInfo)
	case model.StateWriteOnly:
		// write only -> delete only
		job.SchemaState = model.StateDeleteOnly
		indexInfo.State = model.StateDeleteOnly
		err = t.UpdateTable(schemaID, tblInfo)
	case model.StateDeleteOnly:
		// delete only -> reorganization
		job.SchemaState = model.StateDeleteReorganization
		indexInfo.State = model.StateDeleteReorganization
		err = t.UpdateTable(schemaID, tblInfo)
	case model.StateDeleteReorganization:
		// reorganization -> absent
		err = d.runReorgJob(func() error {
			return d.dropTableIndex(indexInfo, job)
		})
		if terror.ErrorEqual(err, errWaitReorgTimeout) {
			// If the timeout happens, we should return.
			// Then check for the owner and re-wait job to finish.
			return nil
		}
		if err != nil {
			return errors.Trace(err)
		}

		// All reorganization jobs are done, drop this index.
		newIndices := make([]*model.IndexInfo, 0, len(tblInfo.Indices))
		for _, idx := range tblInfo.Indices {
			if idx.Name.L != indexName.L {
				newIndices = append(newIndices, idx)
			}
		}
		tblInfo.Indices = newIndices
		// Set column index flag.
		dropIndexColumnFlag(tblInfo, indexInfo)
		if err = t.UpdateTable(schemaID, tblInfo); err != nil {
			return errors.Trace(err)
		}

		// Finish this job.
		job.SchemaState = model.StateNone
		if job.State == model.JobRollback {
			job.State = model.JobRollbackDone
		} else {
			job.State = model.JobDone
		}
		addTableHistoryInfo(job, ver, tblInfo)
	default:
		err = ErrInvalidTableState.Gen("invalid table state %v", tblInfo.State)
	}
	return errors.Trace(err)
}

func isFinished(limit, input int64) bool {
	if limit == 0 || input < limit {
		return false
	}
	return true
}

func (d *ddl) fetchRowColVals(txn kv.Transaction, t table.Table, batchOpInfo *indexBatchOpInfo, handleInfo *handleInfo) (
	[]*indexRecord, *batchRet) {
	batchCnt := defaultSmallBatchCnt
	rawRecords := make([][]byte, 0, batchCnt)
	idxRecords := make([]*indexRecord, 0, batchCnt)
	ret := new(batchRet)
	err := d.iterateSnapshotRows(t, txn.StartTS(), handleInfo.startHandle,
		func(h int64, rowKey kv.Key, rawRecord []byte) (bool, error) {
			rawRecords = append(rawRecords, rawRecord)
			indexRecord := &indexRecord{handle: h, key: rowKey}
			idxRecords = append(idxRecords, indexRecord)
			if len(idxRecords) == batchCnt || isFinished(handleInfo.endHandle, h) {
				return false, nil
			}
			return true, nil
		})
	if err != nil {
		ret.err = errors.Trace(err)
		return nil, ret
	}

	ret.count = len(idxRecords)
	if ret.count > 0 {
		ret.doneHandle = idxRecords[ret.count-1].handle
	}
	// Be sure to do this operation only once.
	handleInfo.once.Do(func() {
		// Notice to start the next batch operation.
		batchOpInfo.nextCh <- ret.doneHandle
		// Record the last handle.
		// Ensure that the handle scope of the batch doesn't change,
		// even if the transaction retries it can't effect the other batches.
		handleInfo.endHandle = ret.doneHandle
	})
	if ret.count == 0 {
		return nil, ret
	}

	cols := t.Cols()
	idxInfo := batchOpInfo.tblIndex.Meta()
	for i, idxRecord := range idxRecords {
		rowMap, err := tablecodec.DecodeRow(rawRecords[i], batchOpInfo.colMap)
		if err != nil {
			ret.err = errors.Trace(err)
			return nil, ret
		}
		idxVal := make([]types.Datum, 0, len(idxInfo.Columns))
		for _, v := range idxInfo.Columns {
			col := cols[v.Offset]
			idxVal = append(idxVal, rowMap[col.ID])
		}
		idxRecord.vals = idxVal
	}
	return idxRecords, ret
}

const (
	defaultBatchCnt      = 1024
	defaultSmallBatchCnt = 128
	defaultSmallBatches  = 16
)

// batchRet is the result of the batch.
type batchRet struct {
	count      int
	doneHandle int64 // This is the last reorg handle that has been processed.
	err        error
}

type batchRetSlice []*batchRet

func (b batchRetSlice) Len() int           { return len(b) }
func (b batchRetSlice) Less(i, j int) bool { return b[i].doneHandle < b[j].doneHandle }
func (b batchRetSlice) Swap(i, j int)      { b[i], b[j] = b[j], b[i] }

type indexRecord struct {
	handle int64
	key    []byte
	vals   []types.Datum
}

type indexBatchOpInfo struct {
	tblIndex   table.Index
	colMap     map[int64]*types.FieldType
	batchRetCh chan *batchRet
	nextCh     chan int64 // It notifies to start the next batch.
}

// How to add index in reorganization state?
//  1. Generate a snapshot with special version.
//  2. Traverse the snapshot, get every row in the table.
//  3. For one row, if the row has been already deleted, skip to next row.
//  4. If not deleted, check whether index has existed, if existed, skip to next row.
//  5. If index doesn't exist, create the index and then continue to handle next row.
func (d *ddl) addTableIndex(t table.Table, indexInfo *model.IndexInfo, reorgInfo *reorgInfo, job *model.Job) error {
	cols := t.Cols()
	colMap := make(map[int64]*types.FieldType)
	for _, v := range indexInfo.Columns {
		col := cols[v.Offset]
		colMap[col.ID] = &col.FieldType
	}
	batches := defaultSmallBatches
	batchOpInfo := &indexBatchOpInfo{
		tblIndex:   tables.NewIndex(t.Meta(), indexInfo),
		colMap:     colMap,
		nextCh:     make(chan int64, 1),
		batchRetCh: make(chan *batchRet, batches),
	}

	addedCount := job.GetRowCount()
	seekHandle := reorgInfo.Handle
	wg := sync.WaitGroup{}
	for {
		startTime := time.Now()
		for i := 0; i < batches; i++ {
			wg.Add(1)
			go d.backfillIndex(t, batchOpInfo, seekHandle, &wg)
			handle := <-batchOpInfo.nextCh
			// There is no data to seek.
			if handle == 0 {
				break
			}
			seekHandle = handle + 1
		}
		wg.Wait()

		retCnt := len(batchOpInfo.batchRetCh)
		batchAddedCount, doneHandle, err := getCountAndHandle(batchOpInfo)
		// Update the reorg handle that has been processed.
		if batchAddedCount != 0 {
			err1 := kv.RunInNewTxn(d.store, true, func(txn kv.Transaction) error {
				return errors.Trace(reorgInfo.UpdateHandle(txn, doneHandle+1))
			})
			if err1 != nil {
				if err == nil {
					err = err1
				} else {
					log.Warnf("[ddl] add index failed when update handle %d, err %v", doneHandle, err)
				}
			}
		}

		addedCount += int64(batchAddedCount)
		sub := time.Since(startTime).Seconds()
		if err != nil {
			log.Warnf("[ddl] total added index for %d rows, this batch add index for %d failed, take time %v",
				addedCount, batchAddedCount, sub)
			return errors.Trace(err)
		}
		job.SetRowCount(addedCount)
		batchHandleDataHistogram.WithLabelValues(batchAddIdx).Observe(sub)
		log.Infof("[ddl] total added index for %d rows, this batch added index for %d rows, take time %v",
			addedCount, batchAddedCount, sub)

		if retCnt < batches {
			return nil
		}
	}
}

type handleInfo struct {
	startHandle int64
	endHandle   int64
	once        sync.Once
}

func getCountAndHandle(batchOpInfo *indexBatchOpInfo) (int64, int64, error) {
	l := len(batchOpInfo.batchRetCh)
	batchRets := make([]*batchRet, 0, l)
	for i := 0; i < l; i++ {
		batchRet := <-batchOpInfo.batchRetCh
		batchRets = append(batchRets, batchRet)
	}
	sort.Sort(batchRetSlice(batchRets))

	batchAddedCount, currHandle := int64(0), int64(0)
	var err error
	for _, ret := range batchRets {
		if ret.err != nil {
			err = ret.err
			break
		}
		batchAddedCount += int64(ret.count)
		currHandle = ret.doneHandle
	}
	return batchAddedCount, currHandle, errors.Trace(err)
}

func (d *ddl) backfillIndex(t table.Table, batchOpInfo *indexBatchOpInfo, seekHandle int64, wg *sync.WaitGroup) {
	defer wg.Done()

	ret := new(batchRet)
	handleInfo := &handleInfo{startHandle: seekHandle}
	err := kv.RunInNewTxn(d.store, true, func(txn kv.Transaction) error {
		err1 := d.isReorgRunnable(txn, ddlJobFlag)
		if err1 != nil {
			return errors.Trace(err1)
		}
		ret = d.backfillIndexInTxn(t, txn, batchOpInfo, handleInfo)
		if ret.err != nil {
			return errors.Trace(ret.err)
		}
		return nil
	})
	if err != nil {
		ret.err = errors.Trace(err)
	}

	// It's failed to fetch row keys.
	if ret.count == 0 && ret.err != nil {
		batchOpInfo.nextCh <- seekHandle
	}

	batchOpInfo.batchRetCh <- ret
}

// backfillIndexInTxn deals with a part of backfilling index data in a Transaction.
// This part of the index data rows is defaultSmallBatchCnt.
func (d *ddl) backfillIndexInTxn(t table.Table, txn kv.Transaction, batchOpInfo *indexBatchOpInfo,
	handleInfo *handleInfo) *batchRet {
	idxRecords, batchRet := d.fetchRowColVals(txn, t, batchOpInfo, handleInfo)
	if batchRet.err != nil {
		batchRet.err = errors.Trace(batchRet.err)
		return batchRet
	}

	for _, idxRecord := range idxRecords {
		log.Debug("[ddl] backfill index...", idxRecord.handle)
		err := txn.LockKeys(idxRecord.key)
		if err != nil {
			batchRet.err = errors.Trace(err)
			return batchRet
		}

		// Create the index.
		handle, err := batchOpInfo.tblIndex.Create(txn, idxRecord.vals, idxRecord.handle)
		if err != nil {
			if terror.ErrorEqual(err, kv.ErrKeyExists) && idxRecord.handle == handle {
				// Index already exists, skip it.
				continue
			}
			batchRet.err = errors.Trace(err)
			return batchRet
		}
	}
	return batchRet
}

func (d *ddl) dropTableIndex(indexInfo *model.IndexInfo, job *model.Job) error {
	startKey := tablecodec.EncodeTableIndexPrefix(job.TableID, indexInfo.ID)
	// It's asynchronous so it doesn't need to consider if it completes.
	deleteAll := -1
	_, _, err := d.delKeysWithStartKey(startKey, startKey, ddlJobFlag, job, deleteAll)
	return errors.Trace(err)
}

func findIndexByName(idxName string, indices []*model.IndexInfo) *model.IndexInfo {
	for _, idx := range indices {
		if idx.Name.L == idxName {
			return idx
		}
	}
	return nil
}

func allocateIndexID(tblInfo *model.TableInfo) int64 {
	tblInfo.MaxIndexID++
	return tblInfo.MaxIndexID
}

// recordIterFunc is used for low-level record iteration.
type recordIterFunc func(h int64, rowKey kv.Key, rawRecord []byte) (more bool, err error)

func (d *ddl) iterateSnapshotRows(t table.Table, version uint64, seekHandle int64, fn recordIterFunc) error {
	ver := kv.Version{Ver: version}
	snap, err := d.store.GetSnapshot(ver)
	if err != nil {
		return errors.Trace(err)
	}

	firstKey := t.RecordKey(seekHandle)
	it, err := snap.Seek(firstKey)
	if err != nil {
		return errors.Trace(err)
	}
	defer it.Close()

	for it.Valid() {
		if !it.Key().HasPrefix(t.RecordPrefix()) {
			break
		}

		var handle int64
		handle, err = tablecodec.DecodeRowKey(it.Key())
		if err != nil {
			return errors.Trace(err)
		}
		rk := t.RecordKey(handle)

		more, err := fn(handle, rk, it.Value())
		if !more || err != nil {
			return errors.Trace(err)
		}

		err = kv.NextUntil(it, util.RowKeyPrefixFilter(rk))
		if terror.ErrorEqual(err, kv.ErrNotExist) {
			break
		} else if err != nil {
			return errors.Trace(err)
		}
	}

	return nil
}
