package https

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/xavidop/mamori"
)

// newTestProvider builds a Provider over f with the given endpoint options.
func newTestProvider(t *testing.T, f *fakeBackend, mutate func(*Endpoint)) *Provider {
	t.Helper()
	e := Endpoint{
		Name:    "billing",
		BaseURL: "https://api.test/v1",
		Client:  &http.Client{Transport: f.transport()},
	}
	if mutate != nil {
		mutate(&e)
	}
	p, err := New(e)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

// mustRef parses a ref or fails the test.
func mustRef(t *testing.T, s string) mamori.Ref {
	t.Helper()
	r, err := mamori.ParseRef(s)
	if err != nil {
		t.Fatalf("ParseRef(%q): %v", s, err)
	}
	return r
}

func TestResolveReturnsBody(t *testing.T) {
	f := newFake()
	f.set("/v1/cfg", []byte(`{"region":"eu-west-1"}`))
	p := newTestProvider(t, f, nil)

	v, err := p.Resolve(context.Background(), mustRef(t, "https://billing/cfg"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != `{"region":"eu-west-1"}` {
		t.Fatalf("Bytes = %q", v.Bytes)
	}
	if v.Version == "" {
		t.Fatal("Version is empty; change detection needs one")
	}
}

func TestResolveSelectsWithPointer(t *testing.T) {
	f := newFake()
	f.set("/v1/cfg", []byte(`{"db":{"pass":"s3cr3t"}}`))
	p := newTestProvider(t, f, nil)

	v, err := p.Resolve(context.Background(), mustRef(t, "https://billing/cfg#/db/pass"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "s3cr3t" {
		t.Fatalf("Bytes = %q, want s3cr3t", v.Bytes)
	}
}

func TestResolveSelectsTopLevelKey(t *testing.T) {
	f := newFake()
	f.set("/v1/cfg", []byte(`{"log.level":"debug"}`))
	p := newTestProvider(t, f, nil)

	v, err := p.Resolve(context.Background(), mustRef(t, "https://billing/cfg#log.level"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "debug" {
		t.Fatalf("Bytes = %q, want debug", v.Bytes)
	}
}

func TestResolveUnknownEndpoint(t *testing.T) {
	f := newFake()
	p := newTestProvider(t, f, nil)

	_, err := p.Resolve(context.Background(), mustRef(t, "https://nope/cfg"))
	if !errors.Is(err, mamori.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
	// An unknown endpoint must NOT be ErrNotFound: that would silently apply the
	// field's default instead of reporting the misconfiguration.
	if errors.Is(err, mamori.ErrNotFound) {
		t.Fatal("unknown endpoint reported as ErrNotFound, which would hide it behind a default")
	}
}

func TestResolveMissingValueIsNotFound(t *testing.T) {
	f := newFake()
	p := newTestProvider(t, f, nil)

	_, err := p.Resolve(context.Background(), mustRef(t, "https://billing/absent"))
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestResolveMergesEndpointQueryAndHeader(t *testing.T) {
	f := newFake()
	f.set("/v1/cfg", []byte(`{}`))
	p := newTestProvider(t, f, func(e *Endpoint) {
		e.Query = url.Values{"env": {"prod"}}
		e.Header = http.Header{"X-Tenant": {"acme"}}
	})

	if _, err := p.Resolve(context.Background(), mustRef(t, "https://billing/cfg")); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if f.lastQuery != "env=prod" {
		t.Fatalf("query = %q, want env=prod", f.lastQuery)
	}
	if got := f.lastHeader.Get("X-Tenant"); got != "acme" {
		t.Fatalf("X-Tenant = %q, want acme", got)
	}
}

func TestResolveMarksSensitive(t *testing.T) {
	f := newFake()
	f.set("/v1/cfg", []byte("s3cr3t"))
	p := newTestProvider(t, f, func(e *Endpoint) { e.Sensitive = true })

	v, err := p.Resolve(context.Background(), mustRef(t, "https://billing/cfg"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !v.Sensitive {
		t.Fatal("Sensitive = false, want true for a Sensitive endpoint")
	}
}

func TestResolveUsesConditionalGetOnSecondCall(t *testing.T) {
	f := newFake()
	f.set("/v1/cfg", []byte("payload"))
	p := newTestProvider(t, f, nil)

	first, err := p.Resolve(context.Background(), mustRef(t, "https://billing/cfg"))
	if err != nil {
		t.Fatalf("first Resolve: %v", err)
	}
	second, err := p.Resolve(context.Background(), mustRef(t, "https://billing/cfg"))
	if err != nil {
		t.Fatalf("second Resolve: %v", err)
	}
	if f.lastHeader.Get("If-None-Match") == "" {
		t.Fatal("second Resolve sent no If-None-Match; the poll is not conditional")
	}
	if string(second.Bytes) != string(first.Bytes) {
		t.Fatalf("second Bytes = %q, want the cached %q", second.Bytes, first.Bytes)
	}
	if second.Version != first.Version {
		t.Fatalf("Version changed across an unmodified poll: %q then %q", first.Version, second.Version)
	}
}

// TestResolveRejectsDotSegments pins that a ref path cannot escape the path
// prefix its endpoint declares.
//
// This is reachable, not theoretical: expandRefVars substitutes ${VAR} from
// WithRefVars, whose values the application supplies at runtime, so a ref of
// https://billing/${TENANT}/cfg carries whatever TENANT holds. For an endpoint
// scoped to a tenant prefix, "../.." reaches another tenant's configuration
// without ever leaving the declared host, so the endpoint check that exists to
// contain exactly this never fires.
func TestResolveRejectsDotSegments(t *testing.T) {
	for _, path := range []string{"../secrets", "a/../../b", "./cfg", "a/./b"} {
		t.Run(path, func(t *testing.T) {
			f := newFake()
			p := newTestProvider(t, f, nil)

			_, err := p.Resolve(context.Background(), mustRef(t, "https://billing/"+path))
			if !errors.Is(err, mamori.ErrInvalid) {
				t.Fatalf("Resolve(%q) err = %v, want ErrInvalid", path, err)
			}
			if errors.Is(err, mamori.ErrNotFound) {
				t.Fatalf("Resolve(%q) reported ErrNotFound, which would hide the traversal behind a field default", path)
			}
		})
	}
}

// TestResolveDoesNotDecodeEscapedDotSegments pins the other half: a
// percent-encoded traversal needs no separate check, because ParseRef does not
// decode escapes and url.URL.String re-encodes the percent sign. The request
// must reach the backend as an ordinary, non-traversing path.
func TestResolveDoesNotDecodeEscapedDotSegments(t *testing.T) {
	f := newFake()
	p := newTestProvider(t, f, nil)

	// Not found is the expected outcome: the point is that it is treated as a
	// literal key rather than resolved into a parent directory.
	_, err := p.Resolve(context.Background(), mustRef(t, "https://billing/%2e%2e/secrets"))
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if strings.Contains(f.lastPath, "../") {
		t.Fatalf("request path %q traversed; the escape was decoded somewhere", f.lastPath)
	}
}

// TestResolvePassesThroughUnknownOptions pins the DecodeOption conformance
// requirement: decoding is core's job, so the provider must not touch ?decode=.
func TestResolvePassesThroughUnknownOptions(t *testing.T) {
	f := newFake()
	f.set("/v1/cfg", []byte("cGF5bG9hZA=="))
	p := newTestProvider(t, f, nil)

	v, err := p.Resolve(context.Background(), mustRef(t, "https://billing/cfg?decode=base64"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "cGF5bG9hZA==" {
		t.Fatalf("Bytes = %q; the provider decoded the value, which is core's job", v.Bytes)
	}
}
