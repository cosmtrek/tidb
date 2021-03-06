// Copyright 2018 PingCAP, Inc.
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

package core

import (
	"bytes"
	"fmt"

	"github.com/pingcap/errors"
	"github.com/pingcap/parser/ast"
	"github.com/pingcap/parser/model"
	"github.com/pingcap/parser/mysql"
	"github.com/pingcap/parser/opcode"
	"github.com/pingcap/parser/terror"
	"github.com/pingcap/tidb/expression"
	"github.com/pingcap/tidb/planner/property"
	"github.com/pingcap/tidb/privilege"
	"github.com/pingcap/tidb/sessionctx"
	"github.com/pingcap/tidb/sessionctx/stmtctx"
	"github.com/pingcap/tidb/types"
	"github.com/pingcap/tidb/types/parser_driver"
	"github.com/pingcap/tipb/go-tipb"
)

// PointGetPlan is a fast plan for simple point get.
// When we detect that the statement has a unique equal access condition, this plan is used.
// This plan is much faster to build and to execute because it avoid the optimization and coprocessor cost.
type PointGetPlan struct {
	basePlan
	schema           *expression.Schema
	DBName           model.CIStr
	TblInfo          *model.TableInfo
	IndexInfo        *model.IndexInfo
	Handle           int64
	HandleParam      *driver.ParamMarkerExpr
	IndexValues      []types.Datum
	IndexValueParams []*driver.ParamMarkerExpr
	expr             expression.Expression
	ctx              sessionctx.Context
	UnsignedHandle   bool
	IsTableDual      bool
	Lock             bool
	IsForUpdate      bool
}

type nameValuePair struct {
	colName string
	value   types.Datum
	param   *driver.ParamMarkerExpr
}

// Schema implements the Plan interface.
func (p *PointGetPlan) Schema() *expression.Schema {
	return p.schema
}

// attach2Task makes the current physical plan as the father of task's physicalPlan and updates the cost of
// current task. If the child's task is cop task, some operator may close this task and return a new rootTask.
func (p *PointGetPlan) attach2Task(...task) task {
	return nil
}

// ToPB converts physical plan to tipb executor.
func (p *PointGetPlan) ToPB(ctx sessionctx.Context) (*tipb.Executor, error) {
	return nil, nil
}

// ExplainInfo returns operator information to be explained.
func (p *PointGetPlan) ExplainInfo() string {
	buffer := bytes.NewBufferString("")
	tblName := p.TblInfo.Name.O
	fmt.Fprintf(buffer, "table:%s", tblName)
	if p.IndexInfo != nil {
		fmt.Fprintf(buffer, ", index:")
		for i, col := range p.IndexInfo.Columns {
			buffer.WriteString(col.Name.O)
			if i < len(p.IndexInfo.Columns)-1 {
				buffer.WriteString(" ")
			}
		}
	} else {
		if p.UnsignedHandle {
			fmt.Fprintf(buffer, ", handle:%d", uint64(p.Handle))
		} else {
			fmt.Fprintf(buffer, ", handle:%d", p.Handle)
		}
	}
	if p.Lock {
		fmt.Fprintf(buffer, ", lock")
	}
	return buffer.String()
}

// GetChildReqProps gets the required property by child index.
func (p *PointGetPlan) GetChildReqProps(idx int) *property.PhysicalProperty {
	return nil
}

// StatsCount will return the the RowCount of property.StatsInfo for this plan.
func (p *PointGetPlan) StatsCount() float64 {
	return 1
}

// statsInfo will return the the RowCount of property.StatsInfo for this plan.
func (p *PointGetPlan) statsInfo() *property.StatsInfo {
	if p.stats == nil {
		p.stats = &property.StatsInfo{}
	}
	p.stats.RowCount = 1
	return p.stats
}

// Children gets all the children.
func (p *PointGetPlan) Children() []PhysicalPlan {
	return nil
}

// SetChildren sets the children for the plan.
func (p *PointGetPlan) SetChildren(...PhysicalPlan) {}

// SetChild sets a specific child for the plan.
func (p *PointGetPlan) SetChild(i int, child PhysicalPlan) {}

// ResolveIndices resolves the indices for columns. After doing this, the columns can evaluate the rows by their indices.
func (p *PointGetPlan) ResolveIndices() error {
	return nil
}

// TryFastPlan tries to use the PointGetPlan for the query.
func TryFastPlan(ctx sessionctx.Context, node ast.Node) Plan {
	switch x := node.(type) {
	case *ast.SelectStmt:
		// Try to convert the `SELECT a, b, c FROM t WHERE (a, b, c) in ((1, 2, 4), (1, 3, 5))` to
		// `PhysicalUnionAll` which children are `PointGet` if exists an unique key (a, b, c) in table `t`
		if fp := tryWhereIn2BatchPointGet(ctx, x); fp != nil {
			return fp
		}
		fp := tryPointGetPlan(ctx, x)
		if fp != nil {
			if checkFastPlanPrivilege(ctx, fp, mysql.SelectPriv) != nil {
				return nil
			}
			if fp.IsTableDual {
				tableDual := PhysicalTableDual{}
				tableDual.SetSchema(fp.Schema())
				return tableDual.Init(ctx, &property.StatsInfo{})
			}
			if x.LockTp == ast.SelectLockForUpdate {
				// Locking of rows for update using SELECT FOR UPDATE only applies when autocommit
				// is disabled (either by beginning transaction with START TRANSACTION or by setting
				// autocommit to 0. If autocommit is enabled, the rows matching the specification are not locked.
				// See https://dev.mysql.com/doc/refman/5.7/en/innodb-locking-reads.html
				sessVars := ctx.GetSessionVars()
				if !sessVars.IsAutocommit() || sessVars.InTxn() {
					fp.Lock = true
					fp.IsForUpdate = true
				}
			}
			return fp
		}
	case *ast.UpdateStmt:
		return tryUpdatePointPlan(ctx, x)
	case *ast.DeleteStmt:
		return tryDeletePointPlan(ctx, x)
	}
	return nil
}

func tryWhereIn2BatchPointGet(ctx sessionctx.Context, selStmt *ast.SelectStmt) Plan {
	if selStmt.OrderBy != nil || selStmt.GroupBy != nil || selStmt.Limit != nil ||
		selStmt.Having != nil || len(selStmt.WindowSpecs) > 0 ||
		selStmt.LockTp != ast.SelectLockNone {
		return nil
	}
	in, ok := selStmt.Where.(*ast.PatternInExpr)
	if !ok || in.Not || len(in.List) < 1 {
		return nil
	}

	children := make([]PhysicalPlan, 0, len(in.List))
	chReqProps := make([]*property.PhysicalProperty, 0, len(in.List))
	reusedStmt := &ast.SelectStmt{
		SelectStmtOpts: selStmt.SelectStmtOpts,
		Distinct:       selStmt.Distinct,
		From:           selStmt.From,
		Fields:         selStmt.Fields,
	}

	switch leftExpr := in.Expr.(type) {
	case *ast.ColumnNameExpr:
		reusedStmt := &ast.SelectStmt{
			SelectStmtOpts: selStmt.SelectStmtOpts,
			Distinct:       selStmt.Distinct,
			From:           selStmt.From,
			Fields:         selStmt.Fields,
		}
		for _, row := range in.List {
			where := &ast.BinaryOperationExpr{
				Op: opcode.EQ,
				L:  in.Expr,
				R:  row,
			}
			reusedStmt.Where = where
			fp := TryFastPlan(ctx, reusedStmt)
			if fp == nil {
				return nil
			}
			chReqProps = append(chReqProps, &property.PhysicalProperty{ExpectedCnt: 1})
			children = append(children, fp.(*PointGetPlan))
		}

	case *ast.RowExpr:
		if len(leftExpr.Values) < 1 {
			return nil
		}

		eleCount := len(leftExpr.Values)
		for _, row := range in.List {
			rightExpr, ok := row.(*ast.RowExpr)
			if !ok || len(rightExpr.Values) != eleCount {
				return nil
			}
			where := &ast.BinaryOperationExpr{
				Op: opcode.EQ,
				L:  leftExpr.Values[0],
				R:  rightExpr.Values[0],
			}
			for i := 1; i < eleCount; i++ {
				right := &ast.BinaryOperationExpr{
					Op: opcode.EQ,
					L:  leftExpr.Values[i],
					R:  rightExpr.Values[i],
				}
				where = &ast.BinaryOperationExpr{
					Op: opcode.LogicAnd,
					L:  where,
					R:  right,
				}
			}
			reusedStmt.Where = where
			fp := TryFastPlan(ctx, reusedStmt)
			if fp == nil {
				return nil
			}
			chReqProps = append(chReqProps, &property.PhysicalProperty{ExpectedCnt: 1})
			children = append(children, fp.(*PointGetPlan))
		}

	default:
		return nil
	}

	ua := PhysicalUnionAll{
		IsPointGetUnion: true,
	}.Init(ctx, children[0].statsInfo().Scale(float64(len(children))), chReqProps...)
	ua.SetSchema(children[0].Schema())
	ua.SetChildren(children...)
	return ua
}

// tryPointGetPlan determine if the SelectStmt can use a PointGetPlan.
// Returns nil if not applicable.
// To use the PointGetPlan the following rules must be satisfied:
// 1. For the limit clause, the count should at least 1 and the offset is 0.
// 2. It must be a single table select.
// 3. All the columns must be public and generated.
// 4. The condition is an access path that the range is a unique key.
func tryPointGetPlan(ctx sessionctx.Context, selStmt *ast.SelectStmt) *PointGetPlan {
	if selStmt.Having != nil {
		return nil
	} else if selStmt.Limit != nil {
		count, offset, err := extractLimitCountOffset(ctx, selStmt.Limit)
		if err != nil || count == 0 || offset > 0 {
			return nil
		}
	}
	tblName, tblAlias := getSingleTableNameAndAlias(selStmt.From)
	if tblName == nil {
		return nil
	}
	tbl := tblName.TableInfo
	if tbl == nil {
		return nil
	}
	dbName := tblName.Schema
	if dbName.L == "" {
		dbName = model.NewCIStr(ctx.GetSessionVars().CurrentDB)
	}
	// Do not handle partitioned table.
	// Table partition implementation translates LogicalPlan from `DataSource` to
	// `Union -> DataSource` in the logical plan optimization pass, since PointGetPlan
	// bypass the logical plan optimization, it can't support partitioned table.
	if tbl.GetPartitionInfo() != nil {
		return nil
	}
	for _, col := range tbl.Columns {
		// Do not handle generated columns.
		if col.IsGenerated() {
			return nil
		}
		// Only handle tables that all columns are public.
		if col.State != model.StatePublic {
			return nil
		}
	}
	pairs := make([]nameValuePair, 0, 4)
	pairs = getNameValuePairs(pairs, tblAlias, selStmt.Where)
	if pairs == nil {
		return nil
	}
	handlePair, fieldType := findPKHandle(tbl, pairs)
	if handlePair.value.Kind() != types.KindNull && len(pairs) == 1 {
		schema := buildSchemaFromFields(ctx, tblName.Schema, tbl, tblAlias, selStmt.Fields.Fields)
		if schema == nil {
			return nil
		}
		p := newPointGetPlan(ctx, schema, dbName, tbl)
		intDatum, err := handlePair.value.ConvertTo(ctx.GetSessionVars().StmtCtx, fieldType)
		if err != nil {
			if terror.ErrorEqual(types.ErrOverflow, err) {
				p.IsTableDual = true
				return p
			}
			// some scenarios cast to int with error, but we may use this value in point get
			if !terror.ErrorEqual(types.ErrTruncatedWrongVal, err) {
				return nil
			}
		}
		cmp, err := intDatum.CompareDatum(ctx.GetSessionVars().StmtCtx, &handlePair.value)
		if err != nil {
			return nil
		} else if cmp != 0 {
			p.IsTableDual = true
			return p
		}
		p.Handle = intDatum.GetInt64()
		p.UnsignedHandle = mysql.HasUnsignedFlag(fieldType.Flag)
		p.HandleParam = handlePair.param
		return p
	}

	for _, idxInfo := range tbl.Indices {
		if !idxInfo.Unique {
			continue
		}
		if idxInfo.State != model.StatePublic {
			continue
		}
		idxValues, idxValueParams := getIndexValues(idxInfo, pairs)
		if idxValues == nil {
			continue
		}
		schema := buildSchemaFromFields(ctx, tblName.Schema, tbl, tblAlias, selStmt.Fields.Fields)
		if schema == nil {
			return nil
		}
		p := newPointGetPlan(ctx, schema, dbName, tbl)
		p.IndexInfo = idxInfo
		p.IndexValues = idxValues
		p.IndexValueParams = idxValueParams
		return p
	}
	return nil
}

func newPointGetPlan(ctx sessionctx.Context, schema *expression.Schema, dbName model.CIStr, tbl *model.TableInfo) *PointGetPlan {
	p := &PointGetPlan{
		basePlan: newBasePlan(ctx, "Point_Get"),
		schema:   schema,
		DBName:   dbName,
		TblInfo:  tbl,
	}
	ctx.GetSessionVars().StmtCtx.Tables = []stmtctx.TableEntry{{DB: ctx.GetSessionVars().CurrentDB, Table: tbl.Name.L}}
	return p
}

func checkFastPlanPrivilege(ctx sessionctx.Context, fastPlan *PointGetPlan, checkTypes ...mysql.PrivilegeType) error {
	pm := privilege.GetPrivilegeManager(ctx)
	if pm == nil {
		return nil
	}
	dbName := ctx.GetSessionVars().CurrentDB
	for _, checkType := range checkTypes {
		if !pm.RequestVerification(ctx.GetSessionVars().ActiveRoles, dbName, fastPlan.TblInfo.Name.L, "", checkType) {
			return errors.New("privilege check fail")
		}
	}
	return nil
}

func buildSchemaFromFields(ctx sessionctx.Context, dbName model.CIStr, tbl *model.TableInfo, tblName model.CIStr, fields []*ast.SelectField) *expression.Schema {
	if dbName.L == "" {
		dbName = model.NewCIStr(ctx.GetSessionVars().CurrentDB)
	}
	columns := make([]*expression.Column, 0, len(tbl.Columns)+1)
	if len(fields) > 0 {
		for _, field := range fields {
			if field.WildCard != nil {
				if field.WildCard.Table.L != "" && field.WildCard.Table.L != tblName.L {
					return nil
				}
				for _, col := range tbl.Columns {
					columns = append(columns, colInfoToColumn(dbName, tbl.Name, tblName, col.Name, col, len(columns)))
				}
				continue
			}
			colNameExpr, ok := field.Expr.(*ast.ColumnNameExpr)
			if !ok {
				return nil
			}
			if colNameExpr.Name.Table.L != "" && colNameExpr.Name.Table.L != tblName.L {
				return nil
			}
			col := findCol(tbl, colNameExpr.Name)
			if col == nil {
				return nil
			}
			asName := col.Name
			if field.AsName.L != "" {
				asName = field.AsName
			}
			columns = append(columns, colInfoToColumn(dbName, tbl.Name, tblName, asName, col, len(columns)))
		}
		return expression.NewSchema(columns...)
	}
	// fields len is 0 for update and delete.
	for _, col := range tbl.Columns {
		column := colInfoToColumn(dbName, tbl.Name, tblName, col.Name, col, len(columns))
		columns = append(columns, column)
	}
	schema := expression.NewSchema(columns...)
	return schema
}

// getSingleTableNameAndAlias return the ast node of queried table name and the alias string.
// `tblName` is `nil` if there are multiple tables in the query.
// `tblAlias` will be the real table name if there is no table alias in the query.
func getSingleTableNameAndAlias(tableRefs *ast.TableRefsClause) (tblName *ast.TableName, tblAlias model.CIStr) {
	if tableRefs == nil || tableRefs.TableRefs == nil || tableRefs.TableRefs.Right != nil {
		return nil, tblAlias
	}
	tblSrc, ok := tableRefs.TableRefs.Left.(*ast.TableSource)
	if !ok {
		return nil, tblAlias
	}
	tblName, ok = tblSrc.Source.(*ast.TableName)
	if !ok {
		return nil, tblAlias
	}
	tblAlias = tblSrc.AsName
	if tblSrc.AsName.L == "" {
		tblAlias = tblName.Name
	}
	return tblName, tblAlias
}

// getNameValuePairs extracts `column = constant/paramMarker` conditions from expr as name value pairs.
func getNameValuePairs(nvPairs []nameValuePair, tblName model.CIStr, expr ast.ExprNode) []nameValuePair {
	binOp, ok := expr.(*ast.BinaryOperationExpr)
	if !ok {
		return nil
	}
	if binOp.Op == opcode.LogicAnd {
		nvPairs = getNameValuePairs(nvPairs, tblName, binOp.L)
		if nvPairs == nil {
			return nil
		}
		nvPairs = getNameValuePairs(nvPairs, tblName, binOp.R)
		if nvPairs == nil {
			return nil
		}
		return nvPairs
	} else if binOp.Op == opcode.EQ {
		var d types.Datum
		var colName *ast.ColumnNameExpr
		var param *driver.ParamMarkerExpr
		var ok bool
		if colName, ok = binOp.L.(*ast.ColumnNameExpr); ok {
			switch x := binOp.R.(type) {
			case *driver.ValueExpr:
				d = x.Datum
			case *driver.ParamMarkerExpr:
				d = x.Datum
				param = x
			}
		} else if colName, ok = binOp.R.(*ast.ColumnNameExpr); ok {
			switch x := binOp.L.(type) {
			case *driver.ValueExpr:
				d = x.Datum
			case *driver.ParamMarkerExpr:
				d = x.Datum
				param = x
			}
		} else {
			return nil
		}
		if d.IsNull() {
			return nil
		}
		if colName.Name.Table.L != "" && colName.Name.Table.L != tblName.L {
			return nil
		}
		return append(nvPairs, nameValuePair{colName: colName.Name.Name.L, value: d, param: param})
	}
	return nil
}

func findPKHandle(tblInfo *model.TableInfo, pairs []nameValuePair) (handlePair nameValuePair, fieldType *types.FieldType) {
	if !tblInfo.PKIsHandle {
		return handlePair, nil
	}
	for _, col := range tblInfo.Columns {
		if mysql.HasPriKeyFlag(col.Flag) {
			i := findInPairs(col.Name.L, pairs)
			if i == -1 {
				return handlePair, nil
			}
			return pairs[i], &col.FieldType
		}
	}
	return handlePair, nil
}

func getIndexValues(idxInfo *model.IndexInfo, pairs []nameValuePair) ([]types.Datum, []*driver.ParamMarkerExpr) {
	idxValues := make([]types.Datum, 0, 4)
	idxValueParams := make([]*driver.ParamMarkerExpr, 0, 4)
	if len(idxInfo.Columns) != len(pairs) {
		return nil, nil
	}
	if idxInfo.HasPrefixIndex() {
		return nil, nil
	}
	for _, idxCol := range idxInfo.Columns {
		i := findInPairs(idxCol.Name.L, pairs)
		if i == -1 {
			return nil, nil
		}
		idxValues = append(idxValues, pairs[i].value)
		idxValueParams = append(idxValueParams, pairs[i].param)
	}
	if len(idxValues) > 0 {
		return idxValues, idxValueParams
	}
	return nil, nil
}

func findInPairs(colName string, pairs []nameValuePair) int {
	for i, pair := range pairs {
		if pair.colName == colName {
			return i
		}
	}
	return -1
}

func tryUpdatePointPlan(ctx sessionctx.Context, updateStmt *ast.UpdateStmt) Plan {
	selStmt := &ast.SelectStmt{
		Fields:  &ast.FieldList{},
		From:    updateStmt.TableRefs,
		Where:   updateStmt.Where,
		OrderBy: updateStmt.Order,
		Limit:   updateStmt.Limit,
	}
	fastSelect := tryPointGetPlan(ctx, selStmt)
	if fastSelect == nil {
		return nil
	}
	if checkFastPlanPrivilege(ctx, fastSelect, mysql.SelectPriv, mysql.UpdatePriv) != nil {
		return nil
	}
	if fastSelect.IsTableDual {
		return PhysicalTableDual{}.Init(ctx, &property.StatsInfo{})
	}
	if ctx.GetSessionVars().TxnCtx.IsPessimistic {
		fastSelect.Lock = true
	}
	orderedList := buildOrderedList(ctx, fastSelect, updateStmt.List)
	if orderedList == nil {
		return nil
	}
	handleCol := fastSelect.findHandleCol()
	updatePlan := Update{
		SelectPlan:  fastSelect,
		OrderedList: orderedList,
		TblColPosInfos: TblColPosInfoSlice{
			TblColPosInfo{
				TblID:         fastSelect.TblInfo.ID,
				Start:         0,
				End:           fastSelect.schema.Len(),
				HandleOrdinal: handleCol.Index,
			},
		},
	}.Init(ctx)
	return updatePlan
}

func buildOrderedList(ctx sessionctx.Context, fastSelect *PointGetPlan, list []*ast.Assignment) []*expression.Assignment {
	orderedList := make([]*expression.Assignment, 0, len(list))
	for _, assign := range list {
		col, err := fastSelect.schema.FindColumn(assign.Column)
		if err != nil {
			return nil
		}
		if col == nil {
			return nil
		}
		newAssign := &expression.Assignment{
			Col: col,
		}
		expr, err := expression.RewriteSimpleExprWithSchema(ctx, assign.Expr, fastSelect.schema)
		if err != nil {
			return nil
		}
		expr = expression.BuildCastFunction(ctx, expr, col.GetType())
		newAssign.Expr, err = expr.ResolveIndices(fastSelect.schema)
		if err != nil {
			return nil
		}
		orderedList = append(orderedList, newAssign)
	}
	return orderedList
}

func tryDeletePointPlan(ctx sessionctx.Context, delStmt *ast.DeleteStmt) Plan {
	if delStmt.IsMultiTable {
		return nil
	}
	selStmt := &ast.SelectStmt{
		Fields:  &ast.FieldList{},
		From:    delStmt.TableRefs,
		Where:   delStmt.Where,
		OrderBy: delStmt.Order,
		Limit:   delStmt.Limit,
	}
	fastSelect := tryPointGetPlan(ctx, selStmt)
	if fastSelect == nil {
		return nil
	}
	if checkFastPlanPrivilege(ctx, fastSelect, mysql.SelectPriv, mysql.DeletePriv) != nil {
		return nil
	}
	if fastSelect.IsTableDual {
		return PhysicalTableDual{}.Init(ctx, &property.StatsInfo{})
	}
	if ctx.GetSessionVars().TxnCtx.IsPessimistic {
		fastSelect.Lock = true
	}
	handleCol := fastSelect.findHandleCol()
	delPlan := Delete{
		SelectPlan: fastSelect,
		TblColPosInfos: TblColPosInfoSlice{
			TblColPosInfo{
				TblID:         fastSelect.TblInfo.ID,
				Start:         0,
				End:           fastSelect.schema.Len(),
				HandleOrdinal: handleCol.Index,
			},
		},
	}.Init(ctx)
	return delPlan
}

func findCol(tbl *model.TableInfo, colName *ast.ColumnName) *model.ColumnInfo {
	for _, col := range tbl.Columns {
		if col.Name.L == colName.Name.L {
			return col
		}
	}
	return nil
}

func colInfoToColumn(db model.CIStr, origTblName model.CIStr, tblName model.CIStr, asName model.CIStr, col *model.ColumnInfo, idx int) *expression.Column {
	return &expression.Column{
		ColName:     asName,
		OrigTblName: origTblName,
		DBName:      db,
		TblName:     tblName,
		RetType:     &col.FieldType,
		ID:          col.ID,
		UniqueID:    int64(col.Offset),
		Index:       idx,
	}
}

func (p *PointGetPlan) findHandleCol() *expression.Column {
	// fields len is 0 for update and delete.
	var handleCol *expression.Column
	tbl := p.TblInfo
	if tbl.PKIsHandle {
		for i, col := range p.TblInfo.Columns {
			if mysql.HasPriKeyFlag(col.Flag) && tbl.PKIsHandle {
				handleCol = p.schema.Columns[i]
			}
		}
	}
	if handleCol == nil {
		oneCol := p.schema.Columns[0]
		handleCol = colInfoToColumn(oneCol.DBName, oneCol.OrigTblName, oneCol.TblName, model.ExtraHandleName, model.NewExtraHandleColInfo(), p.schema.Len())
		p.schema.Append(handleCol)
	}
	return handleCol
}
