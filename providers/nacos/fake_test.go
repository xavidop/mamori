package nacos

import (
	"bytes"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/xavidop/mamori/providers/httpcore"
)

// fakeNacos is an in-process Nacos server reachable through an
// http.RoundTripper.
//
// An httptest.Server is deliberately not used. The conformance kit's
// NoGoroutineLeak case snapshots goroutines with goleak.IgnoreCurrent before the
// first subtest, and a live server's accept goroutine does not survive that
// snapshot. It also could not implement the listener honestly: the whole point
// of the long-poll round is that it parks until the caller's context ends, and
// that is only observable if the fake sees the very context the provider built.
//
// It implements the protocol as the real server does, including the two details
// that are easiest to get wrong and that produce a watch which silently never
// fires:
//
//   - the probe is read from the Listening-Configs FORM field, split on the
//     0x01 and 0x02 control characters after the form decoder has turned %01
//     and %02 back into them;
//   - the response is URLEncoder-encoded, so it carries the literal characters
//     "%02" and "%01" rather than the control bytes.
//
// It also refuses to park a request that carries no Long-Pulling-Timeout header,
// exactly as LongPollingService.isSupportLongPolling does.
type fakeNacos struct {
	mu       sync.Mutex
	configs  map[string]string
	failStat int
	// changed is closed and replaced on every mutation, so a parked listener
	// wakes without polling.
	changed chan struct{}

	// Observations, for tests that assert on the wire format.
	lastListenBody   string
	lastConfigQuery  url.Values
	lastListenHeader http.Header
	listenRounds     int
	loginCalls       int

	// requireAuth makes every non-login request answer 403 unless it carries a
	// valid accessToken.
	requireAuth bool
	token       string
	tokenTTL    int64
}

// newFakeNacos returns an empty server.
func newFakeNacos() *fakeNacos {
	return &fakeNacos{
		configs: map[string]string{},
		changed: make(chan struct{}),
	}
}

// configKey is how a configuration is addressed in the fake's map.
func configKey(tenant, group, dataID string) string {
	return tenant + "\x00" + group + "\x00" + dataID
}

// set publishes a configuration and wakes every parked listener.
func (f *fakeNacos) set(tenant, group, dataID, val string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.configs[configKey(tenant, group, dataID)] = val
	close(f.changed)
	f.changed = make(chan struct{})
}

// del removes a configuration and wakes every parked listener.
func (f *fakeNacos) del(tenant, group, dataID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.configs, configKey(tenant, group, dataID))
	close(f.changed)
	f.changed = make(chan struct{})
}

// fail makes every configuration request answer status until clearFail.
func (f *fakeNacos) fail(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failStat = status
}

// clearFail cancels fail.
func (f *fakeNacos) clearFail() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failStat = 0
}

// rounds reports how many listener requests have been served.
func (f *fakeNacos) rounds() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listenRounds
}

// snapshot returns the recorded request observations.
func (f *fakeNacos) snapshot() (listenBody string, configQuery url.Values, listenHeader http.Header) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastListenBody, f.lastConfigQuery, f.lastListenHeader.Clone()
}

// transport returns an http.RoundTripper serving this fake.
func (f *fakeNacos) transport() http.RoundTripper {
	return roundTripFunc(func(req *http.Request) (*http.Response, error) {
		// Honor cancellation explicitly. http.Client delegates context handling
		// to the transport, and an in-process fake has none of the dial and
		// connection-reuse machinery a real net/http.Transport notices a dead
		// context inside. Without this check a cancelled request would be
		// answered as though it had succeeded, hiding a provider that failed to
		// thread ctx into the request it built.
		if err := req.Context().Err(); err != nil {
			return nil, err
		}

		var body []byte
		if req.Body != nil {
			b, err := io.ReadAll(req.Body)
			if err != nil {
				return nil, err
			}
			_ = req.Body.Close()
			body = b
		}

		switch {
		case strings.HasSuffix(req.URL.Path, "/"+loginPath):
			return f.serveLogin(body)
		case strings.HasSuffix(req.URL.Path, "/"+listenerPath):
			return f.serveListener(req, body)
		case strings.HasSuffix(req.URL.Path, "/"+configPath):
			return f.serveConfig(req)
		default:
			return textResponse(http.StatusNotFound, "unknown endpoint"), nil
		}
	})
}

// serveLogin answers Nacos's username/password login.
func (f *fakeNacos) serveLogin(body []byte) (*http.Response, error) {
	form, _ := url.ParseQuery(string(body))
	f.mu.Lock()
	f.loginCalls++
	tok, ttl := f.token, f.tokenTTL
	f.mu.Unlock()

	if form.Get("username") == "" || form.Get("password") == "" {
		return textResponse(http.StatusForbidden, "unknown user"), nil
	}
	if tok == "" {
		tok = "test-token"
	}
	if ttl == 0 {
		ttl = 18000
	}
	return textResponse(http.StatusOK,
		`{"accessToken":"`+tok+`","tokenTtl":`+strconv.FormatInt(ttl, 10)+`,"globalAdmin":true}`), nil
}

// serveConfig answers the v1 configuration read: a RAW body on success, and the
// real server's plain-text messages on failure.
func (f *fakeNacos) serveConfig(req *http.Request) (*http.Response, error) {
	q := req.URL.Query()

	f.mu.Lock()
	f.lastConfigQuery = q
	fail := f.failStat
	needAuth := f.requireAuth
	want := f.token
	val, ok := f.configs[configKey(q.Get("tenant"), q.Get("group"), q.Get("dataId"))]
	f.mu.Unlock()

	if needAuth && q.Get(accessTokenParam) != orDefault(want, "test-token") {
		return textResponse(http.StatusForbidden, "user not found!"), nil
	}
	if fail != 0 {
		return textResponse(fail, "injected failure"), nil
	}
	if !ok {
		// The exact status and message the real server writes from
		// ConfigServletInner.doGetConfig for a configuration that is absent.
		return textResponse(http.StatusNotFound, "config data not exist"), nil
	}
	return textResponse(http.StatusOK, val), nil
}

// probe is one parsed Listening-Configs record.
type probe struct {
	dataID, group, md5, tenant string
}

// serveListener implements the long-poll listener.
func (f *fakeNacos) serveListener(req *http.Request, body []byte) (*http.Response, error) {
	f.mu.Lock()
	f.listenRounds++
	f.lastListenBody = string(body)
	f.lastListenHeader = req.Header.Clone()
	needAuth := f.requireAuth
	want := f.token
	f.mu.Unlock()

	if needAuth && req.URL.Query().Get(accessTokenParam) != orDefault(want, "test-token") {
		return textResponse(http.StatusForbidden, "user not found!"), nil
	}

	// The probe travels as an ordinary form value, so the form decoder is what
	// turns %02 and %01 back into the control characters.
	form, err := url.ParseQuery(string(body))
	if err != nil {
		return textResponse(http.StatusBadRequest, "invalid Listening-Configs"), nil
	}
	probes := parseProbes(form.Get(listeningConfigsParam))
	if len(probes) == 0 {
		return textResponse(http.StatusBadRequest, "invalid Listening-Configs"), nil
	}

	// LongPollingService.isSupportLongPolling parks a request only when the
	// header is present. Without it the server answers immediately, which reads
	// as "nothing changed" - so a client that omits or misspells the header gets
	// a watch that never fires.
	raw := req.Header.Get(longPullingTimeoutHeader)
	if raw == "" {
		return textResponse(http.StatusOK, f.compare(probes)), nil
	}
	ms, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || ms <= 0 {
		return textResponse(http.StatusBadRequest, "bad Long-Pulling-Timeout"), nil
	}
	hold := time.Duration(ms) * time.Millisecond

	timer := time.NewTimer(hold)
	defer timer.Stop()
	for {
		f.mu.Lock()
		wake := f.changed
		f.mu.Unlock()

		if out := f.compare(probes); out != "" {
			return textResponse(http.StatusOK, out), nil
		}
		select {
		case <-wake:
			// Something was published; re-compare.
		case <-timer.C:
			return textResponse(http.StatusOK, ""), nil
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
	}
}

// compare returns the URL-encoded list of probes whose MD5 no longer matches, in
// exactly the shape MD5Util.compareMd5ResultString produces: the fields joined
// by 0x02, each record terminated by 0x01, and the whole string URL-encoded.
func (f *fakeNacos) compare(probes []probe) string {
	f.mu.Lock()
	defer f.mu.Unlock()

	var b strings.Builder
	for _, pr := range probes {
		content := f.configs[configKey(pr.tenant, pr.group, pr.dataID)]
		if serverMD5(content) == pr.md5 {
			continue
		}
		b.WriteString(pr.dataID)
		b.WriteString(wordSeparator)
		b.WriteString(pr.group)
		if pr.tenant != "" {
			b.WriteString(wordSeparator)
			b.WriteString(pr.tenant)
		}
		b.WriteString(lineSeparator)
	}
	if b.Len() == 0 {
		return ""
	}
	return url.QueryEscape(b.String())
}

// parseProbes splits a decoded Listening-Configs value into records.
func parseProbes(s string) []probe {
	var out []probe
	for _, entry := range strings.Split(s, lineSeparator) {
		if entry == "" {
			continue
		}
		fields := strings.Split(entry, wordSeparator)
		if len(fields) < 3 {
			continue
		}
		pr := probe{dataID: fields[0], group: fields[1], md5: fields[2]}
		if len(fields) > 3 {
			pr.tenant = fields[3]
		}
		out = append(out, pr)
	}
	return out
}

// serverMD5 is the digest the server holds for stored content. Absent content
// has the empty digest, which is what a client that holds nothing sends.
func serverMD5(content string) string {
	return contentMD5([]byte(content))
}

// orDefault returns s, or def when s is empty.
func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// roundTripFunc adapts a function to http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

// RoundTrip calls f.
func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// textResponse builds a plain-text response, which is what every v1 endpoint
// here answers with.
func textResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": {"text/plain;charset=UTF-8"}},
		Body:       io.NopCloser(bytes.NewReader([]byte(body))),
	}
}

// client builds an httpcore.Client over this fake.
func (f *fakeNacos) client() *httpcore.Client {
	c, err := httpcore.New(httpcore.Config{
		BaseURL:    "http://nacos.test/nacos",
		HTTPClient: &http.Client{Transport: f.transport()},
		UserAgent:  "mamori-nacos",
	})
	if err != nil {
		panic("building fake client: " + err.Error())
	}
	return c
}

// provider builds a Provider served by this fake, in the given namespace and
// with a short hold so tests do not wait on a real 30s poll.
func (f *fakeNacos) provider(namespace string, opts ...Option) *Provider {
	base := []Option{
		withClient(f.client()),
		WithNamespace(namespace),
		WithGroup(defaultGroup),
		WithLongPollTimeout(2 * time.Second),
	}
	return New(append(base, opts...)...)
}
