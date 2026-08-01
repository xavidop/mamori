package cloudflarekv

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/xavidop/mamori"
)

// TestSchemeIsRegistered pins Step 0: without mamori.Register(New()) in
// init, import _ ".../providers/cloudflare-kv" never wires the scheme into
// the global registry and the documented usage is a lie.
func TestSchemeIsRegistered(t *testing.T) {
	found := false
	for _, s := range mamori.RegisteredSchemes() {
		if s == scheme {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("scheme %q is not registered; got %v", scheme, mamori.RegisteredSchemes())
	}
}

func TestResolveReturnsRawBytes(t *testing.T) {
	f := newFake()
	f.set(testNamespace, "log-level", []byte("debug"))
	p := f.provider()

	v, err := p.Resolve(context.Background(), ref(t, "cloudflare-kv://log-level"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(v.Bytes) != "debug" {
		t.Fatalf("got %q, want %q", v.Bytes, "debug")
	}
}

// TestResolveKeyWithSlashesIsEscapedNotSplit is the load-bearing test of the
// ref-grammar decision: keyOf takes the whole ref path as one key, and that
// decision only works end to end if Resolve sends the key as a single
// url.PathEscape'd path segment. A value-only assertion would not catch a
// missing escape, because both an escaped and an unescaped slash produce a
// path this fake's lenient parser can still resolve to the right value; only
// asserting the literal escaped bytes on the wire catches it.
func TestResolveKeyWithSlashesIsEscapedNotSplit(t *testing.T) {
	f := newFake()
	f.set(testNamespace, "config/prod/log-level", []byte("info"))
	p := f.provider()

	v, err := p.Resolve(context.Background(), ref(t, "cloudflare-kv://config/prod/log-level"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(v.Bytes) != "info" {
		t.Fatalf("got %q, want %q", v.Bytes, "info")
	}

	f.mu.Lock()
	path := f.lastPath
	f.mu.Unlock()
	wantSuffix := "/accounts/" + testAccount + "/storage/kv/namespaces/" + testNamespace + "/values/config%2Fprod%2Flog-level"
	if !strings.HasSuffix(path, wantSuffix) {
		t.Fatalf("request path %q does not end with the escaped key %q", path, wantSuffix)
	}
}

// TestResolveKeysWithSpecialCharactersRoundTrip covers keys containing ':',
// '%', and a space. This provider does not validate a key's charset itself;
// it forwards whatever key the ref names to url.PathEscape and lets
// Cloudflare accept or reject it. This test pins that PathEscape round-trips
// these characters correctly through the request URL rather than the
// provider mangling or truncating them somewhere along the way.
func TestResolveKeysWithSpecialCharactersRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		key  string
	}{
		{name: "colon", key: "namespace:config"},
		{name: "percent", key: "100%-done"},
		{name: "space", key: "log level"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			want := "value-for-" + tc.name
			f.set(testNamespace, tc.key, []byte(want))
			p := f.provider()

			v, err := p.Resolve(context.Background(), ref(t, "cloudflare-kv://"+tc.key))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if string(v.Bytes) != want {
				t.Fatalf("got %q, want %q", v.Bytes, want)
			}
		})
	}
}

func TestResolveNamespaceOptionOverridesDefault(t *testing.T) {
	f := newFake()
	f.set("other-namespace", "k", []byte("from-other-ns"))
	p := f.provider()

	v, err := p.Resolve(context.Background(), ref(t, "cloudflare-kv://k?namespace=other-namespace"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(v.Bytes) != "from-other-ns" {
		t.Fatalf("got %q, want %q", v.Bytes, "from-other-ns")
	}

	f.mu.Lock()
	path := f.lastPath
	f.mu.Unlock()
	wantSuffix := "/accounts/" + testAccount + "/storage/kv/namespaces/other-namespace/values/k"
	if !strings.HasSuffix(path, wantSuffix) {
		t.Fatalf("request path %q does not end with %q; the ref's ?namespace= must win", path, wantSuffix)
	}
}

// TestResolveNamespaceWithSlashIsEscaped pins that s.namespace, like the key,
// travels through url.PathEscape before it is built into the request path.
// The namespace is ref-controlled through ?namespace=, so without the escape
// a namespace value containing "/" - or a full path-traversal payload such as
// "..%2F..%2F..%2Fzones%2Fevil%2Fpurge" - would produce a request path that
// escapes the intended /storage/kv/namespaces/<id>/... endpoint entirely.
// TestResolveNamespaceOptionOverridesDefault uses a plain namespace needing no
// escaping and cannot catch a missing escape; only asserting the literal
// escaped bytes on the wire (as TestResolveKeyWithSlashesIsEscapedNotSplit
// already does for the key) catches it, because a fake server decodes an
// unescaped slash right back to the same path components.
func TestResolveNamespaceWithSlashIsEscaped(t *testing.T) {
	f := newFake()
	const evilNamespace = "abc/def"
	f.set(evilNamespace, "k", []byte("v"))
	p := f.provider()

	v, err := p.Resolve(context.Background(), ref(t, "cloudflare-kv://k?namespace="+evilNamespace))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(v.Bytes) != "v" {
		t.Fatalf("got %q, want %q", v.Bytes, "v")
	}

	f.mu.Lock()
	path := f.lastPath
	f.mu.Unlock()
	wantSubstr := "/storage/kv/namespaces/abc%2Fdef/values/k"
	if !strings.Contains(path, wantSubstr) {
		t.Fatalf("request path %q does not contain the escaped namespace %q; the namespace must be url.PathEscape'd before it reaches the request URL", path, wantSubstr)
	}
}

func TestResolveAbsentKeyIsNotFound(t *testing.T) {
	f := newFake()
	p := f.provider()

	_, err := p.Resolve(context.Background(), ref(t, "cloudflare-kv://missing"))
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("got %v, want an error satisfying mamori.ErrNotFound", err)
	}
}

func TestResolveSelectsFieldAndPointer(t *testing.T) {
	f := newFake()
	f.set(testNamespace, "api-config", []byte(`{"timeout":"5s","nested":{"deep":"found"}}`))
	p := f.provider()

	v, err := p.Resolve(context.Background(), ref(t, "cloudflare-kv://api-config#timeout"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(v.Bytes) != "5s" {
		t.Fatalf("got %q, want %q", v.Bytes, "5s")
	}

	v, err = p.Resolve(context.Background(), ref(t, "cloudflare-kv://api-config#/nested/deep"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(v.Bytes) != "found" {
		t.Fatalf("got %q, want %q", v.Bytes, "found")
	}
}

// TestResolveSelectingFieldOfNonObjectIsInvalid: Workers KV stores opaque
// bytes, so a stored value is under no obligation to be JSON at all.
// Selecting a #field from one that is not a JSON object is a malformed
// request against that payload, not an absence, so it must fail with
// mamori.ErrInvalid rather than being swallowed like a not-found.
func TestResolveSelectingFieldOfNonObjectIsInvalid(t *testing.T) {
	f := newFake()
	f.set(testNamespace, "log-level", []byte("info"))
	p := f.provider()

	_, err := p.Resolve(context.Background(), ref(t, "cloudflare-kv://log-level#timeout"))
	if !errors.Is(err, mamori.ErrInvalid) {
		t.Fatalf("got %v, want an error satisfying mamori.ErrInvalid", err)
	}
}

// TestResolveValueShape pins Value.Sensitive, Value.Version, and
// Value.Metadata all at once: Sensitive must always be false (Workers KV
// values are configuration, not a managed secret), Version must be the
// content hash of the resolved bytes (there is no native revision id to use
// instead), and Metadata must carry the namespace but never the account id
// or the resolved value itself.
func TestResolveValueShape(t *testing.T) {
	f := newFake()
	const secretLooking = "sensitive-looking-value"
	f.set(testNamespace, "k", []byte(secretLooking))
	p := f.provider()

	v, err := p.Resolve(context.Background(), ref(t, "cloudflare-kv://k"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.Sensitive {
		t.Error("Value.Sensitive must always be false")
	}
	if v.Version != mamori.VersionHash(v.Bytes) {
		t.Errorf("got Version %q, want mamori.VersionHash of the resolved bytes", v.Version)
	}
	if v.Metadata["namespace"] != testNamespace {
		t.Errorf("got Metadata[namespace] %q, want %q", v.Metadata["namespace"], testNamespace)
	}
	for k, val := range v.Metadata {
		if strings.Contains(val, secretLooking) {
			t.Errorf("Metadata[%q] = %q contains the resolved value", k, val)
		}
		if strings.Contains(val, testAccount) {
			t.Errorf("Metadata[%q] = %q contains the account id", k, val)
		}
	}
}

func TestResolveSendsBearerTokenNeverInURL(t *testing.T) {
	f := newFake()
	f.set(testNamespace, "k", []byte("v"))
	p := f.provider()

	if _, err := p.Resolve(context.Background(), ref(t, "cloudflare-kv://k")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	f.mu.Lock()
	auth, path := f.lastAuth, f.lastPath
	f.mu.Unlock()
	if auth != "Bearer "+testToken {
		t.Fatalf("got Authorization %q, want %q", auth, "Bearer "+testToken)
	}
	if strings.Contains(path, testToken) {
		t.Fatalf("token leaked into the request path: %q", path)
	}
}

func TestResolveHonorsContextCancellation(t *testing.T) {
	f := newFake()
	f.set(testNamespace, "k", []byte("v"))
	p := f.provider()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := p.Resolve(ctx, ref(t, "cloudflare-kv://k")); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}

// TestResolveClassifiesFailureStatus exercises classifyStatus through the
// full Resolve path, using failStatus, rather than only unit-testing
// classifyStatus in isolation (errors_test.go does that).
func TestResolveClassifiesFailureStatus(t *testing.T) {
	tests := []struct {
		code int
		want error
	}{
		{http.StatusUnauthorized, mamori.ErrUnauthenticated},
		{http.StatusForbidden, mamori.ErrPermissionDenied},
		{http.StatusTooManyRequests, mamori.ErrRateLimited},
		{http.StatusBadRequest, mamori.ErrInvalid},
		{http.StatusInternalServerError, mamori.ErrUnavailable},
	}
	for _, tc := range tests {
		t.Run(http.StatusText(tc.code), func(t *testing.T) {
			f := newFake()
			f.set(testNamespace, "k", []byte("v"))
			f.failStatus(testNamespace, tc.code)
			p := f.provider()

			_, err := p.Resolve(context.Background(), ref(t, "cloudflare-kv://k"))
			if !errors.Is(err, tc.want) {
				t.Fatalf("status %d: got %v, want an error satisfying %v", tc.code, err, tc.want)
			}
		})
	}
}

// TestResolveNeverCaches pins the hard constraint that this provider holds no
// snapshot at all: three resolves of an unchanged key must cost three GETs
// (unlike providers/vercel-gc, which is allowed to cost one), and a key
// deleted between resolves must be observed as gone on the very next call,
// with no held value masking it.
func TestResolveNeverCaches(t *testing.T) {
	f := newFake()
	f.set(testNamespace, "k", []byte("v1"))
	p := f.provider()
	ctx := context.Background()

	for i := range 3 {
		v, err := p.Resolve(ctx, ref(t, "cloudflare-kv://k"))
		if err != nil {
			t.Fatalf("resolve %d: unexpected error: %v", i, err)
		}
		if string(v.Bytes) != "v1" {
			t.Fatalf("resolve %d: got %q, want %q", i, v.Bytes, "v1")
		}
	}
	get, _ := f.counts()
	if get != 3 {
		t.Fatalf("got %d GET requests for 3 resolves, want 3: a cache would hold at 1", get)
	}

	f.del(testNamespace, "k")
	_, err := p.Resolve(ctx, ref(t, "cloudflare-kv://k"))
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("got %v after a live delete, want mamori.ErrNotFound: a cache would still serve the deleted value", err)
	}
	get, _ = f.counts()
	if get != 4 {
		t.Fatalf("got %d GET requests, want 4 (the delete-observing resolve must have hit the network)", get)
	}
}

// erroringTransport always fails at the transport level, the way a real
// network error (a refused connection, a timeout, a DNS failure) would.
type erroringTransport struct{}

func (erroringTransport) RoundTrip(_ *http.Request) (*http.Response, error) {
	return nil, errors.New("connection refused")
}

// secretToken, secretAccount, and secretNamespace are shared by every
// credential-leak test below so a mismatched constant in one of them can
// never silently narrow what a test actually checks for.
const (
	secretToken     = "super-secret-token"
	secretAccount   = "super-secret-account-id"
	secretNamespace = "super-secret-namespace-id"
)

// assertNoCredentialLeak fails t if err's text contains the token, the
// account id, or the namespace id configured via secretToken, secretAccount,
// and secretNamespace. None of the three is a URL-safe fixture value, so a
// substring match cannot pass by accident.
func assertNoCredentialLeak(t *testing.T, err error) {
	t.Helper()
	if strings.Contains(err.Error(), secretToken) {
		t.Fatalf("error leaked the API token: %v", err)
	}
	if strings.Contains(err.Error(), secretAccount) {
		t.Fatalf("error leaked the account id: %v", err)
	}
	if strings.Contains(err.Error(), secretNamespace) {
		t.Fatalf("error leaked the namespace id: %v", err)
	}
}

// TestResolveTransportErrorNeverLeaksCredentials pins the regression
// providers/vercel-gc shipped: a live token, account id, or namespace id must
// never reach an error message. vercel-gc's leak went through url.Parse on a
// connection string; this provider has no connection string, but its request
// URL embeds the account id and namespace id, and http.Client.Do wraps every
// transport-level failure in a *url.Error whose Error() renders the full
// request URL. Without sanitizing that, an ordinary network hiccup - not
// even a bug in this provider - would put the account id and namespace id
// into a returned error's text.
//
// This is one of four sanitizeTransportError call sites (the
// NewRequestWithContext branch and the httpClient.Do branch, in each of get
// and bulkGet); the other three are pinned by
// TestResolveBatchTransportErrorNeverLeaksCredentials,
// TestResolveMalformedBaseURLNeverLeaksCredentials, and
// TestResolveBatchMalformedBaseURLNeverLeaksCredentials below.
func TestResolveTransportErrorNeverLeaksCredentials(t *testing.T) {
	p := New(
		WithAPIToken(secretToken),
		WithAccountID(secretAccount),
		WithNamespaceID(secretNamespace),
		WithHTTPClient(&http.Client{Transport: erroringTransport{}}),
	)

	_, err := p.Resolve(context.Background(), ref(t, "cloudflare-kv://k"))
	if err == nil {
		t.Fatal("want an error from a failing transport")
	}
	assertNoCredentialLeak(t, err)
}

// TestResolveBatchTransportErrorNeverLeaksCredentials is
// TestResolveTransportErrorNeverLeaksCredentials's counterpart for
// bulkGet's httpClient.Do branch: ResolveBatch, not Resolve, is the only
// caller that reaches it, and nothing before this covered it directly - the
// existing leak test only ever drove Resolve.
func TestResolveBatchTransportErrorNeverLeaksCredentials(t *testing.T) {
	p := New(
		WithAPIToken(secretToken),
		WithAccountID(secretAccount),
		WithNamespaceID(secretNamespace),
		WithHTTPClient(&http.Client{Transport: erroringTransport{}}),
	)

	_, err := p.ResolveBatch(context.Background(), []mamori.Ref{ref(t, "cloudflare-kv://k")})
	if err == nil {
		t.Fatal("want an error from a failing transport")
	}
	assertNoCredentialLeak(t, err)
}

// TestResolveMalformedBaseURLNeverLeaksCredentials pins the
// NewRequestWithContext branch in get, the one sanitizeTransportError call
// site nothing previously exercised: a malformed WithBaseURL makes
// url.Parse fail inside http.NewRequestWithContext itself, before any
// network call, and that failure is also a *url.Error whose Error() renders
// the full (unreachable) request URL - unsanitized, it would read
// `parse "http://example.com/%zz/accounts/<account>/storage/kv/namespaces/<namespace>/values/k": ...`.
func TestResolveMalformedBaseURLNeverLeaksCredentials(t *testing.T) {
	p := New(
		WithAPIToken(secretToken),
		WithAccountID(secretAccount),
		WithNamespaceID(secretNamespace),
		WithBaseURL("http://example.com/%zz"),
	)

	_, err := p.Resolve(context.Background(), ref(t, "cloudflare-kv://k"))
	if err == nil {
		t.Fatal("want an error from a malformed base URL")
	}
	assertNoCredentialLeak(t, err)
}

// TestResolveBatchMalformedBaseURLNeverLeaksCredentials is
// TestResolveMalformedBaseURLNeverLeaksCredentials's counterpart for
// bulkGet's NewRequestWithContext branch.
func TestResolveBatchMalformedBaseURLNeverLeaksCredentials(t *testing.T) {
	p := New(
		WithAPIToken(secretToken),
		WithAccountID(secretAccount),
		WithNamespaceID(secretNamespace),
		WithBaseURL("http://example.com/%zz"),
	)

	_, err := p.ResolveBatch(context.Background(), []mamori.Ref{ref(t, "cloudflare-kv://k")})
	if err == nil {
		t.Fatal("want an error from a malformed base URL")
	}
	assertNoCredentialLeak(t, err)
}

// TestGetErrorBodyIsBounded pins errBodyLimit on get's single-key path: the
// diagnostic read in its non-200, non-404 branch must never let a hostile or
// broken upstream put an unbounded response into an error string. Every
// other test in this file serves bodies far smaller than the bound, so none
// of them would notice if io.LimitReader(resp.Body, errBodyLimit) were
// replaced with resp.Body directly; this one sends a body far larger than
// the bound with a distinctive trailing marker, so an unbounded read would
// carry the marker straight into the returned error.
func TestGetErrorBodyIsBounded(t *testing.T) {
	const marker = "TAIL_MARKER_MUST_NOT_SURVIVE"
	oversized := strings.Repeat("A", 20000) + marker

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, oversized)
	}))
	defer srv.Close()

	p := New(WithAPIToken(testToken), WithAccountID(testAccount), WithNamespaceID(testNamespace), WithBaseURL(srv.URL))

	_, err := p.Resolve(context.Background(), ref(t, "cloudflare-kv://k"))
	if err == nil {
		t.Fatal("want an error for a 500 response")
	}
	if strings.Contains(err.Error(), marker) {
		t.Fatalf("error body was not bounded: the trailing marker reached the error text: %v", err)
	}
	if len(err.Error()) > 10000 {
		t.Fatalf("error message is %d bytes long; the diagnostic read must be bounded well below the %d-byte oversized body", len(err.Error()), len(oversized))
	}
}

// TestBulkGetErrorBodyIsBounded is TestGetErrorBodyIsBounded's counterpart
// for bulkGet's own, separate errBodyLimit read: get and bulkGet each build
// their own statusErr from their own LimitReader call, so a mutation
// dropping the bound from one would not be caught by a test that only
// exercises the other.
func TestBulkGetErrorBodyIsBounded(t *testing.T) {
	const marker = "TAIL_MARKER_MUST_NOT_SURVIVE"
	oversized := strings.Repeat("A", 20000) + marker

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, oversized)
	}))
	defer srv.Close()

	p := New(WithAPIToken(testToken), WithAccountID(testAccount), WithNamespaceID(testNamespace), WithBaseURL(srv.URL))

	_, err := p.ResolveBatch(context.Background(), []mamori.Ref{ref(t, "cloudflare-kv://k")})
	if err == nil {
		t.Fatal("want an error for a 500 response")
	}
	if strings.Contains(err.Error(), marker) {
		t.Fatalf("error body was not bounded: the trailing marker reached the error text: %v", err)
	}
	if len(err.Error()) > 10000 {
		t.Fatalf("error message is %d bytes long; the diagnostic read must be bounded well below the %d-byte oversized body", len(err.Error()), len(oversized))
	}
}
