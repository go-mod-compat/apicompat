package abicheck

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"reflect"
	"strconv"
	"strings"
)

const (
	Unknown     = "unknown change"
	None        = "no change"
	NonBreaking = "non-breaking change"
	Breaking    = "breaking change"
)

type DeclChange struct {
	Change string
	Msg    string
}

type DeclChecker struct {
	binfo *types.Info
	ainfo *types.Info
}

func NewDeclChecker(bi, ai *types.Info) *DeclChecker {
	return &DeclChecker{binfo: bi, ainfo: ai}
}

func nonBreaking(msg string) (*DeclChange, error) { return &DeclChange{NonBreaking, msg}, nil }
func breaking(msg string) (*DeclChange, error)    { return &DeclChange{Breaking, msg}, nil }
func unknown(msg string) (*DeclChange, error)     { return &DeclChange{Unknown, msg}, nil }
func none() (*DeclChange, error)                  { return &DeclChange{None, ""}, nil }

// equal compares two declarations and returns true if they do not have
// incompatible changes. For example, comments aren't compared, names of
// arguments aren't compared etc.
func (c DeclChecker) Check(before, after ast.Decl) (*DeclChange, error) {
	// compare types, ignore comments etc, so reflect.DeepEqual isn't good enough

	if reflect.TypeOf(before) != reflect.TypeOf(after) {
		// Declaration type changed, such as GenDecl to FuncDecl (eg var/const to func)
		return breaking("changed declaration")
	}

	switch b := before.(type) {
	case *ast.GenDecl:
		a := after.(*ast.GenDecl)

		// getDecls flattened var/const blocks, so .Specs should contain just 1

		if reflect.TypeOf(b.Specs[0]) != reflect.TypeOf(a.Specs[0]) {
			// Spec changed, such as ValueSpec to TypeSpec (eg var/const to struct)
			return breaking("changed spec")
		}

		switch bspec := b.Specs[0].(type) {
		case *ast.ValueSpec:
			aspec := a.Specs[0].(*ast.ValueSpec)

			if bspec.Type == nil || aspec.Type == nil {
				// eg: var ErrSomeError = errors.New("Some Error")
				// cannot currently determine the type

				return unknown("cannot currently determine type")
			}

			if reflect.TypeOf(bspec.Type) != reflect.TypeOf(aspec.Type) {
				// eg change from int to []int
				return breaking("changed value spec type")
			}

			// var / const
			switch btype := bspec.Type.(type) {
			case *ast.Ident, *ast.SelectorExpr, *ast.StarExpr:
				// int/string/etc or bytes.Buffer/etc or *int/*bytes.Buffer/etc
				if c.binfo.TypeOf(bspec.Type) != c.ainfo.TypeOf(aspec.Type) {
					// type changed
					return breaking("changed type")
				}
			case *ast.ArrayType:
				// slice/array
				atype := aspec.Type.(*ast.ArrayType)
				if !c.exprEqual(btype, atype) {
					// change of length, or between array and slice or type
					return breaking("changed array/slice's length or type")
				}
			case *ast.MapType:
				// map
				atype := aspec.Type.(*ast.MapType)

				if !c.exprEqual(btype.Key, atype.Key) {
					return breaking("changed map's key's type")
				}
				if !c.exprEqual(btype.Value, atype.Value) {
					return breaking("changed map's value's type")
				}
			case *ast.InterfaceType:
				// this is a special case for just interface{}
				atype := aspec.Type.(*ast.InterfaceType)
				return c.checkInterface(btype, atype)
			case *ast.ChanType:
				// channel
				atype := aspec.Type.(*ast.ChanType)
				return c.checkChan(btype, atype)
			case *ast.FuncType:
				// func
				atype := aspec.Type.(*ast.FuncType)
				return c.checkFunc(btype, atype)
			case *ast.StructType:
				// anonymous struct
				atype := aspec.Type.(*ast.StructType)
				return c.checkStruct(btype, atype)
			default:
				return nil, fmt.Errorf("unknown val spec type: %T", btype)
			}
		case *ast.TypeSpec:
			aspec := a.Specs[0].(*ast.TypeSpec)

			// type struct/interface/aliased

			if reflect.TypeOf(bspec.Type) != reflect.TypeOf(aspec.Type) {
				// Spec change, such as from StructType to InterfaceType or different aliased types
				return breaking("changed type of value spec")
			}

			switch btype := bspec.Type.(type) {
			case *ast.InterfaceType:
				atype := aspec.Type.(*ast.InterfaceType)
				return c.checkInterface(btype, atype)
			case *ast.StructType:
				atype := aspec.Type.(*ast.StructType)
				return c.checkStruct(btype, atype)
			case *ast.Ident:
				// alias
				atype := aspec.Type.(*ast.Ident)
				if btype.Name != atype.Name {
					// Alias typing changed underlying types
					return breaking("alias changed its underlying type")
				}
			}
		}
	case *ast.FuncDecl:
		a := after.(*ast.FuncDecl)
		return c.checkFunc(b.Type, a.Type)
	default:
		return nil, fmt.Errorf("unknown declaration type: %T", before)
	}
	return none()
}

func (c DeclChecker) checkChan(before, after *ast.ChanType) (*DeclChange, error) {
	if !c.exprEqual(before.Value, after.Value) {
		return breaking("changed channel's type")
	}

	// If we're specifying a direction and it's not the same as before
	// (if we remove direction then that change isn't breaking)
	if before.Dir != after.Dir {
		if after.Dir != ast.SEND && after.Dir != ast.RECV {
			return nonBreaking("removed channel's direction")
		}
		return breaking("changed channel's direction")
	}
	return none()
}

func (c DeclChecker) checkInterface(before, after *ast.InterfaceType) (*DeclChange, error) {
	// interfaces don't care if methods are removed
	r := c.diffFields(before.Methods.List, after.Methods.List)
	if r.Added() {
		// Fields were added
		return breaking("members added")
	} else if r.Modified() {
		// Fields changed types
		return breaking("members changed types")
	} else if r.Removed() {
		return nonBreaking("members removed")
	}

	return none()
}

func (c DeclChecker) checkStruct(before, after *ast.StructType) (*DeclChange, error) {
	// structs don't care if fields were added
	r := c.diffFields(before.Fields.List, after.Fields.List)
	if r.Removed() {
		// Fields were removed
		return breaking("members removed")
	} else if r.Modified() {
		// Fields changed types
		return breaking("members changed types")
	} else if r.Added() {
		return nonBreaking("members added")
	}
	return none()
}

func (c DeclChecker) checkFunc(before, after *ast.FuncType) (*DeclChange, error) {
	// don't compare argument names
	bparams := stripNames(before.Params.List)
	aparams := stripNames(after.Params.List)

	r := c.diffFields(bparams, aparams)
	variadicMsg := r.RemoveVariadicCompatible(c)
	interfaceMsg, err := r.RemoveInterfaceCompatible(c)
	if err != nil {
		return nil, err
	}
	if r.Changed() {
		return breaking("parameter types changed")
	}

	if before.Results != nil {
		if after.Results == nil {
			// removed return parameter
			return breaking("removed return parameter")
		}

		// don't compare argument names
		bresults := stripNames(before.Results.List)
		aresults := stripNames(after.Results.List)

		// Adding return parameters to a function, when it didn't have any before is
		// ok, so only check if for breaking changes if there was parameters before
		if len(before.Results.List) > 0 {
			r := c.diffFields(bresults, aresults)
			if r.Changed() {
				return breaking("return parameters changed")
			}
		}
	}

	switch {
	case interfaceMsg != "":
		return nonBreaking(interfaceMsg)
	case variadicMsg != "":
		return nonBreaking(variadicMsg)
	default:
		return none()
	}
}

type diffResult struct {
	added,
	removed []*ast.Field
	modified [][2]*ast.Field
}

// Changed returns true if any of the fields were added, removed or modified
func (d diffResult) Changed() bool {
	return len(d.added) > 0 || len(d.removed) > 0 || len(d.modified) > 0
}

func (d diffResult) Added() bool    { return len(d.added) > 0 }
func (d diffResult) Removed() bool  { return len(d.removed) > 0 }
func (d diffResult) Modified() bool { return len(d.modified) > 0 }

// RemoveVariadicCompatible removes changes and returns a short msg describing
// the change if the added, removed and changed fields only represent an
// addition of variadic parameters or changes an existing field to variadic.
// If no compatible variadic changes were detected, msg will be an empty msg.
func (d *diffResult) RemoveVariadicCompatible(chkr DeclChecker) (msg string) {
	if len(d.added) == 1 && !d.Removed() && !d.Modified() {
		if _, ok := d.added[0].Type.(*ast.Ellipsis); ok {
			// we're adding a variadic
			d.added = []*ast.Field{}
			return "added a variadic parameter"
		}
	}

	if !d.Added() && !d.Removed() && len(d.modified) == 1 {
		btype := d.modified[0][0].Type
		variadic, ok := d.modified[0][1].Type.(*ast.Ellipsis)

		if ok && types.Identical(chkr.binfo.TypeOf(btype), chkr.ainfo.TypeOf(variadic.Elt)) {
			// we're changing to a variadic of the same type
			d.modified = [][2]*ast.Field{}
			return "change parameter to variadic"
		}
	}
	return ""
}

func (d *diffResult) RemoveInterfaceCompatible(chkr DeclChecker) (msg string, err error) {
	var compatible []int
	for i, mod := range d.modified {
		before, after := mod[0].Type, mod[1].Type
		btype, atype := chkr.binfo.TypeOf(before), chkr.ainfo.TypeOf(after)
		if btype != nil && atype != nil && types.IsInterface(btype) && types.IsInterface(atype) {
			bint, err := exprInterfaceType(chkr.binfo.Uses, before)
			if err != nil {
				return msg, err
			}
			aint, err := exprInterfaceType(chkr.ainfo.Uses, after)
			if err != nil {
				return msg, err
			}

			change, err := chkr.checkInterface(bint, aint)
			if err != nil {
				return msg, err
			}
			if change.Change != Breaking {
				compatible = append(compatible, i)
				msg = "compatible interface change"
			}
		}
	}

	// TODO cleanup
	modified := [][2]*ast.Field{}
	for i := range d.modified {
		var keep bool = true
		for c := range compatible {
			if c == i {
				keep = false
			}
		}
		if keep {
			modified = append(modified, d.modified[i])
		}
	}
	d.modified = modified
	return msg, nil
}

// stripNames strips the names from a fieldlist, which is usually a function's
// (or method's) parameter or results list, these are internal to the function.
// This returns a good-enough copy of the field list, but isn't a complete copy
// as some pointers remain, but no other modifications are made, so it's ok.
func stripNames(fields []*ast.Field) []*ast.Field {
	stripped := make([]*ast.Field, 0, len(fields))
	for _, f := range fields {
		stripped = append(stripped, &ast.Field{
			Doc:     f.Doc,
			Names:   nil, // nil the names
			Type:    f.Type,
			Tag:     f.Tag,
			Comment: f.Comment,
		})
	}
	return stripped
}

func (c DeclChecker) diffFields(before, after []*ast.Field) diffResult {
	// Presort after for quicker matching of fieldname -> type, may not be worthwhile
	AfterMembers := make(map[string]*ast.Field)
	for i, field := range after {
		AfterMembers[fieldKey(field, i)] = field
	}

	r := diffResult{}

	for i, bfield := range before {
		bkey := fieldKey(bfield, i)
		if afield, ok := AfterMembers[bkey]; ok {
			if !c.exprEqual(bfield.Type, afield.Type) {
				// modified
				r.modified = append(r.modified, [2]*ast.Field{bfield, afield})
			}
			delete(AfterMembers, bkey)
			continue
		}

		// Removed
		r.removed = append(r.removed, bfield)
	}

	// What's left in afterMembers has added
	for _, afield := range AfterMembers {
		r.added = append(r.added, afield)
	}

	return r
}

// Return an appropriate identifier for a field, if it has an ident (name)
// such as in the case of a struct/interface member, else, use it's provided
// position i, such as the case of a function's parameter or result list
func fieldKey(field *ast.Field, i int) string {
	if len(field.Names) > 0 {
		return field.Names[0].Name
	}
	// No name, probably a function, return position
	return strconv.FormatInt(int64(i), 10)
}

// exprEqual compares two ast.Expr to determine if they are equal
func (c DeclChecker) exprEqual(before, after ast.Expr) bool {
	if reflect.TypeOf(before) != reflect.TypeOf(after) {
		return false
	}

	switch btype := before.(type) {
	case *ast.ChanType:
		atype := after.(*ast.ChanType)
		change, _ := c.checkChan(btype, atype)
		return change.Change != Breaking
	case *ast.Ident:
		// types.Identical returns false for any custom types when comparing
		// the results from two different type checkers. So, just compare by
		// name. Eg, func (_ CustomType) {}, CustomType is not identical, even
		// though comparing the type itself is.
		// https://play.golang.org/p/t6P5Uz6fIa
		return types.ExprString(before) == types.ExprString(after)
	}

	return types.Identical(c.binfo.TypeOf(before), c.ainfo.TypeOf(after))
}

// exprInterfaceType returns a *ast.InterfaceType given an interface type using
// the worst possible method. It's used to determine whether two interfaces
// are compatible based on function parameters/results.
func exprInterfaceType(uses map[*ast.Ident]types.Object, expr ast.Expr) (*ast.InterfaceType, error) {
	var (
		pkg string
		sel *ast.Ident
	)
	switch etype := expr.(type) {
	case *ast.StarExpr:
		switch estar := etype.X.(type) {
		case *ast.SelectorExpr:
			pkg, sel = estar.X.(*ast.Ident).String(), estar.Sel
		case *ast.Ident:
			sel = estar
		}
	case *ast.SelectorExpr:
		pkg, sel = etype.X.(*ast.Ident).String(), etype.Sel
	case *ast.Ident:
		sel = etype
	default:
		return nil, errors.New("unknown interface type")
	}

	obj, ok := uses[sel]
	if !ok {
		return nil, errors.New("could not find interface in uses")
	}

	// use is: *types.TypeName, string: type io.Writer interface{Write(p []byte) (n int, err error)}

	// Remove the package name from the source in order to parse valid program
	src := strings.Replace(obj.String(), fmt.Sprintf("type %s.", pkg), "type ", 1)
	src = fmt.Sprintf("package expr\n%s", src)

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, pkg, src, 0)
	if err != nil {
		return nil, fmt.Errorf("%s parsing: %s", err, src)
	}
	return file.Decls[0].(*ast.GenDecl).Specs[0].(*ast.TypeSpec).Type.(*ast.InterfaceType), nil
}
