package nacos

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providers/httpcore"
)

func mustRef(t *testing.T, raw string) mamori.Ref {
	t.Helper()
	ref, err := mamori.ParseRef(raw)
	if err != nil {
		t.Fatalf("ParseRef(%q): %v", raw, err)
	}
	return ref
}

func TestScheme(t *testing.T) {
	if got := New().Scheme(); got != scheme {
		t.Fatalf("Scheme() = %q, want %q", got, scheme)
	}
}

func TestCoordinatesFor(t *testing.T) {
	clearEnv(t)
	tests := []struct {
		name      string
		ref       string
		opts      []Option
		wantGroup string
		wantData  string
		wantErr   bool
	}{
		{name: "bare dataId takes the default group", ref: "nacos://app.properties", wantGroup: defaultGroup, wantData: "app.properties"},
		{name: "group is the first segment", ref: "nacos://prod/db.json", wantGroup: "prod", wantData: "db.json"},
		{name: "configured group replaces the default", ref: "nacos://app.yaml", opts: []Option{WithGroup("team-a")}, wantGroup: "team-a", wantData: "app.yaml"},
		{name: "an explicit group beats the configured one", ref: "nacos://prod/app.yaml", opts: []Option{WithGroup("team-a")}, wantGroup: "prod", wantData: "app.yaml"},
		{name: "dots in a dataId are ordinary", ref: "nacos://com.example.svc.yaml", wantGroup: defaultGroup, wantData: "com.example.svc.yaml"},
		{name: "a fragment is not part of the path", ref: "nacos://app.json#log.level", wantGroup: defaultGroup, wantData: "app.json"},
		{name: "three segments are rejected", ref: "nacos://a/b/c", wantErr: true},
		{name: "an empty path is rejected", ref: "nacos://", wantErr: true},
		{name: "a group with no dataId is rejected", ref: "nacos://prod/", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := New(tt.opts...)
			got, err := p.coordinatesFor(mustRef(t, tt.ref))
			if tt.wantErr {
				if !errors.Is(err, mamori.ErrInvalid) {
					t.Fatalf("err = %v, want mamori.ErrInvalid", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("coordinatesFor: %v", err)
			}
			if got.group != tt.wantGroup || got.dataID != tt.wantData {
				t.Fatalf("coordinates = %+v, want group=%q dataId=%q", got, tt.wantGroup, tt.wantData)
			}
		})
	}
}

func TestResolveReturnsTheRawBody(t *testing.T) {
	clearEnv(t)
	fake := newFakeNacos()
	fake.set("", defaultGroup, "app.properties", "log.level=debug\nport=8080\n")
	p := fake.provider("")

	v, err := p.Resolve(context.Background(), mustRef(t, "nacos://app.properties"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	// Nacos answers with the configuration content itself, not a JSON envelope,
	// so nothing is unwrapped.
	if got := string(v.Bytes); got != "log.level=debug\nport=8080\n" {
		t.Fatalf("Bytes = %q, want the raw body verbatim", got)
	}
	if v.Version == "" {
		t.Fatal("Version is empty")
	}
	if v.Sensitive {
		t.Fatal("Sensitive is set; Nacos holds configuration, not managed secrets")
	}
}

func TestResolveSelectsAKey(t *testing.T) {
	clearEnv(t)
	fake := newFakeNacos()
	fake.set("", defaultGroup, "db.json", `{"user":"svc","password":"p"}`)
	p := fake.provider("")

	v, err := p.Resolve(context.Background(), mustRef(t, "nacos://db.json#user"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := string(v.Bytes); got != "svc" {
		t.Fatalf("Bytes = %q, want svc", got)
	}
}

func TestVersionCoversOnlyTheSelectedBytes(t *testing.T) {
	clearEnv(t)
	fake := newFakeNacos()
	fake.set("", defaultGroup, "db.json", `{"user":"svc","password":"a"}`)
	p := fake.provider("")
	ref := mustRef(t, "nacos://db.json#user")

	before, err := p.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	// A sibling key moves; the selected field did not.
	fake.set("", defaultGroup, "db.json", `{"user":"svc","password":"b"}`)
	after, err := p.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if before.Version != after.Version {
		t.Fatalf("Version changed from %q to %q because an unrelated key moved; a field bound to #user must not report a change", before.Version, after.Version)
	}

	fake.set("", defaultGroup, "db.json", `{"user":"other","password":"b"}`)
	changed, err := p.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if changed.Version == after.Version {
		t.Fatal("Version did not change when the selected field itself moved")
	}
}

func TestResolveNotFound(t *testing.T) {
	clearEnv(t)
	fake := newFakeNacos()
	p := fake.provider("")

	_, err := p.Resolve(context.Background(), mustRef(t, "nacos://absent.yaml"))
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("err = %v, want mamori.ErrNotFound", err)
	}
	if !strings.Contains(err.Error(), "dataId=absent.yaml") {
		t.Fatalf("err = %v, want the coordinates named so a wrong group is distinguishable", err)
	}
}

func TestResolveSendsNacosCoordinatesAsQueryParameters(t *testing.T) {
	clearEnv(t)
	fake := newFakeNacos()
	fake.set("ns-1", "prod", "app.yaml", "x")
	p := fake.provider("ns-1")

	if _, err := p.Resolve(context.Background(), mustRef(t, "nacos://prod/app.yaml")); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	_, q, _ := fake.snapshot()
	if q.Get("dataId") != "app.yaml" || q.Get("group") != "prod" {
		t.Fatalf("query = %v, want dataId=app.yaml group=prod", q)
	}
	// The namespace parameter is named "tenant" on the v1 API.
	if q.Get("tenant") != "ns-1" {
		t.Fatalf("tenant = %q, want ns-1", q.Get("tenant"))
	}
}

func TestResolveOmitsTenantWhenThereIsNoNamespace(t *testing.T) {
	clearEnv(t)
	fake := newFakeNacos()
	fake.set("", defaultGroup, "app.yaml", "x")
	p := fake.provider("")

	if _, err := p.Resolve(context.Background(), mustRef(t, "nacos://app.yaml")); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	_, q, _ := fake.snapshot()
	if _, present := q["tenant"]; present {
		t.Fatalf("tenant sent as %q; omitting it is what selects the public namespace", q.Get("tenant"))
	}
}

func TestResolveHonorsAContextThatIsAlreadyDone(t *testing.T) {
	clearEnv(t)
	fake := newFakeNacos()
	fake.set("", defaultGroup, "app.yaml", "x")
	p := fake.provider("")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := p.Resolve(ctx, mustRef(t, "nacos://app.yaml"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	// The cancellation is returned bare, not as a backend failure. Letting the
	// request go out anyway and reporting the transport error instead would
	// classify a clean shutdown as mamori.KindUnavailable, which is what marks
	// a field unhealthy in Status, Health, and `mamori doctor`.
	if errors.Is(err, mamori.ErrUnavailable) {
		t.Fatalf("a cancelled context was reported as a backend outage: %v", err)
	}
	if _, q, _ := fake.snapshot(); q != nil {
		t.Fatalf("a request reached the backend on an already-dead context: %v", q)
	}
}

func TestLoginObtainsATokenAndSendsIt(t *testing.T) {
	clearEnv(t)
	fake := newFakeNacos()
	fake.requireAuth = true
	fake.token = "tok-abc"
	fake.set("", defaultGroup, "app.yaml", "value")

	p := New(
		WithServerAddr("http://nacos.test"),
		WithHTTPClient(&http.Client{Transport: fake.transport()}),
		WithCredentials("nacos", "s3cr3t"),
		WithGroup(defaultGroup),
	)

	for range 3 {
		v, err := p.Resolve(context.Background(), mustRef(t, "nacos://app.yaml"))
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if string(v.Bytes) != "value" {
			t.Fatalf("Bytes = %q, want value", v.Bytes)
		}
	}

	_, q, _ := fake.snapshot()
	if q.Get(accessTokenParam) != "tok-abc" {
		t.Fatalf("accessToken = %q, want tok-abc", q.Get(accessTokenParam))
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	// The token is cached until its stated TTL: three resolves, one login.
	if fake.loginCalls != 1 {
		t.Fatalf("loginCalls = %d, want 1; the token must be cached for its tokenTtl", fake.loginCalls)
	}
}

func TestLoginWithoutATTLIsNotCached(t *testing.T) {
	clearEnv(t)
	fake := newFakeNacos()
	fake.requireAuth = true
	fake.tokenTTL = -1 // serialized as a non-positive tokenTtl
	fake.set("", defaultGroup, "app.yaml", "value")

	p := New(
		WithServerAddr("http://nacos.test"),
		WithHTTPClient(&http.Client{Transport: fake.transport()}),
		WithCredentials("nacos", "s3cr3t"),
		WithGroup(defaultGroup),
	)
	for range 3 {
		if _, err := p.Resolve(context.Background(), mustRef(t, "nacos://app.yaml")); err != nil {
			t.Fatalf("Resolve: %v", err)
		}
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.loginCalls != 3 {
		t.Fatalf("loginCalls = %d, want 3; a token of unknown lifetime must not be cached", fake.loginCalls)
	}
}

func TestCredentialsMustBeSuppliedTogether(t *testing.T) {
	clearEnv(t)
	fake := newFakeNacos()
	p := New(
		WithServerAddr("http://nacos.test"),
		WithHTTPClient(&http.Client{Transport: fake.transport()}),
		WithCredentials("nacos", ""),
	)
	_, err := p.Resolve(context.Background(), mustRef(t, "nacos://app.yaml"))
	if !errors.Is(err, mamori.ErrInvalid) {
		t.Fatalf("err = %v, want mamori.ErrInvalid; half a credential must not silently become no credential", err)
	}
}

func TestTheAccessTokenNeverReachesAnError(t *testing.T) {
	clearEnv(t)
	fake := newFakeNacos()
	fake.requireAuth = true
	fake.token = "super-secret-token"
	// Nothing is published, so the read fails and the error is built from the
	// very URL the token was appended to.
	p := New(
		WithServerAddr("http://nacos.test"),
		WithHTTPClient(&http.Client{Transport: fake.transport()}),
		WithCredentials("nacos", "hunter2"),
		WithGroup(defaultGroup),
	)

	_, err := p.Resolve(context.Background(), mustRef(t, "nacos://app.yaml"))
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, secret := range []string{"super-secret-token", "hunter2"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error text leaks %q: %v", secret, err)
		}
	}
}

func TestWithAuthTakesPrecedenceOverCredentials(t *testing.T) {
	clearEnv(t)
	fake := newFakeNacos()
	fake.set("", defaultGroup, "app.yaml", "value")

	p := New(
		WithServerAddr("http://nacos.test"),
		WithHTTPClient(&http.Client{Transport: fake.transport()}),
		WithCredentials("nacos", "s3cr3t"),
		WithAuth(httpcore.HeaderAuth("X-Custom", "v")),
		WithGroup(defaultGroup),
	)
	if _, err := p.Resolve(context.Background(), mustRef(t, "nacos://app.yaml")); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.loginCalls != 0 {
		t.Fatalf("loginCalls = %d, want 0; WithAuth must bypass the username/password login", fake.loginCalls)
	}
}

func TestBaseURLJoinsTheContextPath(t *testing.T) {
	tests := []struct {
		addr, ctxPath, want string
	}{
		{"http://nacos.test:8848", "/nacos", "http://nacos.test:8848/nacos"},
		{"http://nacos.test:8848/", "nacos", "http://nacos.test:8848/nacos"},
		{"https://nacos.example.com", "/custom/", "https://nacos.example.com/custom"},
	}
	for _, tt := range tests {
		if got := baseURL(tt.addr, tt.ctxPath); got != tt.want {
			t.Fatalf("baseURL(%q, %q) = %q, want %q", tt.addr, tt.ctxPath, got, tt.want)
		}
	}
}

func TestDefaultClientOutlastsTheLongPollHold(t *testing.T) {
	// httpcore's own default client has a 30s Timeout, which would abort every
	// idle 30s listener poll at the instant the server was about to answer
	// "nothing changed".
	p := New()
	c := p.watchSafeClient()
	if c.Timeout <= defaultHold {
		t.Fatalf("client Timeout = %v, want more than the %v hold", c.Timeout, defaultHold)
	}
	if c.Timeout <= defaultHold+httpcore.DefaultLongPollGrace {
		t.Fatalf("client Timeout = %v, want more than hold+grace (%v) so the per-round context deadline is what fires",
			c.Timeout, defaultHold+httpcore.DefaultLongPollGrace)
	}

	custom := &http.Client{Timeout: time.Second}
	if got := New(WithHTTPClient(custom)).watchSafeClient(); got != custom {
		t.Fatal("WithHTTPClient must be used verbatim")
	}
}

func TestABadServerAddressIsAClassifiedError(t *testing.T) {
	clearEnv(t)
	p := New(WithServerAddr("://not-a-url"))
	_, err := p.Resolve(context.Background(), mustRef(t, "nacos://app.yaml"))
	if !errors.Is(err, mamori.ErrInvalid) {
		t.Fatalf("err = %v, want mamori.ErrInvalid", err)
	}
	// Sticky: the second call reports the same thing rather than rebuilding.
	if _, err2 := p.Resolve(context.Background(), mustRef(t, "nacos://app.yaml")); err2.Error() != err.Error() {
		t.Fatalf("second error %v differs from the first %v", err2, err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.builds != 1 {
		t.Fatalf("builds = %d, want 1; a failed construction must be remembered, not redone on every resolve", p.builds)
	}
}

func TestTheClientIsBuiltExactlyOnce(t *testing.T) {
	clearEnv(t)
	fake := newFakeNacos()
	fake.set("", defaultGroup, "app.yaml", "value")
	p := New(
		WithServerAddr("http://nacos.test"),
		WithHTTPClient(&http.Client{Transport: fake.transport()}),
		WithGroup(defaultGroup),
	)
	for range 4 {
		if _, err := p.Resolve(context.Background(), mustRef(t, "nacos://app.yaml")); err != nil {
			t.Fatalf("Resolve: %v", err)
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.builds != 1 {
		t.Fatalf("builds = %d, want 1; concurrent and repeated resolves must share one client", p.builds)
	}
}
