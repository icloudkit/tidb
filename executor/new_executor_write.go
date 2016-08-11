// Copyright 2016 PingCAP, Inc.
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

package executor

import (
	"github.com/juju/errors"
	"github.com/pingcap/tidb/ast"
	"github.com/pingcap/tidb/context"
	"github.com/pingcap/tidb/expression"
	"github.com/pingcap/tidb/mysql"
	"github.com/pingcap/tidb/sessionctx/variable"
	"github.com/pingcap/tidb/table"
	"github.com/pingcap/tidb/util/types"
)

// NewUpdateExec represents a new update executor.
type NewUpdateExec struct {
	SelectExec  Executor
	OrderedList []*expression.Assignment

	// Map for unique (Table, handle) pair.
	updatedRowKeys map[table.Table]map[int64]struct{}
	ctx            context.Context

	rows        []*Row          // The rows fetched from TableExec.
	newRowsData [][]types.Datum // The new values to be set.
	fetched     bool
	cursor      int
}

// Schema implements Executor Schema interface.
func (e *NewUpdateExec) Schema() expression.Schema {
	return nil
}

// Next implements Executor Next interface.
func (e *NewUpdateExec) Next() (*Row, error) {
	if !e.fetched {
		err := e.fetchRows()
		if err != nil {
			return nil, errors.Trace(err)
		}
		e.fetched = true
	}

	columns, err := getNewUpdateColumns(e.OrderedList)
	if err != nil {
		return nil, errors.Trace(err)
	}
	if e.cursor >= len(e.rows) {
		return nil, nil
	}
	if e.updatedRowKeys == nil {
		e.updatedRowKeys = make(map[table.Table]map[int64]struct{})
	}
	row := e.rows[e.cursor]
	newData := e.newRowsData[e.cursor]
	for _, entry := range row.RowKeys {
		if entry == nil { // outer join
			continue
		}
		tbl := entry.Tbl
		if e.updatedRowKeys[tbl] == nil {
			e.updatedRowKeys[tbl] = make(map[int64]struct{})
		}
		offset := e.getTableOffset(*entry)
		handle := entry.Handle
		oldData := row.Data[offset : offset+len(tbl.WritableCols())]
		newTableData := newData[offset : offset+len(tbl.WritableCols())]
		_, ok := e.updatedRowKeys[tbl][handle]
		if ok {
			// Each matching row is updated once, even if it matches the conditions multiple times.
			continue
		}
		// Update row
		err1 := newUpdateRecord(e.ctx, handle, oldData, newTableData, columns, tbl, offset, false)
		if err1 != nil {
			return nil, errors.Trace(err1)
		}
		e.updatedRowKeys[tbl][handle] = struct{}{}
	}
	e.cursor++
	return &Row{}, nil
}

func getNewUpdateColumns(assignList []*expression.Assignment) (map[int]*expression.Assignment, error) {
	m := make(map[int]*expression.Assignment, len(assignList))
	for i, v := range assignList {
		m[i] = v
	}
	return m, nil
}

func (e *NewUpdateExec) fetchRows() error {
	for {
		row, err := e.SelectExec.Next()
		if err != nil {
			return errors.Trace(err)
		}
		if row == nil {
			return nil
		}
		data := make([]types.Datum, len(e.SelectExec.Schema()))
		newData := make([]types.Datum, len(e.SelectExec.Schema()))
		for i, s := range e.SelectExec.Schema() {
			data[i], err = s.Eval(row.Data, e.ctx)
			if err != nil {
				return errors.Trace(err)
			}
			newData[i] = data[i]
			if e.OrderedList[i] != nil {
				val, err := e.OrderedList[i].Expr.Eval(row.Data, e.ctx)
				if err != nil {
					return errors.Trace(err)
				}
				newData[i] = val
			}
		}
		row.Data = data
		e.rows = append(e.rows, row)
		e.newRowsData = append(e.newRowsData, newData)
	}
}

func (e *NewUpdateExec) getTableOffset(entry RowKeyEntry) int {
	t := entry.Tbl
	var tblName string
	if entry.TableAsName == nil || len(entry.TableAsName.L) == 0 {
		tblName = t.Meta().Name.L
	} else {
		tblName = entry.TableAsName.L
	}
	schema := e.SelectExec.Schema()
	for i := 0; i < len(schema); i++ {
		s := schema[i]
		if s.TblName.L == tblName {
			return i
		}
	}
	return 0
}

func newUpdateRecord(ctx context.Context, h int64, oldData, newData []types.Datum, updateColumns map[int]*expression.Assignment, t table.Table, offset int, onDuplicateUpdate bool) error {
	cols := t.Cols()
	touched := make(map[int]bool, len(cols))

	assignExists := false
	var newHandle types.Datum
	for i, asgn := range updateColumns {
		if asgn == nil {
			continue
		}
		if i < offset || i >= offset+len(cols) {
			// The assign expression is for another table, not this.
			continue
		}

		colIndex := i - offset
		col := cols[colIndex]
		if col.IsPKHandleColumn(t.Meta()) {
			newHandle = newData[i]
		}
		if mysql.HasAutoIncrementFlag(col.Flag) {
			if newData[i].IsNull() {
				return errors.Errorf("Column '%v' cannot be null", col.Name.O)
			}
			val, err := newData[i].ToInt64()
			if err != nil {
				return errors.Trace(err)
			}
			t.RebaseAutoID(val, true)
		}

		touched[colIndex] = true
		assignExists = true
	}

	// If no assign list for this table, no need to update.
	if !assignExists {
		return nil
	}

	// Check whether new value is valid.
	if err := table.CastValues(ctx, newData, cols); err != nil {
		return errors.Trace(err)
	}

	if err := table.CheckNotNull(cols, newData); err != nil {
		return errors.Trace(err)
	}

	// If row is not changed, we should do nothing.
	rowChanged := false
	for i := range oldData {
		if !touched[i] {
			continue
		}

		n, err := newData[i].CompareDatum(oldData[i])
		if err != nil {
			return errors.Trace(err)
		}
		if n != 0 {
			rowChanged = true
			break
		}
	}
	if !rowChanged {
		// See https://dev.mysql.com/doc/refman/5.7/en/mysql-real-connect.html  CLIENT_FOUND_ROWS
		if variable.GetSessionVars(ctx).ClientCapability&mysql.ClientFoundRows > 0 {
			variable.GetSessionVars(ctx).AddAffectedRows(1)
		}
		return nil
	}

	var err error
	if !newHandle.IsNull() {
		err = t.RemoveRecord(ctx, h, oldData)
		if err != nil {
			return errors.Trace(err)
		}
		_, err = t.AddRecord(ctx, newData)
	} else {
		// Update record to new value and update index.
		err = t.UpdateRecord(ctx, h, oldData, newData, touched)
	}
	if err != nil {
		return errors.Trace(err)
	}
	dirtyDB := getDirtyDB(ctx)
	tid := t.Meta().ID
	dirtyDB.deleteRow(tid, h)
	dirtyDB.addRow(tid, h, newData)

	// Record affected rows.
	if !onDuplicateUpdate {
		variable.GetSessionVars(ctx).AddAffectedRows(1)
	} else {
		variable.GetSessionVars(ctx).AddAffectedRows(2)
	}
	return nil
}

// Fields implements Executor Fields interface.
// Returns nil to indicate there is no output.
func (e *NewUpdateExec) Fields() []*ast.ResultField {
	return nil
}

// Close implements Executor Close interface.
func (e *NewUpdateExec) Close() error {
	return e.SelectExec.Close()
}
