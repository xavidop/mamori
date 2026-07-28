package server

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xavidop/mamori"
)

// stubAuth is a minimal Authenticator for tests that only need "some
// Authenticator is configured", not any particular authentication behavior.
type stubAuth struct{}

func (stubAuth) Authenticate(r *http.Request) (mamori.Identity, error) {
	return mamori.Identity{Subject: "stub"}, nil
}

func TestNewErrorsWithoutPolicy(t *testing.T) {
	_, err := New(NoAuth())
	if err == nil {
		t.Fatal("expected New to error without a Policy")
	}
}

func TestNewErrorsWithoutAuthOrNoAuth(t *testing.T) {
	_, err := New(WithPolicy(AllowAll()))
	if err == nil {
		t.Fatal("expected New to error with neither WithAuth nor NoAuth")
	}
}

func TestNewOKWithPolicyAndNoAuth(t *testing.T) {
	s, err := New(WithPolicy(AllowAll()), NoAuth())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s == nil {
		t.Fatal("expected a non-nil Server")
	}
}

func TestNewOKWithPolicyAndAuth(t *testing.T) {
	s, err := New(WithPolicy(AllowAll()), WithAuth(stubAuth{}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s == nil {
		t.Fatal("expected a non-nil Server")
	}
}

// TestNewErrorsWithNoAuthAndTCP exercises the NoAuth-refused-on-TCP gate
// using withTCPListener, the unexported stand-in for Task 5's real TCP(...)
// Option (which does not exist yet in this task). See withTCPListener's doc
// comment in server.go for why this is structured this way.
func TestNewErrorsWithNoAuthAndTCP(t *testing.T) {
	_, err := New(WithPolicy(AllowAll()), NoAuth(), withTCPListener())
	if err == nil {
		t.Fatal("expected New to error when NoAuth is combined with a TCP listener")
	}
}

func TestNewOKWithAuthAndTCP(t *testing.T) {
	// The gate is specifically NoAuth+TCP; a real Authenticator plus TCP is
	// fine.
	_, err := New(WithPolicy(AllowAll()), WithAuth(stubAuth{}), withTCPListener())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestBindExecRejectedWithoutAllowExec(t *testing.T) {
	_, err := New(
		WithPolicy(AllowAll()),
		NoAuth(),
		Bind("x", "exec:echo hi"),
	)
	if err == nil {
		t.Fatal("expected New to reject an exec: binding without AllowExec()")
	}
}

func TestBindExecAllowedWithAllowExec(t *testing.T) {
	s, err := New(
		WithPolicy(AllowAll()),
		NoAuth(),
		Bind("x", "exec:echo hi"),
		AllowExec(),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := s.bindings["x"]; !ok {
		t.Fatal("expected binding \"x\" to be present")
	}
}

// TestBindExecUppercaseSchemeRejectedWithoutAllowExec exercises the
// case-insensitive scheme gate (bindings.go's resolveBindings lowercases
// ref.Scheme before matching): an "EXEC:" ref must be gated exactly like an
// "exec:" one, not slip through unmatched because the switch used to compare
// ref.Scheme verbatim.
func TestBindExecUppercaseSchemeRejectedWithoutAllowExec(t *testing.T) {
	_, err := New(
		WithPolicy(AllowAll()),
		NoAuth(),
		Bind("x", "EXEC:echo hi"),
	)
	if err == nil {
		t.Fatal("expected New to reject an EXEC: binding without AllowExec()")
	}
}

func TestBindMamoriRejectedWithoutAllowChaining(t *testing.T) {
	_, err := New(
		WithPolicy(AllowAll()),
		NoAuth(),
		Bind("y", "mamori://other-server/db-password"),
	)
	if err == nil {
		t.Fatal("expected New to reject a mamori: binding without AllowChaining()")
	}
}

func TestBindMamoriAllowedWithAllowChaining(t *testing.T) {
	s, err := New(
		WithPolicy(AllowAll()),
		NoAuth(),
		Bind("y", "mamori://other-server/db-password"),
		AllowChaining(),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := s.bindings["y"]; !ok {
		t.Fatal("expected binding \"y\" to be present")
	}
}

// TestBindMamoriMixedCaseSchemeRejectedWithoutAllowChaining is the mamori:
// counterpart to TestBindExecUppercaseSchemeRejectedWithoutAllowExec, using a
// mixed-case scheme ("Mamori:") to prove the lowercasing is not merely an
// all-uppercase special case.
func TestBindMamoriMixedCaseSchemeRejectedWithoutAllowChaining(t *testing.T) {
	_, err := New(
		WithPolicy(AllowAll()),
		NoAuth(),
		Bind("y", "Mamori://other-server/db-password"),
	)
	if err == nil {
		t.Fatal("expected New to reject a Mamori: binding without AllowChaining()")
	}
}

func TestBindExecAllowExecOrderIndependent(t *testing.T) {
	// AllowExec appears BEFORE Bind here, the opposite order of
	// TestBindExecAllowedWithAllowExec, to prove the gate does not depend on
	// option order (see resolveBindings's doc comment in bindings.go).
	_, err := New(
		WithPolicy(AllowAll()),
		NoAuth(),
		AllowExec(),
		Bind("x", "exec:echo hi"),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestBindDecodeOptionRejected covers the ?decode= gate in resolveBindings.
// The server resolves bindings through mamori.WatchRef, which yields the
// provider's raw Update with no decode pipeline anywhere in the path, so a
// binding carrying ?decode= would serve still-encoded bytes to every client
// without an error - a wrong value rather than a failure. It is refused at
// construction, like the exec:/mamori: schemes above.
func TestBindDecodeOptionRejected(t *testing.T) {
	_, err := New(
		WithPolicy(AllowAll()),
		NoAuth(),
		Bind("tls-key", "aws-sm://prod/tls#key?decode=base64"),
	)
	if err == nil {
		t.Fatal("expected New to reject a binding carrying ?decode=")
	}
	// The error has to name the alternative that does work, since the whole
	// point of rejecting is that the operator wanted the decoding to happen
	// somewhere: on the client's own mamori:// ref, where core applies it.
	if !strings.Contains(err.Error(), "mamori://tls-key?decode=base64") {
		t.Fatalf("expected the error to name the client-side alternative, got: %v", err)
	}
}

// TestBindEmptyDecodeOptionRejected pins the gate to the option's presence
// rather than to it having a non-empty value: "?decode=" is a no-op in core,
// but accepting it here would tell an operator this server understands an
// option it never applies.
func TestBindEmptyDecodeOptionRejected(t *testing.T) {
	_, err := New(
		WithPolicy(AllowAll()),
		NoAuth(),
		Bind("tls-key", "aws-sm://prod/tls#key?decode="),
	)
	if err == nil {
		t.Fatal("expected New to reject a binding carrying a bare ?decode=")
	}
}

func TestBindFileDecodeOptionRejected(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "bindings.yaml")
	content := "bindings:\n  tls-key: aws-sm://prod/tls#key?decode=base64\n"
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("failed to write bind file: %v", err)
	}

	_, err := New(
		WithPolicy(AllowAll()),
		NoAuth(),
		BindFile(p),
	)
	if err == nil {
		t.Fatal("expected New to reject a bind file entry carrying ?decode=")
	}
}

// TestBindOtherQueryOptionsAccepted is the negative half of the ?decode=
// gate: only decode is refused, so a pointer fragment (which providers apply
// themselves, inside Resolve, via mamori.SelectKey) and ordinary
// provider-specific options still bind.
func TestBindOtherQueryOptionsAccepted(t *testing.T) {
	s, err := New(
		WithPolicy(AllowAll()),
		NoAuth(),
		Bind("db-user", "aws-sm://prod/db#/credentials/user?region=eu-west-1"),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	b, ok := s.bindings["db-user"]
	if !ok {
		t.Fatal("expected binding \"db-user\" to be present")
	}
	if b.Ref.Key != "/credentials/user" {
		t.Fatalf("expected the pointer fragment to survive binding, got %q", b.Ref.Key)
	}
}

func TestDuplicateBindingNameRejected(t *testing.T) {
	_, err := New(
		WithPolicy(AllowAll()),
		NoAuth(),
		Bind("dup", "env:A"),
		Bind("dup", "env:B"),
	)
	if err == nil {
		t.Fatal("expected New to reject a duplicate binding name")
	}
}

func TestBindFileParsesYAMLBindingsMap(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "bindings.yaml")
	content := "bindings:\n  db-password: env:DB_PASSWORD\n  api-key: env:API_KEY\n"
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("failed to write bind file: %v", err)
	}

	s, err := New(
		WithPolicy(AllowAll()),
		NoAuth(),
		BindFile(p),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(s.bindings) != 2 {
		t.Fatalf("expected 2 bindings, got %d: %+v", len(s.bindings), s.bindings)
	}
	if _, ok := s.bindings["db-password"]; !ok {
		t.Fatal("expected binding \"db-password\" from bind file")
	}
	if _, ok := s.bindings["api-key"]; !ok {
		t.Fatal("expected binding \"api-key\" from bind file")
	}
}

func TestBindFileAndBindCombineAndStillDetectDuplicates(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "bindings.yaml")
	content := "bindings:\n  db-password: env:DB_PASSWORD\n"
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("failed to write bind file: %v", err)
	}

	_, err := New(
		WithPolicy(AllowAll()),
		NoAuth(),
		Bind("db-password", "env:OTHER"),
		BindFile(p),
	)
	if err == nil {
		t.Fatal("expected New to reject a name duplicated between Bind and BindFile")
	}
}

func TestBindFileMissingFileErrors(t *testing.T) {
	_, err := New(
		WithPolicy(AllowAll()),
		NoAuth(),
		BindFile(filepath.Join(t.TempDir(), "does-not-exist.yaml")),
	)
	if err == nil {
		t.Fatal("expected New to error when the bind file does not exist")
	}
}
