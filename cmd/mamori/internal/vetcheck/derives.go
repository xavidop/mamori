// This file implements vetcheck's second rule: a WithDerive hook that reveals
// a secret.String/secret.Bytes and writes the plaintext into a declared write
// path naming a plain string or []byte field. That field carries no source:
// tag at all (a derived field never does - see cmd/mamori/extract.go's
// KindDerived), so checkField's tag-based rule above cannot see it; a hook
// doing exactly this is how a secret gets laundered past that check.
//
// It deliberately duplicates roughly the callee-matching shape
// cmd/mamori/derives.go's withDeriveTypeArg already has, rather than
// importing it: that file's Extract does its own packages.Load, which
// unitchecker (the go vet driver's one-package-at-a-time protocol vetcheck
// must also run under, via `go vet -vettool=$(which mamori)`) cannot support.
// See this package's doc comment in analyzer.go for the fuller explanation.
package vetcheck

import (
	"go/ast"
	"go/token"
	"go/types"
	"strconv"
	"strings"

	"golang.org/x/tools/go/analysis"
)

// mamoriPkgPath is github.com/xavidop/mamori's own import path: the package a
// WithDerive callee must belong to for withDeriveCall to accept it, so a
// local function merely named WithDerive (or a WithDerive belonging to some
// unrelated package) is never mistaken for it.
const mamoriPkgPath = "github.com/xavidop/mamori"

// secretPkgPath is github.com/xavidop/mamori/secret's own import path. A
// Reveal/RevealBytes call only counts as revealing secret material when its
// receiver's named type belongs to this exact package path - matched by
// path, not by identity with the real package's *types.Package. The
// analysistest fixtures vendor a stub at this same import path
// (testdata/src/github.com/xavidop/mamori/secret), so a check that only
// recognized the real package would pass in production yet never fire in
// its own tests; matching by path keeps both the same check.
const secretPkgPath = "github.com/xavidop/mamori/secret"

// checkDerives inspects call for a mamori.WithDerive invocation whose hook
// reveals a secret and launders the plaintext into a declared write path
// naming a plain string or []byte field, reporting a diagnostic for each
// such path found.
func checkDerives(pass *analysis.Pass, call *ast.CallExpr) {
	named, hook, writePaths, ok := withDeriveCall(pass, call)
	if !ok {
		return
	}

	revealPos, revealed := findReveal(pass, hook)
	if !revealed {
		// No Reveal/RevealBytes call anywhere in the hook body: no secret
		// material can have moved, however this hook shuffles fields around
		// (e.g. FullName = First + " " + Last). Nothing to report.
		return
	}

	for _, path := range writePaths {
		fieldType, ok := fieldTypeByPath(named, path)
		if !ok {
			// Names no field on T: not this rule's concern - the same
			// "declared but unresolvable" case cmd/mamori/derives.go's
			// findDerives drops rather than flags.
			continue
		}
		kind, isPlain := plainSecretKind(fieldType)
		if !isPlain {
			// Already secret.String / secret.Bytes: writing a revealed
			// secret back into a redacting type is exactly the safe pattern
			// (see the SafeDSN fixture), not laundering.
			continue
		}
		pass.Reportf(revealPos,
			"derive hook writes revealed secret material into %q, a plain %s; use secret.String or secret.Bytes",
			path, kind)
	}
}

// withDeriveCall reports whether call is a call to mamori.WithDerive[T],
// returning T itself (as a *types.Named, so fieldTypeByPath can walk its
// fields), the hook literal passed as its first argument, and the literal
// write paths declared after it.
//
// Resolve the callee through types, never by name text, exactly as
// cmd/mamori/derives.go's withDeriveTypeArg does - see this file's doc
// comment for why that matcher is duplicated here rather than imported.
func withDeriveCall(pass *analysis.Pass, call *ast.CallExpr) (named *types.Named, hook *ast.FuncLit, writePaths []string, ok bool) {
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
		return nil, nil, nil, false
	}

	obj, _ := pass.TypesInfo.Uses[id].(*types.Func)
	if obj == nil || obj.Name() != "WithDerive" || obj.Pkg() == nil ||
		obj.Pkg().Path() != mamoriPkgPath {
		return nil, nil, nil, false
	}

	// T comes from the instantiated type arguments.
	inst, hasInst := pass.TypesInfo.Instances[id]
	if !hasInst || inst.TypeArgs.Len() == 0 {
		return nil, nil, nil, false
	}
	named, ok = inst.TypeArgs.At(0).(*types.Named)
	if !ok || named.Obj().Pkg() == nil {
		return nil, nil, nil, false
	}

	if len(call.Args) == 0 {
		return nil, nil, nil, false
	}
	// Only an inline hook literal is walked for a Reveal call - a hook passed
	// by name (WithDerive(buildDSN, ...)) is out of scope for this rule, the
	// same "literal" boundary the brief itself draws.
	hook, ok = call.Args[0].(*ast.FuncLit)
	if !ok {
		return nil, nil, nil, false
	}

	for _, a := range call.Args[1:] {
		tv, hasType := pass.TypesInfo.Types[a]
		if !hasType || tv.Value == nil {
			// Non-literal write path (a variable, a spread slice): this rule
			// only ever matches a literal write path against a field, so a
			// non-literal one simply contributes nothing to check, same as
			// cmd/mamori/derives.go marks it "incomplete" rather than guessing.
			continue
		}
		// tv.Value.String() is quoted; unquote to get the path itself.
		path, err := strconv.Unquote(tv.Value.String())
		if err != nil {
			continue
		}
		writePaths = append(writePaths, path)
	}

	return named, hook, writePaths, true
}

// findReveal walks hook's body for a call to Reveal or RevealBytes whose
// receiver's named type belongs to secretPkgPath, returning the position of
// the first such call found and true. It returns false if no such call
// exists anywhere in the body.
func findReveal(pass *analysis.Pass, hook *ast.FuncLit) (pos token.Pos, found bool) {
	ast.Inspect(hook.Body, func(n ast.Node) bool {
		if found {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if sel.Sel.Name != "Reveal" && sel.Sel.Name != "RevealBytes" {
			return true
		}

		recvType := pass.TypesInfo.TypeOf(sel.X)
		if recvType == nil {
			return true
		}
		recvNamed, ok := recvType.(*types.Named)
		if !ok || recvNamed.Obj().Pkg() == nil ||
			recvNamed.Obj().Pkg().Path() != secretPkgPath {
			// Matching name, wrong (or no) package: not the secret package's
			// Reveal - e.g. the fakeSecret fixture. Keep walking; some other
			// call in the body may still be the real thing.
			return true
		}

		pos = call.Pos()
		found = true
		return false
	})
	return pos, found
}

// fieldTypeByPath resolves a dotted field path against t, mirroring
// cmd/mamori/derives.go's own fieldTypeByPath (itself mirroring core's
// decode.go reflect-based fieldByPath): step into a nested struct field by
// name at each "."-separated segment. Duplicated here for the same reason
// mamoriPkgPath is - see this file's doc comment.
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
