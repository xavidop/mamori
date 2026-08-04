//go:build integration

// Integration test for the Nacos provider. It runs against a real Nacos server
// and is the only place the long-poll listener meets the actual implementation
// of the protocol rather than this repository's reading of it.
//
// Start a server and run the suite:
//
//	docker run --rm -p 8848:8848 -e MODE=standalone nacos/nacos-server:v2.4.3
//	export MAMORI_NACOS_ADDR=http://127.0.0.1:8848
//	# optional, when auth is enabled on the server:
//	export MAMORI_NACOS_USERNAME=nacos MAMORI_NACOS_PASSWORD=nacos
//	# optional, to run in a namespace other than public:
//	export MAMORI_NACOS_NAMESPACE=<namespace-id>
//	GOWORK=off go test -tags integration -run Integration ./...
//
// The test publishes configurations under a unique dataId prefix and deletes
// them afterwards. It skips cleanly, without failing, when MAMORI_NACOS_ADDR is
// unset, so `go test -tags integration ./...` is safe to run anywhere.
package nacos

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providers/httpcore"
	"github.com/xavidop/mamori/providertest"
)

// liveConfig is the environment a live run needs, or a skip.
type liveConfig struct {
	addr      string
	namespace string
	username  string
	password  string
}

func liveEnv(t *testing.T) liveConfig {
	t.Helper()
	addr := os.Getenv("MAMORI_NACOS_ADDR")
	if addr == "" {
		t.Skip("MAMORI_NACOS_ADDR not set; skipping live Nacos integration test")
	}
	return liveConfig{
		addr:      addr,
		namespace: os.Getenv("MAMORI_NACOS_NAMESPACE"),
		username:  os.Getenv("MAMORI_NACOS_USERNAME"),
		password:  os.Getenv("MAMORI_NACOS_PASSWORD"),
	}
}

// provider builds the provider under test from the live environment.
func (lc liveConfig) provider() *Provider {
	opts := []Option{
		WithServerAddr(lc.addr),
		WithNamespace(lc.namespace),
		WithGroup(defaultGroup),
		// Short enough that a whole conformance run does not sit on one 30s
		// hold, long enough that the server still parks the request.
		WithLongPollTimeout(10 * time.Second),
	}
	if lc.username != "" {
		opts = append(opts, WithCredentials(lc.username, lc.password))
	}
	return New(opts...)
}

// admin publishes and deletes configurations over the same v1 API, which is what
// a Seed and a Mutate need and what this provider deliberately does not offer:
// mamori providers read, they never write.
type admin struct {
	lc liveConfig

	once   sync.Once
	client *httpcore.Client
	err    error
}

func (a *admin) core(t *testing.T) *httpcore.Client {
	t.Helper()
	a.once.Do(func() {
		var auth httpcore.Authenticator
		if a.lc.username != "" {
			auth, a.err = newTokenAuth(baseURL(a.lc.addr, defaultContextPath), a.lc.username, a.lc.password, nil)
			if a.err != nil {
				return
			}
		}
		a.client, a.err = httpcore.New(httpcore.Config{
			BaseURL:    baseURL(a.lc.addr, defaultContextPath),
			Auth:       auth,
			HTTPClient: &http.Client{Timeout: 30 * time.Second},
			UserAgent:  "mamori-nacos-integration",
		})
	})
	if a.err != nil {
		t.Fatalf("building admin client: %v", a.err)
	}
	return a.client
}

func (a *admin) form(dataID string) url.Values {
	form := url.Values{"dataId": {dataID}, "group": {defaultGroup}}
	if a.lc.namespace != "" {
		form.Set("tenant", a.lc.namespace)
	}
	return form
}

func (a *admin) publish(t *testing.T, dataID, content string) error {
	t.Helper()
	form := a.form(dataID)
	form.Set("content", content)
	_, err := a.core(t).Do(context.Background(), httpcore.Request{
		Method: http.MethodPost,
		Path:   configPath,
		Header: http.Header{"Content-Type": {"application/x-www-form-urlencoded"}},
		Body:   []byte(form.Encode()),
	})
	return err
}

func (a *admin) remove(t *testing.T, dataID string) {
	t.Helper()
	_, _ = a.core(t).Do(context.Background(), httpcore.Request{
		Method: http.MethodDelete,
		Path:   configPath,
		Query:  a.form(dataID),
	})
}

// TestIntegrationConformance runs the conformance kit against a live server.
func TestIntegrationConformance(t *testing.T) {
	lc := liveEnv(t)
	a := &admin{lc: lc}

	prefix := fmt.Sprintf("mamori-it-%d-", time.Now().UnixNano())
	var created sync.Map
	t.Cleanup(func() {
		created.Range(func(k, _ any) bool {
			a.remove(t, k.(string))
			return true
		})
	})

	publish := func(_ context.Context, key, val string) error {
		created.Store(key, struct{}{})
		return a.publish(t, key, val)
	}

	providertest.Run(t, providertest.Config{
		New:        func() mamori.Provider { return lc.provider() },
		Ref:        func(key string) string { return "nacos://" + key },
		Key:        func(name string) string { return prefix + name },
		PointerRef: func(key, frag string) string { return "nacos://" + key + frag },
		Seed:       publish,
		Mutate:     publish,
		// See the unit-test conformance config: the baseline read and the MD5
		// the first listener round subscribes with come from one response.
		WatchDeliversBaseline: true,
		// A live backend offers nothing to inject a per-key failure with. The
		// unit run against the fake covers classification, including the whole
		// status table (errors_test.go).
		NoResolveErrors:   true,
		EventuallyTimeout: 30 * time.Second,
	})
}

// TestIntegrationLongPollListener exercises the listener against the real
// implementation of the protocol, which is the one thing a fake cannot settle:
// whether this package's reading of the separators, the header spelling, and the
// response encoding matches what Nacos actually does.
func TestIntegrationLongPollListener(t *testing.T) {
	lc := liveEnv(t)
	a := &admin{lc: lc}

	dataID := fmt.Sprintf("mamori-it-watch-%d.properties", time.Now().UnixNano())
	t.Cleanup(func() { a.remove(t, dataID) })
	if err := a.publish(t, dataID, "value=one"); err != nil {
		t.Fatalf("publish: %v", err)
	}

	p := lc.provider()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ref, err := mamori.ParseRef("nacos://" + dataID)
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	ch, err := p.Watch(ctx, ref)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}

	await := func(want string) {
		t.Helper()
		deadline := time.After(45 * time.Second)
		for {
			select {
			case u, open := <-ch:
				if !open {
					t.Fatal("watch channel closed")
				}
				if u.Err != nil {
					t.Logf("transient watch error: %v", u.Err)
					continue
				}
				if strings.TrimSpace(string(u.Value.Bytes)) == want {
					return
				}
			case <-deadline:
				t.Fatalf("no update carrying %q within 45s; if this is the only failure, the long-poll wire format is wrong", want)
			}
		}
	}

	await("value=one") // baseline
	if err := a.publish(t, dataID, "value=two"); err != nil {
		t.Fatalf("publish: %v", err)
	}
	await("value=two")
}
