// Copyright 2019 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package importer

import (
	"context"
	"io"
	"strings"

	"github.com/pingcap/errors"
	"github.com/pingcap/tidb/pkg/expression"
	"github.com/pingcap/tidb/pkg/lightning/backend/encode"
	"github.com/pingcap/tidb/pkg/lightning/backend/kv"
	"github.com/pingcap/tidb/pkg/lightning/common"
	"github.com/pingcap/tidb/pkg/meta/autoid"
	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/pingcap/tidb/pkg/parser/mysql" //nolint: goimports
	"github.com/pingcap/tidb/pkg/table"
	"github.com/pingcap/tidb/pkg/types"
	"github.com/pingcap/tidb/pkg/util/chunk"
)

// KVEncoder encodes a row of data into a KV pair.
type KVEncoder interface {
	Encode(row []types.Datum, rowID int64) (*kv.Pairs, error)
	io.Closer
}

// tableKVEncoder encodes a row of data into a KV pair.
type tableKVEncoder struct {
	*kv.BaseKVEncoder
	// see import.go
	columnAssignments  []expression.Expression
	columnsAndUserVars []*ast.ColumnNameOrUserVar
	fieldMappings      []*FieldMapping
	insertColumns      []*table.Column
	// Following cache use to avoid `runtime.makeslice`.
	insertColumnRowCache []types.Datum
	rowCache             []types.Datum
	hasValueCache        []bool
}

var _ KVEncoder = &tableKVEncoder{}

// NewTableKVEncoder creates a new tableKVEncoder.
// exported for test.
func NewTableKVEncoder(
	config *encode.EncodingConfig,
	ti *TableImporter,
) (KVEncoder, error) {
	baseKVEncoder, err := kv.NewBaseKVEncoder(config)
	if err != nil {
		return nil, err
	}
	colAssignExprs, _, err := ti.CreateColAssignSimpleExprs(baseKVEncoder.SessionCtx.GetExprCtx())
	if err != nil {
		return nil, err
	}

	return &tableKVEncoder{
		BaseKVEncoder:      baseKVEncoder,
		columnAssignments:  colAssignExprs,
		columnsAndUserVars: ti.ColumnsAndUserVars,
		fieldMappings:      ti.FieldMappings,
		insertColumns:      ti.InsertColumns,
	}, nil
}

// Encode implements the KVEncoder interface.
func (en *tableKVEncoder) Encode(row []types.Datum, rowID int64) (*kv.Pairs, error) {
	// we ignore warnings when encoding rows now, but warnings uses the same memory as parser, since the input
	// row []types.Datum share the same underlying buf, and when doing CastValue, we're using hack.String/hack.Slice.
	// when generating error such as mysql.ErrDataOutOfRange, the data will be part of the error, causing the buf
	// unable to release. So we truncate the warnings here.
	defer en.TruncateWarns()
	record, err := en.parserData2TableData(row, rowID)
	if err != nil {
		return nil, err
	}

	return en.Record2KV(record, row, rowID)
}

// todo merge with code in load_data.go
func (en *tableKVEncoder) parserData2TableData(parserData []types.Datum, rowID int64) ([]types.Datum, error) {
	if cap(en.insertColumnRowCache) < len(en.insertColumns) {
		en.insertColumnRowCache = make([]types.Datum, 0, len(en.insertColumns))
	}
	row := en.insertColumnRowCache[:0]
	setVar := func(name string, col *types.Datum) {
		// User variable names are not case-sensitive
		// https://dev.mysql.com/doc/refman/8.0/en/user-variables.html
		name = strings.ToLower(name)
		if col == nil || col.IsNull() {
			en.SessionCtx.UnsetUserVar(name)
		} else {
			en.SessionCtx.SetUserVarVal(name, *col)
		}
	}

	for i := range en.fieldMappings {
		if i >= len(parserData) {
			if en.fieldMappings[i].Column == nil {
				setVar(en.fieldMappings[i].UserVar.Name, nil)
				continue
			}

			// If some columns is missing and their type is time and has not null flag, they should be set as current time.
			if types.IsTypeTime(en.fieldMappings[i].Column.GetType()) && mysql.HasNotNullFlag(en.fieldMappings[i].Column.GetFlag()) {
				row = append(row, types.NewTimeDatum(types.CurrentTime(en.fieldMappings[i].Column.GetType())))
				continue
			}

			row = append(row, types.NewDatum(nil))
			continue
		}

		if en.fieldMappings[i].Column == nil {
			setVar(en.fieldMappings[i].UserVar.Name, &parserData[i])
			continue
		}

		row = append(row, parserData[i])
	}
	for i := range en.columnAssignments {
		// eval expression of `SET` clause
		d, err := en.columnAssignments[i].Eval(en.SessionCtx.GetExprCtx().GetEvalCtx(), chunk.Row{})
		if err != nil {
			return nil, err
		}
		row = append(row, d)
	}

	// a new row buffer will be allocated in getRow
	newRow, err := en.getRow(row, rowID)
	if err != nil {
		return nil, err
	}

	return newRow, nil
}

// getRow gets the row which from `insert into select from` or `load data`.
// The input values from these two statements are datums instead of
// expressions which are used in `insert into set x=y`.
// copied from InsertValues
func (en *tableKVEncoder) getRow(vals []types.Datum, rowID int64) ([]types.Datum, error) {
	rowLen := len(en.Columns)
	if cap(en.rowCache) < rowLen || cap(en.hasValueCache) < rowLen {
		en.rowCache = make([]types.Datum, rowLen)
		en.hasValueCache = make([]bool, rowLen)
	} else {
		en.rowCache = en.rowCache[:0]
		en.hasValueCache = en.hasValueCache[:0]
		for range rowLen {
			en.rowCache = append(en.rowCache, types.Datum{})
			en.hasValueCache = append(en.hasValueCache, false)
		}
	}
	row := en.rowCache
	hasValue := en.hasValueCache
	for i := range en.insertColumns {
		casted, err := table.CastColumnValue(en.SessionCtx.GetExprCtx(), vals[i], en.insertColumns[i].ToInfo(), false, false)
		if err != nil {
			return nil, err
		}

		offset := en.insertColumns[i].Offset
		row[offset] = casted
		hasValue[offset] = true
	}

	return en.fillRow(row, hasValue, rowID)
}

func (en *tableKVEncoder) fillRow(row []types.Datum, hasValue []bool, rowID int64) ([]types.Datum, error) {
	var value types.Datum
	var err error

	record := en.GetOrCreateRecord()
	for i, col := range en.Columns {
		var theDatum *types.Datum
		doCast := true
		if hasValue[i] {
			theDatum = &row[i]
			doCast = false
		}
		value, err = en.ProcessColDatum(col, rowID, theDatum, doCast)
		if err != nil {
			return nil, en.LogKVConvertFailed(row, i, col.ToInfo(), err)
		}

		record = append(record, value)
	}

	if common.TableHasAutoRowID(en.TableMeta()) {
		rowValue := rowID
		newRowID := en.AutoIDFn(rowID)
		value = types.NewIntDatum(newRowID)
		record = append(record, value)
		alloc := en.TableAllocators().Get(autoid.RowIDAllocType)
		if err := alloc.Rebase(context.Background(), rowValue, false); err != nil {
			return nil, errors.Trace(err)
		}
	}

	if len(en.GenCols) > 0 {
		if errCol, err := en.EvalGeneratedColumns(record, en.Columns); err != nil {
			return nil, en.LogEvalGenExprFailed(row, errCol, err)
		}
	}

	return record, nil
}

func (en *tableKVEncoder) Close() error {
	en.SessionCtx.Close()
	return nil
}
