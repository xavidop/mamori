package cloudflarekv

import (
	"context"
	"errors"
	"net/http"
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

// TestResolveTransportErrorNeverLeaksCredentials pins the regression
// providers/vercel-gc shipped: a live token or account id must never reach an
// error message. vercel-gc's leak went through url.Parse on a connection
// string; this provider has no connection string, but its request URL embeds
// the account id and namespace id, and http.Client.Do wraps every
// transport-level failure in a *url.Error whose Error() renders the full
// request URL. Without sanitizing that, an ordinary network hiccup - not
// even a bug in this provider - would put the account id into a returned
// error's text.
func TestResolveTransportErrorNeverLeaksCredentials(t *testing.T) {
	const secretToken = "super-secret-token"
	const secretAccount = "super-secret-account-id"
	p := New(
		WithAPIToken(secretToken),
		WithAccountID(secretAccount),
		WithNamespaceID("ns"),
		WithHTTPClient(&http.Client{Transport: erroringTransport{}}),
	)

	_, err := p.Resolve(context.Background(), ref(t, "cloudflare-kv://k"))
	if err == nil {
		t.Fatal("want an error from a failing transport")
	}
	if strings.Contains(err.Error(), secretToken) {
		t.Fatalf("error leaked the API token: %v", err)
	}
	if strings.Contains(err.Error(), secretAccount) {
		t.Fatalf("error leaked the account id: %v", err)
	}
}
