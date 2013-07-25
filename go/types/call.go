// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// This file implements typechecking of call and selector expressions.

package types

import (
	"go/ast"
	"go/token"
)

func (check *checker) call(x *operand, e *ast.CallExpr) {
	check.exprOrType(x, e.Fun)
	if x.mode == invalid {
		// We don't have a valid call or conversion but we have a list of arguments.
		// Typecheck them independently for better partial type information in
		// the presence of type errors.
		for _, arg := range e.Args {
			check.expr(x, arg)
		}
		x.mode = invalid
		x.expr = e
		return
	}

	if x.mode == typexpr {
		check.conversion(x, e, x.typ)
		return
	}

	if sig, ok := x.typ.Underlying().(*Signature); ok {
		// function/method call

		passSlice := false
		if e.Ellipsis.IsValid() {
			// last argument is of the form x...
			if sig.isVariadic {
				passSlice = true
			} else {
				check.errorf(e.Ellipsis, "cannot use ... in call to non-variadic %s", e.Fun)
				// ok to continue
			}
		}

		// evaluate arguments
		n := len(e.Args) // argument count
		if n == 1 {
			// single argument but possibly a multi-valued function call
			arg := e.Args[0]
			check.expr(x, arg)
			if x.mode != invalid {
				if t, ok := x.typ.(*Tuple); ok {
					// argument is multi-valued function call
					n = t.Len()
					for i := 0; i < n; i++ {
						x.mode = value
						x.expr = arg
						x.typ = t.At(i).typ
						check.argument(sig, i, x, passSlice && i == n-1)
					}
				} else {
					// single value
					check.argument(sig, 0, x, passSlice)
				}
			} else {
				n = sig.params.Len() // avoid additional argument length errors below
			}
		} else {
			// zero or multiple arguments
			for i, arg := range e.Args {
				check.expr(x, arg)
				if x.mode != invalid {
					check.argument(sig, i, x, passSlice && i == n-1)
				}
			}
		}

		// check argument count
		if sig.isVariadic {
			// a variadic function accepts an "empty"
			// last argument: count one extra
			n++
		}
		if n < sig.params.Len() {
			check.errorf(e.Fun.Pos(), "too few arguments in call to %s", e.Fun)
			// ok to continue
		}

		// determine result
		switch sig.results.Len() {
		case 0:
			x.mode = novalue
		case 1:
			x.mode = value
			x.typ = sig.results.vars[0].typ // unpack tuple
		default:
			x.mode = value
			x.typ = sig.results
		}
		x.expr = e
		return
	}

	if bin, ok := x.typ.(*Builtin); ok {
		check.builtin(x, e, bin)
		return
	}

	check.invalidOp(x.pos(), "cannot call non-function %s", x)
	x.mode = invalid
	x.expr = e
}

// argument checks passing of argument x to the i'th parameter of the given signature.
// If passSlice is set, the argument is followed by ... in the call.
func (check *checker) argument(sig *Signature, i int, x *operand, passSlice bool) {
	n := sig.params.Len()

	// determine parameter type
	var typ Type
	switch {
	case i < n:
		typ = sig.params.vars[i].typ
	case sig.isVariadic:
		typ = sig.params.vars[n-1].typ
		if debug {
			if _, ok := typ.(*Slice); !ok {
				check.dump("%s: expected slice type, got %s", sig.params.vars[n-1].Pos(), typ)
			}
		}
	default:
		check.errorf(x.pos(), "too many arguments")
		return
	}

	if passSlice {
		// argument is of the form x...
		if i != n-1 {
			check.errorf(x.pos(), "can only use ... with matching parameter")
			return
		}
		if _, ok := x.typ.(*Slice); !ok {
			check.errorf(x.pos(), "cannot use %s as parameter of type %s", x, typ)
			return
		}
	} else if sig.isVariadic && i >= n-1 {
		// use the variadic parameter slice's element type
		typ = typ.(*Slice).elt
	}

	if !check.assignment(x, typ) && x.mode != invalid {
		check.errorf(x.pos(), "cannot pass argument %s to parameter of type %s", x, typ)
	}
}

func (check *checker) selector(x *operand, e *ast.SelectorExpr) {
	// these must be declared before the "goto Error" statements
	var (
		obj      Object
		index    []int
		indirect bool
	)

	sel := e.Sel.Name
	// If the identifier refers to a package, handle everything here
	// so we don't need a "package" mode for operands: package names
	// can only appear in qualified identifiers which are mapped to
	// selector expressions.
	if ident, ok := e.X.(*ast.Ident); ok {
		if pkg, ok := check.topScope.LookupParent(ident.Name).(*Package); ok {
			check.recordObject(ident, pkg)
			exp := pkg.scope.Lookup(sel)
			if exp == nil {
				check.errorf(e.Pos(), "%s not declared by package %s", sel, ident)
				goto Error
			} else if !exp.IsExported() {
				// gcimported package scopes contain non-exported
				// objects such as types used in partially exported
				// objects - do not accept them
				check.errorf(e.Pos(), "%s not exported by package %s", sel, ident)
				goto Error
			}
			check.recordObject(e.Sel, exp)
			// Simplified version of the code for *ast.Idents:
			// - imported packages use types.Scope and types.Objects
			// - imported objects are always fully initialized
			switch exp := exp.(type) {
			case *Const:
				assert(exp.Val != nil)
				x.mode = constant
				x.typ = exp.typ
				x.val = exp.val
			case *TypeName:
				x.mode = typexpr
				x.typ = exp.typ
			case *Var:
				x.mode = variable
				x.typ = exp.typ
			case *Func:
				x.mode = value
				x.typ = exp.typ
			default:
				unreachable()
			}
			x.expr = e
			return
		}
	}

	check.exprOrType(x, e.X)
	if x.mode == invalid {
		goto Error
	}

	obj, index, indirect = LookupFieldOrMethod(x.typ, check.pkg, sel)
	if obj == nil {
		if index != nil {
			// TODO(gri) should provide actual type where the conflict happens
			check.invalidOp(e.Pos(), "ambiguous selector %s", sel)
		} else {
			check.invalidOp(e.Pos(), "%s has no field or method %s", x, sel)
		}
		goto Error
	}

	// TODO(gri) don't create a lookupResult if no Objects map exists
	check.recordObject(e.Sel, lookupResult(x.typ, obj, index, indirect))

	if x.mode == typexpr {
		// method expression
		m, _ := obj.(*Func)
		if m == nil {
			check.invalidOp(e.Pos(), "%s has no method %s", x, sel)
			goto Error
		}

		// verify that m is in the method set of x.typ
		if !indirect && ptrRecv(m) {
			check.invalidOp(e.Pos(), "%s is not in method set of %s", sel, x.typ)
			goto Error
		}

		// the receiver type becomes the type of the first function
		// argument of the method expression's function type
		var params []*Var
		sig := m.typ.(*Signature)
		if sig.params != nil {
			params = sig.params.vars
		}
		x.mode = value
		x.typ = &Signature{
			params:     NewTuple(append([]*Var{NewVar(token.NoPos, check.pkg, "", x.typ)}, params...)...),
			results:    sig.results,
			isVariadic: sig.isVariadic,
		}

	} else {
		// regular selector
		switch obj := obj.(type) {
		case *Var:
			x.mode = variable
			x.typ = obj.typ

		case *Func:
			// TODO(gri) This code appears elsewhere, too. Factor!
			// verify that obj is in the method set of x.typ (or &(x.typ) if x is addressable)
			//
			// spec: "A method call x.m() is valid if the method set of (the type of) x
			//        contains m and the argument list can be assigned to the parameter
			//        list of m. If x is addressable and &x's method set contains m, x.m()
			//        is shorthand for (&x).m()".
			if !indirect && x.mode != variable && ptrRecv(obj) {
				check.invalidOp(e.Pos(), "%s is not in method set of %s", sel, x)
				goto Error
			}

			if debug {
				// Verify that LookupFieldOrMethod and MethodSet.Lookup agree.
				typ := x.typ
				if x.mode == variable {
					// If typ is not an (unnamed) pointer, use *typ instead,
					// because the method set of *typ includes the methods
					// of typ.
					// Variables are addressable, so we can always take their
					// address.
					if _, isPtr := typ.(*Pointer); !isPtr {
						typ = &Pointer{base: typ}
					}
				}
				// If we created a synthetic pointer type above, we will throw
				// away the method set computed here after use.
				// TODO(gri) Consider also using a method set cache for the lifetime
				// of checker once we rely on MethodSet lookup instead of individual
				// lookup.
				mset := typ.MethodSet()
				if m := mset.Lookup(check.pkg, sel); m == nil || m.Func != obj {
					check.dump("%s: (%s).%v -> %s", e.Pos(), typ, obj.name, m)
					check.dump("%s\n", mset)
					panic("method sets and lookup don't agree")
				}
			}

			x.mode = value
			x.typ = obj.typ

		default:
			unreachable()
		}
	}

	// everything went well
	x.expr = e
	return

Error:
	x.mode = invalid
	x.expr = e
}
