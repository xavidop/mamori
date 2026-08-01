package mamori

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestArmReentrancyCallSitesMatchReentrantCallbacks is the structural pin for
// the bug that kept recurring across seven review rounds on this feature: a
// doc comment or a doc page enumerating the callbacks that run inline on the
// reconciler goroutine drifting out of sync with how many actually do, after
// WithDerive joined PreApply and OnError as a third one. Every one of those
// rounds fixed the single instance it was shown and missed the others,
// because prose has nothing tying it back to the code.
//
// If you are adding a fourth callback that runs inline on the reconciler
// goroutine (arming armReentrancy around it, the way runPreApply,
// buildCandidate's derive loop, and emitErr already do): this test is the one
// that is supposed to fail on you. Add its name to reentrantCallbacks
// (errors.go), and then go update every doc comment and doc page that
// enumerates the callbacks - this test cannot do that part for you.
//
// The mechanism: parse reconciler.go and preapply.go and count call
// expressions whose function is the identifier armReentrancy - not a count of
// lines or a grep for the text "armReentrancy(", so a call spread over
// several lines counts once and the word appearing in a comment (as it does
// throughout this very file) counts zero, since comments are not part of the
// parsed syntax tree at all. That count is compared against
// len(reentrantCallbacks), the single named list ErrReentrantCall's message
// is itself built from (see errors.go). A mismatch in either direction - a
// call site added with no matching name, or a name added with no matching
// call site - fails here.
//
// What this deliberately does NOT catch: it would not have caught any of
// rounds two through seven as they actually happened. Every one of those was
// a doc comment or a markdown page whose prose said "two callbacks" (or left
// WithDerive out of an enumeration) while the code already had three correct
// armReentrancy call sites - the call-site count never drifted from
// reentrantCallbacks in any of those rounds, only the prose did, in files
// this test does not read. This test only fires when the CODE's call-site
// count and reentrantCallbacks actually disagree; it guards the next
// callback's addition (round eight and beyond), not the seven rounds of pure
// prose entropy that came before it.
func TestArmReentrancyCallSitesMatchReentrantCallbacks(t *testing.T) {
	got := countArmReentrancyCallSites(t, "reconciler.go") + countArmReentrancyCallSites(t, "preapply.go")
	if want := len(reentrantCallbacks); got != want {
		t.Fatalf("found %d armReentrancy(...) call site(s) across reconciler.go and preapply.go, want %d (len(reentrantCallbacks)): "+
			"a callback that runs inline on the reconciler goroutine was added or removed on one side without the other. "+
			"Update reentrantCallbacks (errors.go) to name the real set of inline callbacks, and update every doc comment "+
			"and doc page that enumerates them - this test only pins the count, not the prose.", got, want)
	}
}

// countArmReentrancyCallSites parses file (a path relative to this package's
// own directory, which is where `go test` runs with its working directory
// set) and returns the number of call expressions invoking the identifier
// armReentrancy.
func countArmReentrancyCallSites(t *testing.T, file string) int {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", file, err)
	}
	count := 0
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == "armReentrancy" {
			count++
		}
		return true
	})
	return count
}

// TestJoinCallbackList pins joinCallbackList's shape directly, independent of
// ErrReentrantCall's wording, so a future edit to the message's surrounding
// text cannot accidentally hide a break in the enumeration logic itself.
func TestJoinCallbackList(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{nil, ""},
		{[]string{"a"}, "a"},
		{[]string{"a", "b"}, "a or b"},
		{[]string{"a", "b", "c"}, "a, b, or c"},
		{[]string{"a", "b", "c", "d"}, "a, b, c, or d"},
	}
	for _, c := range cases {
		if got := joinCallbackList(c.in); got != c.want {
			t.Errorf("joinCallbackList(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}
