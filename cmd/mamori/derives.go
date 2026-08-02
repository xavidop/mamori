// This file discovers WithDerive call sites statically: every place source
// code calls github.com/xavidop/mamori's generic WithDerive[T](fn,
// writes...) and what write paths it declares for T. extract.go's Extract
// wires the result into StructInfo as KindDerived fields, so a field a
// WithDerive hook populates stops being invisible to `mamori explain`/
// `mamori diff` the same way a `source:` tagged field already is not
// (decision D1: the CLI reads source and never resolves or runs anything,
// so "which fields does this hook write" can only come from reading the call
// site itself, never from running the hook).
package main

import (
	"go/ast"
	"go/types"
	"strconv"
	"strings"

	"golang.org/x/tools/go/packages"
)

// mamoriPkgPath is github.com/xavidop/mamori's own import path: the package
// a WithDerive callee must belong to for withDeriveTypeArg to accept it, so
// a local function that merely happens to be named WithDerive (or a
// WithDerive belonging to some unrelated package) is never mistaken for it.
const mamoriPkgPath = "github.com/xavidop/mamori"

// findDerives walks every already-loaded package's syntax tree for a call to
// mamori's WithDerive[T] and returns, for every T it finds, the write paths
// declared for it - accumulated across every call site for the same T,
// wherever in the module that call site lives (a call in one package can
// declare writes for a T defined in another - see
// TestFindDerivesCrossPackage) - keyed by "pkgpath.TypeName" using T's own
// package, never the call site's.
//
// A literal path that names no field on T (directly, or through a dotted
// nested-struct path - the same shape a source: tag's own dotted Path uses)
// is dropped from the declared list: this matches WithDerive's own
// documented runtime rule that such a path simply never reports as written,
// so it is not a discovery failure and does not mark the type incomplete.
//
// The second return value instead names every type key for which at least
// one WithDerive argument could not be read statically at all - a variable,
// a spread slice, any non-literal expression. Extract sets
// StructInfo.DerivesIncomplete from it, and explain prints a note rather
// than silently under-reporting what such a hook might write.
//
// pkgs is whatever the caller already loaded (Extract's own loadPackages
// result); findDerives never loads packages itself, and in particular never
// needs packages.Config.Tests set - explain describes the shipping config
// surface, so a WithDerive call that exists only in a _test.go file is
// invisible here exactly as Extract already makes a _test.go-only source:
// tag invisible.
func findDerives(pkgs []*packages.Package) (map[string][]string, map[string]bool) {
	declared := make(map[string][]string)
	incomplete := make(map[string]bool)

	for _, pkg := range pkgs {
		for _, file := range pkg.Syntax {
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				named, ok := withDeriveTypeArg(pkg, call)
				if !ok {
					return true
				}
				key := derivesKey(named)

				// Declared paths: every argument after the hook. A literal
				// yields a constant.
				for _, a := range call.Args[1:] {
					tv, hasType := pkg.TypesInfo.Types[a]
					if !hasType || tv.Value == nil {
						// non-literal: mark this type's discovery incomplete
						incomplete[key] = true
						continue
					}
					// tv.Value.String() is quoted; use strconv.Unquote.
					path, err := strconv.Unquote(tv.Value.String())
					if err != nil {
						incomplete[key] = true
						continue
					}
					if _, ok := fieldTypeByPath(named, path); !ok {
						// Names no field on T: a valid, deliberate WithDerive
						// call per its own doc comment, not something the
						// matcher failed to read - dropped, not incomplete.
						continue
					}
					declared[key] = append(declared[key], path)
				}
				return true
			})
		}
	}
	return declared, incomplete
}

// withDeriveTypeArg reports whether call is a call to mamori's WithDerive.
//
// Resolve the callee through types, never by name text, so a local function
// called WithDerive is not mistaken for mamori's.
func withDeriveTypeArg(pkg *packages.Package, call *ast.CallExpr) (*types.Named, bool) {
	var id *ast.Ident
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		id = fn
	case *ast.SelectorExpr:
		id = fn.Sel
	case *ast.IndexExpr: // an explicit type argument: WithDerive[Config](...)
		switch inner := fn.X.(type) {
		case *ast.Ident:
			id = inner
		case *ast.SelectorExpr:
			id = inner.Sel
		}
	}
	if id == nil {
		return nil, false
	}
	obj, _ := pkg.TypesInfo.Uses[id].(*types.Func)
	if obj == nil || obj.Name() != "WithDerive" || obj.Pkg() == nil ||
		obj.Pkg().Path() != mamoriPkgPath {
		return nil, false
	}

	// T comes from the instantiated type arguments.
	inst, ok := pkg.TypesInfo.Instances[id]
	if !ok || inst.TypeArgs.Len() == 0 {
		return nil, false
	}
	named, ok := inst.TypeArgs.At(0).(*types.Named)
	if !ok || named.Obj().Pkg() == nil {
		return nil, false
	}
	return named, true
}

// derivesKey is the "pkgpath.TypeName" map key both findDerives and
// extract.go's Extract use for a config type: T's own package and name, so a
// call site living in a different package than T still keys by T's own
// package (see TestFindDerivesCrossPackage).
func derivesKey(named *types.Named) string {
	return named.Obj().Pkg().Path() + "." + named.Obj().Name()
}

// fieldTypeByPath resolves a dotted field path against t, mirroring core's
// decode.go reflect-based fieldByPath: split on ".", step into a nested
// struct field by name at each segment - the same shape a source: tag's own
// dotted Path and a WithDerive write path both use. It reports false for a
// path that does not resolve (an unknown field name, or a segment that is
// not itself a struct), the static-analysis equivalent of that same
// existence check.
func fieldTypeByPath(t types.Type, path string) (types.Type, bool) {
	cur := t
	for _, part := range strings.Split(path, ".") {
		st, ok := cur.Underlying().(*types.Struct)
		if !ok {
			return nil, false
		}
		next, found := fieldByName(st, part)
		if !found {
			return nil, false
		}
		cur = next
	}
	return cur, true
}

// fieldByName returns the type of st's field named name, and whether one
// exists.
func fieldByName(st *types.Struct, name string) (types.Type, bool) {
	for i := 0; i < st.NumFields(); i++ {
		f := st.Field(i)
		if f.Name() == name {
			return f.Type(), true
		}
	}
	return nil, false
}

// derivedFields converts findDerives's already-validated write paths for one
// struct into KindDerived Field entries: no source: tag (a derived field has
// no ref to list - see FieldKind's own doc comment) and GoType/Sensitive
// resolved from the field the path actually names, exactly as a plain
// scalar field's would be.
//
// walked is whatever walkFields (extract.go) already produced for the same
// struct, and a declared path that names one of those fields contributes
// nothing here: it is already described, more completely than a KindDerived
// entry ever could be. Declaring a write path for a field that ALSO carries a
// source: or validate: tag is legal and documented ("the derive runs after
// decoding and simply wins", site/src/pages/docs/usage/derived-fields.md), so
// this is the common case, not an edge one, and without the skip such a field
// is emitted twice: once by walkFields with its Default/HasDefault/Optional/
// Validate, and once here with none of them. schema.go's builderNode.insert is
// last-write-wins, so the second, emptier entry silently erased the real one -
// a defaulted, optional field lost its "default" and moved into "required",
// and a validate:"min=10" field lost its "minLength".
//
// This mirrors the decision core already made for the same overlap:
// reconciler.go's hasSpecPath, which report.go's derived-append loop and
// derivedFieldChanges both consult so a declared path that also names a real
// fieldSpec yields one entry, not two. paths is also deduplicated against
// itself, the way derivedFieldChanges's own seen map is, so two WithDerive
// calls naming the same path cannot produce two rows either.
func derivedFields(pkg *types.Package, st *types.Struct, paths []string, walked []Field) []Field {
	seen := make(map[string]struct{}, len(walked))
	for _, f := range walked {
		seen[f.Path] = struct{}{}
	}

	var fields []Field
	for _, p := range paths {
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		t, ok := fieldTypeByPath(st, p)
		if !ok {
			// findDerives already validates this against the same struct, so
			// this is defensive rather than expected; still drop, don't panic.
			continue
		}
		fields = append(fields, Field{
			Path:      p,
			Kind:      KindDerived,
			GoType:    types.TypeString(t, shortQualifier(pkg)),
			Sensitive: isSecretType(t),
		})
	}
	return fields
}
