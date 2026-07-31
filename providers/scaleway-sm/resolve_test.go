package scalewaysm

import (
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"net/http"
	"strings"
	"testing"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/secret"
)

// TestSchemeIsRegistered pins Step 0: without mamori.Register(New()) in
// init, import _ ".../providers/scaleway-sm" never wires the scheme into
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

func TestResolveDecodesSecretAndSendsQueryParams(t *testing.T) {
	f := newFakeSM()
	f.set("/prod", "db-password", []byte("hunter2"))
	p := f.provider()

	v, err := p.Resolve(context.Background(), ref(t, "scaleway-sm://prod/db-password"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(v.Bytes) != "hunter2" {
		t.Fatalf("got %q, want %q", v.Bytes, "hunter2")
	}

	f.mu.Lock()
	q := f.lastQuery
	f.mu.Unlock()
	if got := q.Get("secret_path"); got != "/prod" {
		t.Fatalf("got secret_path %q, want %q", got, "/prod")
	}
	if got := q.Get("secret_name"); got != "db-password" {
		t.Fatalf("got secret_name %q, want %q", got, "db-password")
	}
	if got := q.Get("project_id"); got != testProjectID {
		t.Fatalf("got project_id %q, want %q", got, testProjectID)
	}
}

func TestResolveSendsAuthTokenHeaderNeverInURL(t *testing.T) {
	f := newFakeSM()
	f.set("/", "k", []byte("v"))
	p := f.provider()

	if _, err := p.Resolve(context.Background(), ref(t, "scaleway-sm://k")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	f.mu.Lock()
	auth, path, q := f.lastAuthHeader, f.lastPath, f.lastQuery
	f.mu.Unlock()

	if auth != testSecretKey {
		t.Fatalf("got X-Auth-Token %q, want %q", auth, testSecretKey)
	}
	if strings.Contains(path, testSecretKey) {
		t.Fatalf("secret key leaked into the request path: %q", path)
	}
	for k, vals := range q {
		for _, val := range vals {
			if strings.Contains(val, testSecretKey) {
				t.Fatalf("secret key leaked into query param %q: %q", k, val)
			}
		}
	}
}

// TestResolveBase64FidelityRoundTripsNonUTF8Bytes pins that accessResponse's
// Data []byte field needs no manual base64 decode step: encoding/json
// base64-decodes into it for free, and this must round-trip a payload that
// is not valid UTF-8, not merely a plain ASCII string a manual string-based
// decode could paper over.
func TestResolveBase64FidelityRoundTripsNonUTF8Bytes(t *testing.T) {
	f := newFakeSM()
	payload := []byte{0x00, 0xFF, 0xFE, 'a', 0x80, 0x81, 0x00, 'z'}
	f.set("/", "binary-secret", payload)
	p := f.provider()

	v, err := p.Resolve(context.Background(), ref(t, "scaleway-sm://binary-secret"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(v.Bytes) != string(payload) {
		t.Fatalf("got %v, want %v byte-for-byte", v.Bytes, payload)
	}
}

// TestResolveVersionIsRevisionNotContentHash is the load-bearing test for
// this module's entire reason for existing: two responses with
// byte-identical payloads at different revisions must produce different
// Version values. A content-hash implementation (mamori.VersionHash(bytes),
// as every sibling provider in this stack uses) cannot pass this, because it
// depends only on the bytes, which are identical here by construction.
func TestResolveVersionIsRevisionNotContentHash(t *testing.T) {
	f := newFakeSM()
	const identical = "identical-payload-at-two-different-revisions"
	f.setRevision("/", "s", 3, []byte(identical), nil, true)
	f.setRevision("/", "s", 4, []byte(identical), nil, true)
	p := f.provider()

	v3, err := p.Resolve(context.Background(), ref(t, "scaleway-sm://s?revision=3"))
	if err != nil {
		t.Fatalf("resolving revision 3: %v", err)
	}
	v4, err := p.Resolve(context.Background(), ref(t, "scaleway-sm://s?revision=4"))
	if err != nil {
		t.Fatalf("resolving revision 4: %v", err)
	}

	if string(v3.Bytes) != identical || string(v4.Bytes) != identical {
		t.Fatalf("payloads must be byte-identical by construction: got %q and %q", v3.Bytes, v4.Bytes)
	}
	if v3.Version == v4.Version {
		t.Fatalf("Version must differ between revisions carrying identical bytes (both were %q); "+
			"a content-hash implementation cannot pass this, which is exactly the point of this test", v3.Version)
	}
	if v3.Version != "3" || v4.Version != "4" {
		t.Fatalf("got Version %q and %q, want the exact revision numbers %q and %q", v3.Version, v4.Version, "3", "4")
	}
}

// TestResolveVersionStaysRevisionEvenWithFieldSelection pins the doc comment
// on valueFor: a #field selection narrows Value.Bytes but must not change
// Version, because Version identifies which write produced the underlying
// secret, not which slice of its JSON was returned.
func TestResolveVersionStaysRevisionEvenWithFieldSelection(t *testing.T) {
	f := newFakeSM()
	f.setRevision("/", "api-config", 7, []byte(`{"timeout":"5s"}`), nil, true)
	p := f.provider()

	v, err := p.Resolve(context.Background(), ref(t, "scaleway-sm://api-config#timeout"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(v.Bytes) != "5s" {
		t.Fatalf("got %q, want %q", v.Bytes, "5s")
	}
	if v.Version != "7" {
		t.Fatalf("got Version %q, want the secret's revision %q even though #timeout narrowed the bytes", v.Version, "7")
	}
}

// TestResolveValueSensitiveAndRedactsViaSecretString pins Value.Sensitive
// (true, unlike every sibling provider in this trio, since this is a real
// secret manager) and demonstrates the practical consequence: wrapping the
// resolved bytes in secret.String must redact under fmt.
func TestResolveValueSensitiveAndRedactsViaSecretString(t *testing.T) {
	f := newFakeSM()
	const payload = "hunter2"
	f.set("/", "k", []byte(payload))
	p := f.provider()

	v, err := p.Resolve(context.Background(), ref(t, "scaleway-sm://k"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !v.Sensitive {
		t.Fatal("Value.Sensitive must be true: this provider reads a secret manager, not a config store")
	}

	s := secret.NewStringBytes(v.Bytes)
	if got := fmt.Sprintf("%v", s); got != secret.Redacted {
		t.Fatalf("secret.String wrapping the resolved value rendered %q under fmt, want the redaction placeholder %q", got, secret.Redacted)
	}
	if got := s.String(); strings.Contains(got, payload) {
		t.Fatalf("secret.String leaked the resolved value under fmt: %q", got)
	}
}

// TestResolveMetadataOnlyRegionAndRevision pins Value.Metadata's entire
// contents: region and revision, and nothing else. Never the secret id,
// never the project id, never the path, never the value - a secret's
// location is itself information, and Metadata reaches the admin HTTP
// endpoint and the status report.
func TestResolveMetadataOnlyRegionAndRevision(t *testing.T) {
	f := newFakeSM()
	const path, name = "/prod", "db-password"
	const payload = "super-secret-payload-value"
	f.setRevision(path, name, 3, []byte(payload), nil, true)
	p := f.provider()

	v, err := p.Resolve(context.Background(), ref(t, "scaleway-sm://prod/db-password"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(v.Metadata) != 2 {
		t.Fatalf("got Metadata %v with %d entries, want exactly region and revision", v.Metadata, len(v.Metadata))
	}
	if v.Metadata["region"] != testRegion {
		t.Fatalf("got Metadata[region] %q, want %q", v.Metadata["region"], testRegion)
	}
	if v.Metadata["revision"] != "3" {
		t.Fatalf("got Metadata[revision] %q, want %q", v.Metadata["revision"], "3")
	}
	for k, val := range v.Metadata {
		if strings.Contains(val, payload) {
			t.Errorf("Metadata[%q] = %q contains the resolved value", k, val)
		}
		if strings.Contains(val, testProjectID) {
			t.Errorf("Metadata[%q] = %q contains the project id", k, val)
		}
		if strings.Contains(val, path) {
			t.Errorf("Metadata[%q] = %q contains the secret path", k, val)
		}
		if strings.Contains(val, fakeSecretID) {
			t.Errorf("Metadata[%q] = %q contains the secret id", k, val)
		}
	}
}

func TestResolveCRCMatchResolves(t *testing.T) {
	f := newFakeSM()
	data := []byte("crc-checked-payload")
	sum := crc32.ChecksumIEEE(data)
	f.setRevision("/", "k", 1, data, &sum, true)
	p := f.provider()

	v, err := p.Resolve(context.Background(), ref(t, "scaleway-sm://k"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(v.Bytes) != string(data) {
		t.Fatalf("got %q, want %q", v.Bytes, data)
	}
}

func TestResolveCRCMismatchIsInvalid(t *testing.T) {
	f := newFakeSM()
	data := []byte("crc-checked-payload")
	wrong := crc32.ChecksumIEEE(data) + 1
	f.setRevision("/", "k", 1, data, &wrong, true)
	p := f.provider()

	_, err := p.Resolve(context.Background(), ref(t, "scaleway-sm://k"))
	if !errors.Is(err, mamori.ErrInvalid) {
		t.Fatalf("got %v, want an error satisfying mamori.ErrInvalid", err)
	}
}

// TestResolveCRCAbsentIsNotAnError pins that a nil data_crc32 is normal, not
// an error: Scaleway populates it only when a CRC was supplied at write
// time, so its absence must resolve exactly as if no verification were
// requested at all.
func TestResolveCRCAbsentIsNotAnError(t *testing.T) {
	f := newFakeSM()
	f.setRevision("/", "k", 1, []byte("no-crc-supplied"), nil, true)
	p := f.provider()

	v, err := p.Resolve(context.Background(), ref(t, "scaleway-sm://k"))
	if err != nil {
		t.Fatalf("an absent data_crc32 must not be treated as an error: %v", err)
	}
	if string(v.Bytes) != "no-crc-supplied" {
		t.Fatalf("got %q", v.Bytes)
	}
}

// TestResolveLatestEnabledSkipsDisabledRevision is the honest test of this
// module's most important design decision: with revision 4 disabled and
// revision 3 enabled, a ref naming no revision (defaulting to
// "latest_enabled") must resolve revision 3, while ?revision=latest must
// still reach revision 4 regardless of its disabled state. Both the
// resolved Version and the revision selector that reached the request URL
// are asserted, so this catches either a wrong default or a Resolve that
// never actually threads the selector through to the wire.
func TestResolveLatestEnabledSkipsDisabledRevision(t *testing.T) {
	f := newFakeSM()
	f.setRevision("/", "cred", 3, []byte("v3"), nil, true)
	f.setRevision("/", "cred", 4, []byte("v4"), nil, true)
	f.disable("/", "cred", 4)
	p := f.provider()

	v, err := p.Resolve(context.Background(), ref(t, "scaleway-sm://cred"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(v.Bytes) != "v3" || v.Version != "3" {
		t.Fatalf("default ref must resolve the newest ENABLED revision (3), got bytes %q version %q", v.Bytes, v.Version)
	}
	f.mu.Lock()
	path := f.lastPath
	f.mu.Unlock()
	if !strings.Contains(path, "/versions/latest_enabled/") {
		t.Fatalf("request path %q must carry the latest_enabled selector when the ref names no revision", path)
	}

	v, err = p.Resolve(context.Background(), ref(t, "scaleway-sm://cred?revision=latest"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(v.Bytes) != "v4" || v.Version != "4" {
		t.Fatalf("?revision=latest must resolve the newest revision REGARDLESS of enabled state (4), got bytes %q version %q", v.Bytes, v.Version)
	}
	f.mu.Lock()
	path = f.lastPath
	f.mu.Unlock()
	if !strings.Contains(path, "/versions/latest/") {
		t.Fatalf("request path %q must carry the latest selector", path)
	}
}

func TestResolveSelectsFieldAndPointer(t *testing.T) {
	f := newFakeSM()
	f.set("/", "api-config", []byte(`{"timeout":"5s","nested":{"deep":"found"}}`))
	p := f.provider()

	v, err := p.Resolve(context.Background(), ref(t, "scaleway-sm://api-config#timeout"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(v.Bytes) != "5s" {
		t.Fatalf("got %q, want %q", v.Bytes, "5s")
	}

	v, err = p.Resolve(context.Background(), ref(t, "scaleway-sm://api-config#/nested/deep"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(v.Bytes) != "found" {
		t.Fatalf("got %q, want %q", v.Bytes, "found")
	}
}

func TestResolveUnknownSecretIsNotFound(t *testing.T) {
	f := newFakeSM()
	p := f.provider()

	_, err := p.Resolve(context.Background(), ref(t, "scaleway-sm://missing"))
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("got %v, want an error satisfying mamori.ErrNotFound", err)
	}
}

func TestResolveHonorsContextCancellation(t *testing.T) {
	f := newFakeSM()
	f.set("/", "k", []byte("v"))
	p := f.provider()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := p.Resolve(ctx, ref(t, "scaleway-sm://k")); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}

// TestResolveNeverCaches pins the hard constraint from the module's design
// (no time-based caching, TTL, or held snapshot): repeated resolves of an
// unchanged secret must cost one request each, and a secret that starts
// failing between resolves must be observed as failing on the very next
// call, with no held value masking it. clearFail then restores ordinary
// service and a further resolve must reach the network again rather than
// replaying anything cached from before the failure.
func TestResolveNeverCaches(t *testing.T) {
	f := newFakeSM()
	f.set("/", "k", []byte("v1"))
	p := f.provider()
	ctx := context.Background()

	for i := range 3 {
		v, err := p.Resolve(ctx, ref(t, "scaleway-sm://k"))
		if err != nil {
			t.Fatalf("resolve %d: unexpected error: %v", i, err)
		}
		if string(v.Bytes) != "v1" {
			t.Fatalf("resolve %d: got %q, want %q", i, v.Bytes, "v1")
		}
	}
	if got := f.counts(); got != 3 {
		t.Fatalf("got %d requests for 3 resolves, want 3: a cache would hold at 1", got)
	}

	f.failStatus("/", "k", http.StatusServiceUnavailable)
	if _, err := p.Resolve(ctx, ref(t, "scaleway-sm://k")); !errors.Is(err, mamori.ErrUnavailable) {
		t.Fatalf("got %v after a live failure injection, want mamori.ErrUnavailable: a cache would still serve the last-good value", err)
	}
	if got := f.counts(); got != 4 {
		t.Fatalf("got %d requests, want 4 (the failure-observing resolve must have hit the network)", got)
	}

	f.clearFail("/", "k")
	v, err := p.Resolve(ctx, ref(t, "scaleway-sm://k"))
	if err != nil {
		t.Fatalf("unexpected error after clearFail: %v", err)
	}
	if string(v.Bytes) != "v1" {
		t.Fatalf("got %q after clearFail, want %q", v.Bytes, "v1")
	}
	if got := f.counts(); got != 5 {
		t.Fatalf("got %d requests, want 5 (clearFail must not be served from a cache either)", got)
	}
}

// TestResolveClassifiesFailureStatus exercises classifyStatus through the
// full Resolve path, using the fake's failStatus injection, rather than only
// unit-testing classifyStatus in isolation (errors_test.go does that). This
// is also the shape Task 3's conformance Fail hook needs: fail one secret's
// requests by status code, resolve, observe the classified error.
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
			f := newFakeSM()
			f.set("/", "k", []byte("v"))
			f.failStatus("/", "k", tc.code)
			p := f.provider()

			_, err := p.Resolve(context.Background(), ref(t, "scaleway-sm://k"))
			if !errors.Is(err, tc.want) {
				t.Fatalf("status %d: got %v, want an error satisfying %v", tc.code, err, tc.want)
			}
		})
	}
}

// erroringTransport always fails at the transport level, the way a real
// network error (a refused connection, a timeout, a DNS failure) would.
type erroringTransport struct{}

func (erroringTransport) RoundTrip(_ *http.Request) (*http.Response, error) {
	return nil, errors.New("connection refused")
}

// secretLeakKey and secretLeakProjectID are shared by both credential-leak
// tests below so a mismatched constant in one of them can never silently
// narrow what a test actually checks for. Neither is a URL-safe value one
// would type by accident, so a substring match cannot pass by chance.
const (
	secretLeakKey       = "super-secret-scw-key"
	secretLeakProjectID = "super-secret-project-id"
)

// assertNoCredentialLeak fails t if err's text contains the secret key or the
// project id configured via secretLeakKey and secretLeakProjectID.
func assertNoCredentialLeak(t *testing.T, err error) {
	t.Helper()
	if strings.Contains(err.Error(), secretLeakKey) {
		t.Fatalf("error leaked the API secret key: %v", err)
	}
	if strings.Contains(err.Error(), secretLeakProjectID) {
		t.Fatalf("error leaked the project id: %v", err)
	}
}

// TestResolveTransportErrorNeverLeaksCredentials pins the httpClient.Do
// sanitizeTransportError call site in access: this provider's request URL
// carries the project id in its query string (see access's doc comment),
// and http.Client.Do wraps every transport-level failure in a *url.Error
// whose Error() renders the full request URL. Without sanitizing that, an
// ordinary network hiccup - not even a bug in this provider - would put the
// project id into a returned error's text.
//
// This is one of the two sanitizeTransportError call sites in access (this
// one, and the NewRequestWithContext branch pinned by
// TestResolveMalformedBaseURLNeverLeaksCredentials below); unlike
// providers/cloudflare-kv, this provider has no batch endpoint and so no
// second function duplicating the pair.
func TestResolveTransportErrorNeverLeaksCredentials(t *testing.T) {
	p := New(
		WithSecretKey(secretLeakKey),
		WithProjectID(secretLeakProjectID),
		WithRegion(testRegion),
		WithHTTPClient(&http.Client{Transport: erroringTransport{}}),
	)

	_, err := p.Resolve(context.Background(), ref(t, "scaleway-sm://k"))
	if err == nil {
		t.Fatal("want an error from a failing transport")
	}
	assertNoCredentialLeak(t, err)
}

// TestResolveMalformedBaseURLNeverLeaksCredentials pins the
// NewRequestWithContext branch in access, the other sanitizeTransportError
// call site: a malformed WithBaseURL makes url.Parse fail inside
// http.NewRequestWithContext itself, before any network call, and that
// failure is also a *url.Error whose Error() renders the full (unreachable)
// request URL - unsanitized, it would read
// `parse "http://example.com/%zz/regions/.../secrets-by-path/versions/.../access?...project_id=<projectID>...": ...`.
// access assembles the query string (including project_id) into the URL
// BEFORE calling NewRequestWithContext specifically so this failure mode is
// real rather than theoretical.
func TestResolveMalformedBaseURLNeverLeaksCredentials(t *testing.T) {
	p := New(
		WithSecretKey(secretLeakKey),
		WithProjectID(secretLeakProjectID),
		WithRegion(testRegion),
		WithBaseURL("http://example.com/%zz"),
	)

	_, err := p.Resolve(context.Background(), ref(t, "scaleway-sm://k"))
	if err == nil {
		t.Fatal("want an error from a malformed base URL")
	}
	assertNoCredentialLeak(t, err)
}
