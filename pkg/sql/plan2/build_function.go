// Copyright 2021 - 2022 Matrix Origin
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

package plan2

import (
	"go/constant"
	"strings"

	"github.com/matrixorigin/matrixone/pkg/container/types"
	"github.com/matrixorigin/matrixone/pkg/errno"
	"github.com/matrixorigin/matrixone/pkg/pb/plan"
	"github.com/matrixorigin/matrixone/pkg/sql/errors"
	"github.com/matrixorigin/matrixone/pkg/sql/parsers/tree"
	"github.com/matrixorigin/matrixone/pkg/sql/plan2/function"
)

func getFunctionExprByNameAndPlanExprs(name string, exprs []*Expr) (resultExpr *Expr, isAgg bool, err error) {
	name = strings.ToLower(name)

	// deal with special function
	switch name {
	case "+", "-":
		if len(exprs) != 2 {
			return nil, false, errors.New(errno.SyntaxErrororAccessRuleViolation, "operator function need two args")
		}
		if exprs[0].Typ.Id == plan.Type_DATE && exprs[1].Typ.Id == plan.Type_INTERVAL {
			resultExpr, err = getIntervalFunction(name, exprs[0], exprs[1])
			return
		}
		if exprs[0].Typ.Id == plan.Type_INTERVAL && exprs[1].Typ.Id == plan.Type_DATE {
			resultExpr, err = getIntervalFunction(name, exprs[1], exprs[0])
			return
		}
	case "and", "or", "not":
		if err := convertValueIntoBool(name, exprs, true); err != nil {
			return nil, false, err
		}
	case "=", "<", "<=", ">", ">=", "<>":
		if err := convertValueIntoBool(name, exprs, false); err != nil {
			return nil, false, err
		}
	}

	// get args(exprs) & types
	argsLength := len(exprs)
	argsType := make([]types.T, argsLength)
	for idx, expr := range exprs {
		argsType[idx] = types.T(expr.Typ.Id)
	}

	// get function definition
	funcDef, funcId, argsCastType, err := function.GetFunctionByName(name, argsType)
	if err != nil {
		return
	}
	if argsCastType != nil {
		if len(argsCastType) != argsLength {
			err = errors.New(errno.SyntaxErrororAccessRuleViolation, "cast types length not match args length")
			return
		}
		for idx, castType := range argsCastType {
			if argsType[idx] != castType {
				exprs[idx], err = appendCastExpr(exprs[idx], &plan.Type{
					Id: plan.Type_TypeId(castType),
				})
				if err != nil {
					return
				}
			}
		}
	}

	// return new expr
	returnType := &Type{
		Id: plan.Type_TypeId(funcDef.ReturnTyp),
	}
	resultExpr = &Expr{
		Expr: &plan.Expr_F{
			F: &plan.Function{
				Func: getFunctionObjRef(funcId, name),
				Args: exprs,
			},
		},
		Typ: returnType,
	}
	isAgg = funcDef.IsAggregate()

	return
}

func getFunctionExprByNameAndAstExprs(name string, astExprs []tree.Expr, ctx CompilerContext, query *Query, node *Node, binderCtx *BinderContext, needAgg bool) (resultExpr *Expr, isAgg bool, err error) {
	name = strings.ToLower(name)
	args := make([]*Expr, len(astExprs))
	// deal with special function
	switch name {
	case "extract":
		kindExpr := astExprs[0].(*tree.UnresolvedName)
		astExprs[0] = tree.NewNumVal(constant.MakeString(kindExpr.Parts[0]), kindExpr.Parts[0], false)
	}

	isAgg = true

	// get args
	var expr *Expr
	var paramIsAgg bool
	for idx, astExpr := range astExprs {
		expr, paramIsAgg, err = buildExpr(astExpr, ctx, query, node, binderCtx, needAgg)
		if err != nil {
			return
		}
		isAgg = isAgg && paramIsAgg
		args[idx] = expr
	}

	// deal with special function
	switch name {
	case "date":
		resultExpr, err = appendCastExpr(args[0], &plan.Type{
			Id: plan.Type_DATE,
		})
	case "interval":
		resultExpr, err = appendCastExpr(args[0], &plan.Type{
			Id: plan.Type_INTERVAL,
		})
	default:
		resultExpr, paramIsAgg, err = getFunctionExprByNameAndPlanExprs(name, args)
		if paramIsAgg {
			node.AggList = append(node.AggList, resultExpr)
			resultExpr = &Expr{
				Typ: resultExpr.Typ,
				Expr: &plan.Expr_Col{
					Col: &ColRef{
						RelPos: -2,
						ColPos: int32(len(node.AggList) - 1),
					},
				},
			}
			isAgg = true
		}
	}

	return
}

func appendCastExpr(expr *Expr, toType *Type) (*Expr, error) {
	argsType := []types.T{
		types.T(expr.Typ.Id),
		types.T(toType.Id),
	}
	_, funcId, _, err := function.GetFunctionByName("cast", argsType)
	if err != nil {
		return nil, err
	}
	return &Expr{
		Expr: &plan.Expr_F{
			F: &plan.Function{
				Func: getFunctionObjRef(funcId, "cast"),
				Args: []*Expr{expr},
			},
		},
		Typ: toType,
	}, nil
}

func getFunctionObjRef(funcId int64, name string) *ObjectRef {
	return &ObjectRef{
		Obj:     funcId,
		ObjName: name,
	}
}

func getIntervalFunction(name string, dateExpr *Expr, intervalExpr *Expr) (*Expr, error) {
	strExpr := intervalExpr.Expr.(*plan.Expr_F).F.Args[0].Expr
	intervalStr := strExpr.(*plan.Expr_C).C.Value.(*plan.Const_Sval).Sval
	intervalArray := strings.Split(intervalStr, " ")

	intervalType, err := types.IntervalTypeOf(intervalArray[1])
	if err != nil {
		return nil, err
	}
	returnNum, returnType, err := types.NormalizeInterval(intervalArray[0], intervalType)
	if err != nil {
		return nil, err
	}

	// only support date operator now
	namesMap := map[string]string{
		"+": "date_add",
		"-": "date_sub",
	}

	exprs := []*Expr{
		dateExpr,
		{
			Expr: &plan.Expr_C{
				C: &Const{
					Value: &plan.Const_Ival{
						Ival: returnNum,
					},
				},
			},
			Typ: &plan.Type{
				Id:   plan.Type_INT64,
				Size: 8,
			},
		},
		{
			Expr: &plan.Expr_C{
				C: &Const{
					Value: &plan.Const_Ival{
						Ival: int64(returnType),
					},
				},
			},
			Typ: &plan.Type{
				Id:   plan.Type_INT64,
				Size: 8,
			},
		},
	}

	resultExpr, _, err := getFunctionExprByNameAndPlanExprs(namesMap[name], exprs)

	return resultExpr, err
}

func convertValueIntoBool(name string, args []*Expr, isLogic bool) error {
	if !isLogic && (len(args) != 2 || (args[0].Typ.Id != plan.Type_BOOL && args[1].Typ.Id != plan.Type_BOOL)) {
		return nil
	}
	for _, arg := range args {
		if arg.Typ.Id == plan.Type_BOOL {
			continue
		}
		switch ex := arg.Expr.(type) {
		case *plan.Expr_C:
			arg.Typ.Id = plan.Type_BOOL
			switch value := ex.C.Value.(type) {
			case *plan.Const_Ival:
				if value.Ival == 0 {
					ex.C.Value = &plan.Const_Bval{Bval: false}
				} else if value.Ival == 1 {
					ex.C.Value = &plan.Const_Bval{Bval: true}
				} else {
					return errors.New("", "the params type is not right")
				}
			}
		}
	}
	return nil
}
