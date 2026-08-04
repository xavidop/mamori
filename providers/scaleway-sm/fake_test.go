package scalewaysm

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
)

const (
	testSecretKey = "test-secret-key"
	testProjectID = "test-project-id"
	testRegion    = "test-region"
)

// Path markers the fake uses to parse the URL Resolve builds:
//
//	{base}/secrets-by-path/versions/{revision}/access?secret_path=&secret_name=&project_id=
//
// where {base} already ends in ".../regions/{region}" once defaultBaseURL's
// own path is included. Matched with strings.Index/Cut, the same way
// providers/cloudflare-kv's fake parses its URL, rather than assuming the
// marker starts at position zero.
const (
	pathRegionsMarker  = "/regions/"
	pathVersionsMarker = "/secrets-by-path/versions/"
	pathAccessSuffix   = "/access"
)

// secretRevision is one stored, numbered revision of a secret: its payload,
// the CRC supplied at write time (nil when none was), and whether it is
// currently enabled.
type secretRevision struct {
	data    []byte
	crc     *uint32
	enabled bool
}

// fakeSecretID is the secret_id every fake response reports, distinctive
// enough that TestResolveMetadataOnlyRegionAndRevision can assert it never
// leaks into Value.Metadata.
const fakeSecretID = "fake-secret-id-must-not-leak-into-metadata"

// fakeSM is an in-memory emulation of Secret Manager's by-path access route.
// Unlike a simple stub returning one canned response, it holds MULTIPLE
// numbered revisions per secret with independent enabled/disabled state: the
// module's most important design decision is that the ?revision= default
// ("latest_enabled") skips a disabled revision and serves the newest one
// still enabled, and that behavior can only be tested honestly against a
// fake that actually models revision history, not one that fakes a single
// response regardless of which revision was asked for.
type fakeSM struct {
	mu sync.Mutex

	// secrets is "path\x00name" -> revision number -> secretRevision.
	// Revision numbers start at 1, per Scaleway's documented numbering.
	secrets map[string]map[uint32]secretRevision

	// failCode is "path\x00name" -> status code to return for every request
	// against that secret, until the test replaces the fake or calls
	// clearFail. There is no need for most tests to clear it: each starts
	// from a fresh newFakeSM().
	failCode map[string]int

	reqCount int

	// lastAuthHeader, lastPath, and lastQuery record what actually went over
	// the wire for the most recent request: the X-Auth-Token header, the
	// request path (which carries the region and the revision selector), and
	// the parsed query (which carries secret_path, secret_name, and
	// project_id). Tests assert against these rather than only the value
	// that came back, so a request built with the wrong parameter name or a
	// credential in the wrong place cannot pass by accident.
	lastAuthHeader string
	lastPath       string
	lastQuery      url.Values
}

func newFakeSM() *fakeSM {
	return &fakeSM{
		secrets:  map[string]map[uint32]secretRevision{},
		failCode: map[string]int{},
	}
}

// secretKey combines path and name into fakeSM's internal map key.
func secretKey(path, name string) string { return path + "\x00" + name }

// set stores data as revision 1 of the secret at (path, name), enabled, with
// no CRC. It is the common-case helper; setRevision covers every other case
// (a specific revision number, a CRC, a disabled revision).
func (f *fakeSM) set(path, name string, data []byte) {
	f.setRevision(path, name, 1, data, nil, true)
}

// setRevision stores one numbered revision of a secret, as a real Scaleway
// write (optionally supplying a CRC) would. crc is nil to model a write with
// no CRC supplied, which is the normal case Scaleway's own data_crc32 being
// absent reflects, not an error.
func (f *fakeSM) setRevision(path, name string, revision uint32, data []byte, crc *uint32, enabled bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := secretKey(path, name)
	if f.secrets[key] == nil {
		f.secrets[key] = map[uint32]secretRevision{}
	}
	f.secrets[key][revision] = secretRevision{data: data, crc: crc, enabled: enabled}
}

// disable flips a stored revision's enabled bit off, modeling an operator
// revoking a leaked credential without deleting its history - the exact
// scenario the package doc comment gives for why ?revision defaults to
// "latest_enabled" rather than "latest".
func (f *fakeSM) disable(path, name string, revision uint32) {
	f.mu.Lock()
	defer f.mu.Unlock()
	revs := f.secrets[secretKey(path, name)]
	if revs == nil {
		return
	}
	sr := revs[revision]
	sr.enabled = false
	revs[revision] = sr
}

// failStatus makes every request against the secret at (path, name) return
// code, until the test replaces the fake or calls clearFail. The shape
// (fail one secret at a time, by path+name) mirrors what a conformance Fail
// hook needs: it can inject a specific mamori sentinel's HTTP status against
// only the key under test, the same shape providers/cloudflare-kv's
// failStatus(namespace, code) established for its own per-namespace failures.
func (f *fakeSM) failStatus(path, name string, code int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failCode[secretKey(path, name)] = code
}

// clearFail cancels a failStatus injected for (path, name).
func (f *fakeSM) clearFail(path, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.failCode, secretKey(path, name))
}

// counts returns how many access requests have been served.
func (f *fakeSM) counts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reqCount
}

// resolveRevisionSelector resolves a {revision} path-segment selector
// ("latest", "latest_enabled", or a decimal revision number) against revs,
// mirroring the real access route's own documented {revision} semantics.
// Disabling a revision on Scaleway makes it INACCESSIBLE, not merely "not
// preferred": scaleway-sdk-go's SecretVersion.Status doc comment says a
// disabled version "is not accessible but can be enabled", and the Scaleway
// CLI names the operations that flip it accordingly (`scw secret version
// disable` = "Make a specific version inaccessible", `enable` = "Make a
// specific version accessible"). So a disabled revision fails to resolve
// however it is addressed: "latest" selects the highest revision number but
// fails if THAT revision is disabled (it does not fall back to the newest
// enabled one - that is what "latest_enabled" is for), a decimal number fails
// if the revision it names is disabled, and only "latest_enabled" ever skips
// a disabled revision in favor of another. It reports false when no revision
// satisfies the selector, which the caller turns into a 404 - the same
// response an unknown secret produces, which is exactly the ambiguity
// access's 404 branch warns about.
func resolveRevisionSelector(revs map[uint32]secretRevision, selector string) (uint32, bool) {
	switch selector {
	case "latest":
		var max uint32
		found := false
		for rev := range revs {
			if !found || rev > max {
				max, found = rev, true
			}
		}
		if !found || !revs[max].enabled {
			return 0, false
		}
		return max, true
	case "latest_enabled":
		var max uint32
		found := false
		for rev, sr := range revs {
			if sr.enabled && (!found || rev > max) {
				max, found = rev, true
			}
		}
		return max, found
	default:
		n, err := strconv.ParseUint(selector, 10, 32)
		if err != nil {
			return 0, false
		}
		rev := uint32(n)
		sr, ok := revs[rev]
		if !ok || !sr.enabled {
			return 0, false
		}
		return rev, true
	}
}

// handle routes one request to the by-path access endpoint. Only GET against
// the documented path shape is recognized; anything else is a 404, matching
// how an unrecognized route behaves against the real API.
func (f *fakeSM) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.reqCount++
	f.lastAuthHeader = r.Header.Get("X-Auth-Token")
	f.lastPath = r.URL.Path
	f.lastQuery = r.URL.Query()
	f.mu.Unlock()

	if r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}

	idx := strings.Index(r.URL.Path, pathRegionsMarker)
	if idx < 0 {
		http.NotFound(w, r)
		return
	}
	rest := r.URL.Path[idx+len(pathRegionsMarker):]
	_, rest, ok := strings.Cut(rest, pathVersionsMarker)
	if !ok {
		http.NotFound(w, r)
		return
	}
	revisionSelector, ok := strings.CutSuffix(rest, pathAccessSuffix)
	if !ok {
		http.NotFound(w, r)
		return
	}

	q := r.URL.Query()
	f.serveAccess(w, q.Get("secret_path"), q.Get("secret_name"), revisionSelector)
}

// serveAccess serves the documented envelope for (path, name) at
// revisionSelector, or a 404 for an unknown secret, an unresolvable
// revision selector, or (via failCode) an injected failure.
func (f *fakeSM) serveAccess(w http.ResponseWriter, path, name, revisionSelector string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	key := secretKey(path, name)
	if code, failing := f.failCode[key]; failing {
		w.WriteHeader(code)
		_, _ = io.WriteString(w, `{"message":"injected failure"}`)
		return
	}

	revs, ok := f.secrets[key]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"secret not found"}`)
		return
	}
	rev, ok := resolveRevisionSelector(revs, revisionSelector)
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"revision not found"}`)
		return
	}
	sr := revs[rev]

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(accessResponse{
		SecretID:  fakeSecretID,
		Revision:  rev,
		Data:      sr.data,
		DataCrc32: sr.crc,
		Type:      "opaque",
	})
}

// roundTripper drives fakeSM's handler in-process, with no listener and no
// background goroutine. This is deliberate rather than a real
// httptest.Server: providertest's NoGoroutineLeak case (Task 4) runs
// goleak.VerifyNone with no ignore options, which a live server's accept
// goroutine can never satisfy.
type roundTripper struct{ f *fakeSM }

func (rt roundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	// Honor cancellation explicitly. http.Client delegates context handling
	// to the transport, so a RoundTripper that ignores it would serve a
	// request whose context is already dead and hide a provider that forgot
	// to thread ctx through.
	if err := r.Context().Err(); err != nil {
		return nil, err
	}
	rec := httptest.NewRecorder()
	rt.f.handle(rec, r)
	resp := rec.Result()
	resp.Request = r
	return resp, nil
}

// provider builds a Provider talking to this fake in-process, with no
// network listener. Extra options are applied last so a test can override
// any default.
func (f *fakeSM) provider(opts ...Option) *Provider {
	base := []Option{
		WithSecretKey(testSecretKey),
		WithProjectID(testProjectID),
		WithRegion(testRegion),
		WithHTTPClient(&http.Client{Transport: roundTripper{f: f}}),
	}
	return New(append(base, opts...)...)
}
