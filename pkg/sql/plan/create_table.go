// Copyright 2021 Matrix Origin
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package plan

import (
	"fmt"
	"go/constant"
	"math"

	"github.com/matrixorigin/matrixone/pkg/compress"
	"github.com/matrixorigin/matrixone/pkg/container/types"
	"github.com/matrixorigin/matrixone/pkg/defines"
	"github.com/matrixorigin/matrixone/pkg/errno"
	"github.com/matrixorigin/matrixone/pkg/sql/errors"
	"github.com/matrixorigin/matrixone/pkg/sql/parsers/tree"
	"github.com/matrixorigin/matrixone/pkg/vm/engine"
)

// BuildCreateTable do semantic analyze and get table definition from tree.CreateTable to make create table plan.
func (b *build) BuildCreateTable(stmt *tree.CreateTable, plan *CreateTable) error {
	defs := make([]engine.TableDef, 0, len(stmt.Defs))

	// 1. get database's information
	dbName, tblName, err := b.tableInfo(stmt.Table)
	if err != nil {
		return err
	}
	db, err := b.e.Database(dbName, nil)
	if err != nil {
		return err
	}

	// 2. analyze and get table definition
	pkNames := []string(nil) // stores primary key column's names.
	pkFlag := true           // if true, we need to add a definition for the primary key additionally
	for i := range stmt.Defs {
		def, pks, err := b.getTableDef(stmt.Defs[i])
		if err != nil {
			return err
		}

		// only allow primary keys to be defined once
		if pkNames != nil && pks != nil {
			return errors.New(errno.SyntaxErrororAccessRuleViolation, "Multiple primary key defined")
		}

		if _, ok := def.(*engine.PrimaryIndexDef); ok {
			pkFlag = false
		}
		if pks != nil {
			pkNames = pks
		}
		defs = append(defs, def)
	}

	for _, option := range stmt.Options {
		def, _ := b.getOptionDef(option)
		defs = append(defs, def)
	}
	if pkFlag && pkNames != nil {
		defs = append(defs, &engine.PrimaryIndexDef{Names: pkNames})
	}

	if stmt.PartitionOption != nil {
		return errors.New(errno.SQLStatementNotYetComplete, "partitionBy not yet complete")
	}

	plan.IfNotExistFlag = stmt.IfNotExists
	plan.Defs = defs
	plan.Db = db
	plan.Id = tblName
	return nil
}

func (b *build) getOptionDef(option tree.TableOption) (engine.TableDef, error) {
	switch n := option.(type) {
	case *tree.TableOptionProperties:
		properties := make([]engine.Property, len(n.Preperties))
		for i, property := range n.Preperties {
			properties[i] = engine.Property{
				Key:   property.Key,
				Value: property.Value}
		}
		return &engine.PropertiesDef{Properties: properties}, nil

	}
	return nil, nil
}

func (b *build) tableInfo(stmt tree.TableExpr) (string, string, error) {
	tbl, ok := stmt.(tree.TableName)
	if !ok {
		return "", "", errors.New(errno.SQLStatementNotYetComplete, fmt.Sprintf("unsupport table: '%v'", stmt))
	}
	if len(tbl.SchemaName) == 0 {
		tbl.SchemaName = tree.Identifier(b.db)
	}
	return string(tbl.SchemaName), string(tbl.ObjectName), nil
}

func (b *build) getTableDef(def tree.TableDef) (engine.TableDef, []string, error) {
	var primaryKeys []string = nil

	switch n := def.(type) {
	case *tree.ColumnTableDef:
		typ, err := b.getTableDefType(n.Type)
		if err != nil {
			return nil, nil, err
		}

		defaultExpr, err := getDefaultExprFromColumnDef(n, typ)
		if err != nil {
			return nil, nil, err
		}

		for _, attr := range n.Attributes {
			if _, ok := attr.(*tree.AttributePrimaryKey); ok {
				primaryKeys = append(primaryKeys, n.Name.Parts[0])
			}
		}

		return &engine.AttributeDef{
			Attr: engine.Attribute{
				Name:    n.Name.Parts[0],
				Alg:     compress.Lz4,
				Type:    *typ,
				Default: defaultExpr,
			},
		}, primaryKeys, nil
	case *tree.PrimaryKeyIndex:
		mapPrimaryKeyNames := map[string]struct{}{}
		primaryKeys = make([]string, len(n.KeyParts))

		for i, key := range n.KeyParts {
			name := key.ColName.Parts[0] // name of primary key column

			if _, ok := mapPrimaryKeyNames[name]; ok {
				return nil, nil, errors.New(errno.InvalidTableDefinition, fmt.Sprintf("Duplicate column name '%s'", name))
			}
			primaryKeys[i] = name
			mapPrimaryKeyNames[name] = struct{}{}
		}

		return &engine.PrimaryIndexDef{
			Names: primaryKeys,
		}, primaryKeys, nil
	case *tree.Index:
		keyType := engine.ZoneMap
		switch n.KeyType {
		case tree.INDEX_TYPE_BSI:
			keyType = engine.BsiIndex
		case tree.INDEX_TYPE_ZONEMAP:
			keyType = engine.ZoneMap
		}

		nameMap := map[string]struct{}{}
		colNames := make([]string, len(n.KeyParts))
		for i, key := range n.KeyParts {
			name := key.ColName.Parts[0] // name of index column

			if _, ok := nameMap[name]; ok {
				return nil, nil, errors.New(errno.InvalidTableDefinition, fmt.Sprintf("Duplicate column name '%s'", key.ColName.Parts[0]))
			}
			colNames[i] = name
			nameMap[name] = struct{}{}
		}

		return &engine.IndexTableDef{
			Name:     n.Name,
			Typ:      keyType,
			ColNames: colNames,
		}, primaryKeys, nil
	default:
		return nil, nil, errors.New(errno.SQLStatementNotYetComplete, fmt.Sprintf("unsupport table def: '%v'", def))
	}
}

func (b *build) getTableDefType(typ tree.ResolvableTypeReference) (*types.Type, error) {
	if n, ok := typ.(*tree.T); ok {
		switch uint8(n.InternalType.Oid) {
		case defines.MYSQL_TYPE_BOOL:
			return &types.Type{Oid: types.T_bool, Size: 1, Width: n.InternalType.Width}, nil
		case defines.MYSQL_TYPE_TINY:
			if n.InternalType.Unsigned {
				return &types.Type{Oid: types.T_uint8, Size: 1, Width: n.InternalType.Width}, nil
			}
			return &types.Type{Oid: types.T_int8, Size: 1, Width: n.InternalType.Width}, nil
		case defines.MYSQL_TYPE_SHORT:
			if n.InternalType.Unsigned {
				return &types.Type{Oid: types.T_uint16, Size: 2, Width: n.InternalType.Width}, nil
			}
			return &types.Type{Oid: types.T_int16, Size: 2, Width: n.InternalType.Width}, nil
		case defines.MYSQL_TYPE_LONG:
			if n.InternalType.Unsigned {
				return &types.Type{Oid: types.T_uint32, Size: 4, Width: n.InternalType.Width}, nil
			}
			return &types.Type{Oid: types.T_int32, Size: 4, Width: n.InternalType.Width}, nil
		case defines.MYSQL_TYPE_LONGLONG:
			if n.InternalType.Unsigned {
				return &types.Type{Oid: types.T_uint64, Size: 8, Width: n.InternalType.Width}, nil
			}
			return &types.Type{Oid: types.T_int64, Size: 8, Width: n.InternalType.Width}, nil
		case defines.MYSQL_TYPE_FLOAT:
			return &types.Type{Oid: types.T_float32, Size: 4, Width: n.InternalType.Width}, nil
		case defines.MYSQL_TYPE_DOUBLE:
			return &types.Type{Oid: types.T_float64, Size: 8, Width: n.InternalType.Width}, nil
		case defines.MYSQL_TYPE_STRING:
			if n.InternalType.DisplayWith == -1 { // type char
				return &types.Type{Oid: types.T_char, Size: 24, Width: 1}, nil
			}
			return &types.Type{Oid: types.T_char, Size: 24, Width: n.InternalType.DisplayWith}, nil
		case defines.MYSQL_TYPE_VAR_STRING, defines.MYSQL_TYPE_VARCHAR:
			if n.InternalType.DisplayWith == -1 { // type char
				return &types.Type{Oid: types.T_char, Size: 24, Width: 1}, nil
			}
			return &types.Type{Oid: types.T_varchar, Size: 24, Width: n.InternalType.DisplayWith}, nil
		case defines.MYSQL_TYPE_DATE:
			return &types.Type{Oid: types.T_date, Size: 4}, nil
		case defines.MYSQL_TYPE_DATETIME:
			return &types.Type{Oid: types.T_datetime, Size: 8}, nil
		case defines.MYSQL_TYPE_TIMESTAMP:
			return &types.Type{Oid: types.T_timestamp, Size: 8, Precision: n.InternalType.Precision}, nil
		case defines.MYSQL_TYPE_DECIMAL:
			if n.InternalType.DisplayWith > 18 {
				return &types.Type{Oid: types.T_decimal128, Size: 16, Width: n.InternalType.DisplayWith, Scale: n.InternalType.Precision}, nil
			}
			return &types.Type{Oid: types.T_decimal64, Size: 8, Width: n.InternalType.DisplayWith, Scale: n.InternalType.Precision}, nil
		}
	}
	return nil, errors.New(errno.IndeterminateDatatype, fmt.Sprintf("unsupport type: '%v'", typ))
}

// getDefaultExprFromColumnDef returns
// has default expr or not / column default expr string / is null expression / error msg
// from column definition when create table.
// it will verify that default expression's type and value match, and if not,
// there will make a simple type conversion for values
// For example:
// 		create table testTb1 (first int default 15.6) ==> create table testTb1 (first int default 16)
//		create table testTb2 (first int default 'abc') ==> error(Invalid default value for 'first')
func getDefaultExprFromColumnDef(column *tree.ColumnTableDef, typ *types.Type) (engine.DefaultExpr, error) {
	allowNull := true // be false when column has not null constraint

	{
		for _, attr := range column.Attributes {
			if nullAttr, ok := attr.(*tree.AttributeNull); ok && !nullAttr.Is {
				allowNull = false
				break
			}
		}
	}

	for _, attr := range column.Attributes {
		if d, ok := attr.(*tree.AttributeDefault); ok {
			defaultExpr := d.Expr
			if isNullExpr(defaultExpr) {
				if !allowNull {
					return engine.EmptyDefaultExpr, errors.New(errno.InvalidColumnDefinition, fmt.Sprintf("Invalid default value for '%s'", column.Name.Parts[0]))
				}
				return engine.MakeDefaultExpr(true, nil, true), nil
			}

			// check value and its type, only support constant value for default expression now.
			var value interface{}
			var err error
			if value, err = buildConstant(*typ, defaultExpr); err != nil { // build constant failed
				return engine.EmptyDefaultExpr, errors.New(errno.InvalidColumnDefinition, fmt.Sprintf("Invalid default value for '%s'", column.Name.Parts[0]))
			}
			if _, err = rangeCheck(value, *typ, "", 0); err != nil { // value out of range
				return engine.EmptyDefaultExpr, errors.New(errno.InvalidColumnDefinition, fmt.Sprintf("Invalid default value for '%s'", column.Name.Parts[0]))
			}
			return engine.MakeDefaultExpr(true, value, false), nil
		}
	}

	// if no definition and allow null value for this column, default will be null
	if allowNull {
		return engine.MakeDefaultExpr(true, "", true), nil
	}
	return engine.EmptyDefaultExpr, nil
}

// rangeCheck do range check for value, and do type conversion.
func rangeCheck(value interface{}, typ types.Type, columnName string, rowNumber int) (interface{}, error) {
	errString := "Out of range value for column '%s' at row %d"

	switch v := value.(type) {
	case int64:
		switch typ.Oid {
		case types.T_int8:
			if v <= math.MaxInt8 && v >= math.MinInt8 {
				return int8(v), nil
			}
		case types.T_int16:
			if v <= math.MaxInt16 && v >= math.MinInt16 {
				return int16(v), nil
			}
		case types.T_int32:
			if v <= math.MaxInt32 && v >= math.MinInt32 {
				return int32(v), nil
			}
		case types.T_int64:
			return v, nil
		default:
			return nil, errors.New(errno.DatatypeMismatch, "unexpected type and value")
		}
		return nil, errors.New(errno.DataException, fmt.Sprintf(errString, columnName, rowNumber))
	case uint64:
		switch typ.Oid {
		case types.T_uint8:
			if v <= math.MaxUint8 {
				return uint8(v), nil
			}
		case types.T_uint16:
			if v <= math.MaxUint16 {
				return uint16(v), nil
			}
		case types.T_uint32:
			if v <= math.MaxUint32 {
				return uint32(v), nil
			}
		case types.T_uint64:
			return v, nil
		default:
			return nil, errors.New(errno.DatatypeMismatch, "unexpected type and value")
		}
		return nil, errors.New(errno.DataException, fmt.Sprintf(errString, columnName, rowNumber))
	case float32:
		if typ.Oid == types.T_float32 {
			return v, nil
		}
		return nil, errors.New(errno.DatatypeMismatch, "unexpected type and value")
	case float64:
		switch typ.Oid {
		case types.T_float32:
			if v <= math.MaxFloat32 && v >= -math.MaxFloat32 {
				return float32(v), nil
			}
		case types.T_float64:
			return v, nil
		default:
			return nil, errors.New(errno.DatatypeMismatch, "unexpected type and value")
		}
		return nil, errors.New(errno.DataException, fmt.Sprintf(errString, columnName, rowNumber))
	case string:
		switch typ.Oid {
		case types.T_char, types.T_varchar: // string family should compare the length but not value
			if len(v) > math.MaxUint16 {
				return nil, errors.New(errno.DataException, "length out of uint16 is unexpected for char / varchar value")
			}
			if len(v) <= int(typ.Width) {
				return v, nil
			}
		default:
			return nil, errors.New(errno.DatatypeMismatch, "unexpected type and value")
		}
		return nil, errors.New(errno.DataException, fmt.Sprintf("Data too long for column '%s' at row %d", columnName, rowNumber))
	case types.Date, types.Datetime, types.Timestamp, types.Decimal64, types.Decimal128, bool:
		return v, nil
	default:
		return nil, errors.New(errno.DatatypeMismatch, "unexpected type and value")
	}
}

// isDefaultExpr returns true when input expression means default expr
func isDefaultExpr(expr tree.Expr) bool {
	_, ok := expr.(*tree.DefaultVal)
	return ok
}

// isNullExpr returns true when input expression means null expr
func isNullExpr(expr tree.Expr) bool {
	v, ok := expr.(*tree.NumVal)
	return ok && v.Value.Kind() == constant.Unknown
}
