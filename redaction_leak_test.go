package mamori

import (
	"context"
	"strings"
	"testing"

	"github.com/xavidop/mamori/secret"
)

// refWithInlineCredential is the shape these guards exist for: some providers
// accept a credential as a query option, so Ref.Raw can carry a live secret.
func refWithInlineCredential(t *testing.T) Ref {
	t.Helper()
	ref, err := ParseRef("pg://host/db?password=hunter2&sslmode=require")
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	if !strings.Contains(ref.Raw, "hunter2") {
		t.Fatalf("fixture is wrong: Raw does not carry the credential: %q", ref.Raw)
	}
	return ref
}

// TestRefRedactedHidesInlineCredential pins the exported redaction a Meter,
// Tracer, or middleware outside this package relies on.
func TestRefRedactedHidesInlineCredential(t *testing.T) {
	got := refWithInlineCredential(t).Redacted()
	if strings.Contains(got, "hunter2") {
		t.Fatalf("Redacted() leaked the credential: %q", got)
	}
	if !strings.Contains(got, secret.Redacted) {
		t.Fatalf("Redacted() = %q, want the redaction placeholder", got)
	}
	// Non-sensitive options must survive, or the ref stops being diagnostic.
	if !strings.Contains(got, "sslmode=require") {
		t.Fatalf("Redacted() dropped a non-sensitive option: %q", got)
	}
}

// TestRefRedactedMatchesReportRedaction pins that the exported method and the
// redaction a Report already applies cannot drift apart.
func TestRefRedactedMatchesReportRedaction(t *testing.T) {
	ref := refWithInlineCredential(t)
	if ref.Redacted() != redactRef(ref) {
		t.Fatalf("Redacted() = %q, redactRef() = %q", ref.Redacted(), redactRef(ref))
	}
}

// TestTracerNeverSeesAnInlineCredential is the regression guard for a span
// attribute leaving the process with a live secret in it.
func TestTracerNeverSeesAnInlineCredential(t *testing.T) {
	ref := refWithInlineCredential(t)
	tr := &capturingTracer{}
	o := defaultOptions()
	o.tracer = tr
	o.providers[ref.Scheme] = staticProvider{scheme: ref.Scheme, val: []byte("v")}

	_, _ = resolveRef(t.Context(), ref, o)

	if tr.ref == "" {
		t.Fatal("tracer was never called; the guard would not catch a regression")
	}
	if strings.Contains(tr.ref, "hunter2") {
		t.Fatalf("tracer received an unredacted ref: %q", tr.ref)
	}
}

type capturingTracer struct {
	ref string
}

func (c *capturingTracer) StartResolve(ctx context.Context, scheme, ref string) (context.Context, func(error)) {
	c.ref = ref
	return ctx, func(error) {}
}

type staticProvider struct {
	scheme string
	val    []byte
}

func (s staticProvider) Scheme() string { return s.scheme }
func (s staticProvider) Resolve(ctx context.Context, ref Ref) (Value, error) {
	return Value{Bytes: s.val}, nil
}
