// Copyright 2017 PingCAP, Inc.
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

package generatedexpr

import (
	"fmt"

	"github.com/pingcap/errors"
	"github.com/pingcap/tidb/pkg/meta/model"
	"github.com/pingcap/tidb/pkg/parser"
	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/pingcap/tidb/pkg/parser/charset"
	"github.com/pingcap/tidb/pkg/util"
	parserutil "github.com/pingcap/tidb/pkg/util/parser"
)

// nameResolver is the visitor to resolve table name and column name.
// it combines TableInfo and ColumnInfo to a generation expression.
type nameResolver struct {
	tableInfo *model.TableInfo
	err       error
}

// Enter implements ast.Visitor interface.
func (*nameResolver) Enter(inNode ast.Node) (ast.Node, bool) {
	return inNode, false
}

// Leave implements ast.Visitor interface.
func (nr *nameResolver) Leave(inNode ast.Node) (node ast.Node, ok bool) {
	//nolint: revive,all_revive
	switch v := inNode.(type) {
	case *ast.ColumnNameExpr:
		for _, col := range nr.tableInfo.Columns {
			if col.Name.L == v.Name.Name.L {
				return inNode, true
			}
		}
		nr.err = errors.Errorf("can't find column %s in %s", v.Name.Name.O, nr.tableInfo.Name.O)
		return inNode, false
	}
	return inNode, true
}

// ParseExpression parses an ExprNode from a string.
// When TiDB loads infoschema from TiKV, `GeneratedExprString`
// of `ColumnInfo` is a string field, so we need to parse
// it into ast.ExprNode. This function is for that.
func ParseExpression(expr string) (node ast.ExprNode, err error) {
	expr = fmt.Sprintf("select %s", expr)
	charset, collation := charset.GetDefaultCharsetAndCollate()
	parse := parserutil.GetParser()
	defer func() {
		parserutil.DestroyParser(parse)
	}()
	stmts, _, err := parse.ParseSQL(expr,
		parser.CharsetConnection(charset),
		parser.CollationConnection(collation))
	if err == nil {
		node = stmts[0].(*ast.SelectStmt).Fields.Fields[0].Expr
	}
	return node, util.SyntaxError(err)
}

// SimpleResolveName resolves all column names in the expression node.
func SimpleResolveName(node ast.ExprNode, tblInfo *model.TableInfo) (ast.ExprNode, error) {
	nr := nameResolver{tblInfo, nil}
	if _, ok := node.Accept(&nr); !ok {
		return nil, errors.Trace(nr.err)
	}
	return node, nil
}
