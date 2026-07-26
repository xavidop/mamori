package server

import (
	"errors"
	"testing"

	"github.com/xavidop/mamori"
)

func TestAllowAllPermitsAnySubjectAndName(t *testing.T) {
	p := AllowAll()

	if err := p.Allow(mamori.Identity{Subject: "anyone"}, "anything"); err != nil {
		t.Fatalf("AllowAll denied a request: %v", err)
	}
	if err := p.Allow(mamori.Identity{}, ""); err != nil {
		t.Fatalf("AllowAll denied an empty identity/name: %v", err)
	}
}

func TestPrefixPolicyAllowsMatchingGlob(t *testing.T) {
	p := PrefixPolicy(map[string][]string{
		"svc": {"db-*"},
	})
	id := mamori.Identity{Subject: "svc"}

	if err := p.Allow(id, "db-password"); err != nil {
		t.Fatalf("expected allow for db-password, got: %v", err)
	}
}

func TestPrefixPolicyDeniesNonMatchingGlob(t *testing.T) {
	p := PrefixPolicy(map[string][]string{
		"svc": {"db-*"},
	})
	id := mamori.Identity{Subject: "svc"}

	err := p.Allow(id, "api-key")
	if err == nil {
		t.Fatal("expected deny for api-key, got nil error")
	}
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("expected ErrDenied, got: %v", err)
	}
}

func TestPrefixPolicyDeniesSubjectAbsentFromMap(t *testing.T) {
	p := PrefixPolicy(map[string][]string{
		"svc": {"db-*"},
	})

	err := p.Allow(mamori.Identity{Subject: "someone-else"}, "db-password")
	if err == nil {
		t.Fatal("expected deny for a subject with no rules, got nil error")
	}
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("expected ErrDenied, got: %v", err)
	}
}

// TestPrefixPolicyDenialIsNotADirectory verifies that a denial for a name
// that fails to match a listed subject's globs is byte-identical to a
// denial for a subject that is not in the rules map at all (and, separately,
// for a name that is not a real binding). Neither response may let a caller
// distinguish those cases, or the policy becomes a way to enumerate what
// exists.
func TestPrefixPolicyDenialIsNotADirectory(t *testing.T) {
	p := PrefixPolicy(map[string][]string{
		"svc": {"db-*"},
	})

	// Listed subject, name exists elsewhere but doesn't match this subject's
	// globs.
	errNonMatching := p.Allow(mamori.Identity{Subject: "svc"}, "api-key")
	// Subject entirely absent from the rules map, name is pure fiction.
	errUnknownSubject := p.Allow(mamori.Identity{Subject: "ghost"}, "totally-made-up-binding")

	if errNonMatching == nil || errUnknownSubject == nil {
		t.Fatalf("expected both denied, got %v / %v", errNonMatching, errUnknownSubject)
	}
	if errNonMatching.Error() != errUnknownSubject.Error() {
		t.Fatalf("denial errors differ, policy leaks existence: %q vs %q",
			errNonMatching.Error(), errUnknownSubject.Error())
	}
}

func TestPolicyFuncDelegates(t *testing.T) {
	var gotID mamori.Identity
	var gotName string
	p := PolicyFunc(func(id mamori.Identity, name string) error {
		gotID = id
		gotName = name
		return nil
	})

	if err := p.Allow(mamori.Identity{Subject: "x"}, "y"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotID.Subject != "x" || gotName != "y" {
		t.Fatalf("PolicyFunc did not delegate arguments: got id=%+v name=%q", gotID, gotName)
	}
}

func TestPolicyFuncPropagatesError(t *testing.T) {
	sentinel := errors.New("boom")
	p := PolicyFunc(func(mamori.Identity, string) error { return sentinel })

	if err := p.Allow(mamori.Identity{}, "n"); !errors.Is(err, sentinel) {
		t.Fatalf("expected sentinel error to propagate, got: %v", err)
	}
}
