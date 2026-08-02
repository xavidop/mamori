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
	"slices"
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
//
// The reveal is matched per write path, not per hook. A hook routinely writes
// several paths, and only some of them touch secret material: the safe pattern
// this very rule recommends (SafeDSN = secret.NewString(... Pass.Reveal()
// ...)) sitting in the same hook as an ordinary FullName = First + " " + Last
// is enough for a hook-scoped reveal to fire on FullName, at a position
// pointing at the SafeDSN line. A check that cries wolf gets turned off, so
// each declared path is judged only by the assignments that actually target
// it.
func checkDerives(pass *analysis.Pass, call *ast.CallExpr) {
	named, hook, writePaths, ok := withDeriveCall(pass, call)
	if !ok {
		return
	}

	writes := hookWrites(hook)
	if len(writes) == 0 {
		// The hook assigns nothing this rule can attribute to a write path
		// (an unnamed or "_" parameter, or every write routed through a
		// helper). Nothing to report; see hookWrites for the boundary.
		return
	}
	tainted := revealedLocals(pass, hook)

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
		for _, w := range writes[path] {
			if !revealsAny(pass, w.rhs, tainted) {
				continue
			}
			// Report at the assignment that launders, not at the first reveal
			// anywhere in the hook: with several paths written from one hook
			// those are different lines, and the reveal's line names the wrong
			// field.
			pass.Reportf(w.pos,
				"derive hook writes revealed secret material into %q, a plain %s; use secret.String or secret.Bytes",
				path, kind)
			break // one diagnostic per path, however many times it is written
		}
	}
}

// hookWrite is one assignment inside a hook that targets a declared write
// path: where to report, and the right-hand side expressions feeding it.
type hookWrite struct {
	pos token.Pos
	rhs []ast.Expr
}

// hookWrites maps every dotted path assigned on the hook's own parameter to
// the assignments that target it, so checkDerives can ask "does the assignment
// to THIS path carry revealed secret material" rather than "does this hook
// reveal anything at all".
//
// Only assignments written directly against the hook's parameter are seen
// (c.SafeDSN = ..., c.Nested.Field = ...). A write routed through a helper
// function or through a pointer alias produces no entry and therefore no
// diagnostic: the same "inline hook literal only" boundary withDeriveCall
// already draws, and the direction a static check should err in.
func hookWrites(hook *ast.FuncLit) map[string][]hookWrite {
	recv, ok := hookParamName(hook)
	if !ok {
		return nil
	}

	writes := make(map[string][]hookWrite)
	ast.Inspect(hook.Body, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for i, lhs := range assign.Lhs {
			path, ok := fieldPathOf(lhs, recv)
			if !ok {
				continue
			}
			// Paired assignment (a, b = x, y) feeds each path its own
			// expression; a single right-hand side (a, b = f()) feeds both.
			rhs := assign.Rhs
			if len(assign.Rhs) == len(assign.Lhs) {
				rhs = assign.Rhs[i : i+1]
			}
			writes[path] = append(writes[path], hookWrite{pos: assign.Pos(), rhs: rhs})
		}
		return true
	})
	return writes
}

// hookParamName returns the name of the hook's single *T parameter, the
// identifier every write path is rooted at. It reports false for a hook that
// does not name its parameter at all (func(*Config) error) or names it "_",
// neither of which can carry a field assignment.
func hookParamName(hook *ast.FuncLit) (string, bool) {
	if hook.Type == nil || hook.Type.Params == nil || len(hook.Type.Params.List) == 0 {
		return "", false
	}
	names := hook.Type.Params.List[0].Names
	if len(names) == 0 || names[0].Name == "_" {
		return "", false
	}
	return names[0].Name, true
}

// fieldPathOf renders expr as a dotted field path rooted at recv, the same
// "Redis.Password" shape a source: tag's Path and a WithDerive write path both
// use, and reports false for anything not rooted at recv. The hook parameter
// is a *T, so c.Field and (*c).Field are the same write and both resolve here.
func fieldPathOf(expr ast.Expr, recv string) (string, bool) {
	var parts []string
	cur := expr
	for {
		switch e := cur.(type) {
		case *ast.SelectorExpr:
			parts = append(parts, e.Sel.Name)
			cur = e.X
		case *ast.ParenExpr:
			cur = e.X
		case *ast.StarExpr:
			cur = e.X
		case *ast.Ident:
			if e.Name != recv || len(parts) == 0 {
				return "", false
			}
			slices.Reverse(parts)
			return strings.Join(parts, "."), true
		default:
			return "", false
		}
	}
}

// revealedLocals returns the local identifiers inside hook that hold revealed
// secret material, so the very common two-step shape
//
//	plain := c.Pass.Reveal()
//	c.PlainDSN = "postgres://" + plain + "@h/db"
//
// is still caught once the reveal is matched per assignment rather than per
// hook. ast.Inspect visits statements in source order, so a local is tainted
// before the assignment that consumes it is examined; this is a deliberately
// shallow heuristic, not a dataflow analysis, and a laundering path it cannot
// follow degrades to no diagnostic rather than to a wrong one.
func revealedLocals(pass *analysis.Pass, hook *ast.FuncLit) map[string]bool {
	tainted := make(map[string]bool)
	ast.Inspect(hook.Body, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for i, lhs := range assign.Lhs {
			id, ok := lhs.(*ast.Ident)
			if !ok || id.Name == "_" {
				continue
			}
			rhs := assign.Rhs
			if len(assign.Rhs) == len(assign.Lhs) {
				rhs = assign.Rhs[i : i+1]
			}
			if revealsAny(pass, rhs, tainted) {
				tainted[id.Name] = true
			}
		}
		return true
	})
	return tainted
}

// revealsAny reports whether any of exprs carries revealed secret material:
// either a Reveal/RevealBytes call on the secret package's own types, or a
// reference to a local already known to hold one (see revealedLocals).
func revealsAny(pass *analysis.Pass, exprs []ast.Expr, tainted map[string]bool) bool {
	found := false
	for _, expr := range exprs {
		ast.Inspect(expr, func(n ast.Node) bool {
			if found {
				return false
			}
			switch node := n.(type) {
			case *ast.CallExpr:
				if isRevealCall(pass, node) {
					found = true
					return false
				}
			case *ast.Ident:
				if tainted[node.Name] {
					found = true
					return false
				}
			}
			return true
		})
		if found {
			return true
		}
	}
	return false
}

// isRevealCall reports whether call is Reveal or RevealBytes on a type
// belonging to secretPkgPath.
func isRevealCall(pass *analysis.Pass, call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	if sel.Sel.Name != "Reveal" && sel.Sel.Name != "RevealBytes" {
		return false
	}

	recvType := pass.TypesInfo.TypeOf(sel.X)
	if recvType == nil {
		return false
	}
	named := namedReceiver(recvType)
	if named == nil || named.Obj().Pkg() == nil {
		// Matching name, wrong (or no) package: not the secret package's
		// Reveal - e.g. the fakeSecret fixture.
		return false
	}
	return named.Obj().Pkg().Path() == secretPkgPath
}

// namedReceiver unwraps t to the named type a method call resolves against:
// through an alias, and through one level of pointer, because Go dereferences
// automatically and a *secret.String field reaches String's value-receiver
// Reveal exactly as a secret.String field does. Asserting *types.Named on the
// receiver directly missed that shape.
func namedReceiver(t types.Type) *types.Named {
	t = types.Unalias(t)
	if ptr, ok := t.(*types.Pointer); ok {
		t = types.Unalias(ptr.Elem())
	}
	named, _ := t.(*types.Named)
	return named
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
