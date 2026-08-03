package httpcore_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providers/httpcore"
)

// ExampleNew is the README's "Client" block, verbatim.
func ExampleNew() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"level":"debug"}`))
	}))
	defer srv.Close()

	c, err := httpcore.New(httpcore.Config{
		BaseURL: srv.URL,
		Auth:    httpcore.Bearer("token"),
	})
	if err != nil {
		panic(err)
	}

	resp, err := c.Do(context.Background(), httpcore.Request{Path: "/config"})
	if err != nil {
		panic(err)
	}
	fmt.Println(string(resp.Body))
	// Output: {"level":"debug"}
}

// ExampleOAuth2ClientCredentials is the README's "Authenticators" block,
// verbatim.
func ExampleOAuth2ClientCredentials() {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token":"abc123","expires_in":3600}`))
	}))
	defer tokenSrv.Close()

	auth, err := httpcore.OAuth2ClientCredentials(httpcore.OAuth2Config{
		TokenURL:     tokenSrv.URL,
		ClientID:     "client-id",
		ClientSecret: "client-secret",
	})
	if err != nil {
		panic(err)
	}

	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, r.Header.Get("Authorization"))
	}))
	defer apiSrv.Close()

	c, err := httpcore.New(httpcore.Config{
		BaseURL: apiSrv.URL,
		Auth:    auth,
	})
	if err != nil {
		panic(err)
	}

	resp, err := c.Do(context.Background(), httpcore.Request{Path: "/config"})
	if err != nil {
		panic(err)
	}
	fmt.Println(string(resp.Body))
	// Output: Bearer abc123
}

// ExampleStatusForKind is the README's "Error classification" block, verbatim.
func ExampleStatusForKind() {
	status := httpcore.StatusForKind(mamori.KindRateLimited)
	fmt.Println(status)
	// Output: 429
}

// ExampleNewRevalidator is the README's "Conditional GET" block, verbatim.
func ExampleNewRevalidator() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == "v1" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", "v1")
		w.Write([]byte(`{"level":"debug"}`))
	}))
	defer srv.Close()

	c, err := httpcore.New(httpcore.Config{BaseURL: srv.URL})
	if err != nil {
		panic(err)
	}
	rv := httpcore.NewRevalidator(c, 0)

	first, err := rv.Get(context.Background(), "cfg", httpcore.Request{Path: "/config"})
	if err != nil {
		panic(err)
	}
	second, err := rv.Get(context.Background(), "cfg", httpcore.Request{Path: "/config"})
	if err != nil {
		panic(err)
	}
	fmt.Println(string(first.Body), second.NotModified, string(second.Body))
	// Output: {"level":"debug"} true {"level":"debug"}
}

// configProvider is the README's "Writing a provider on httpcore" block,
// verbatim: a minimal mamori.Provider built on httpcore.Client.
type configProvider struct {
	client *httpcore.Client
}

// newConfigProvider constructs a provider that resolves refs against baseURL
// using a bearer token.
func newConfigProvider(baseURL, token string) (*configProvider, error) {
	c, err := httpcore.New(httpcore.Config{
		BaseURL: baseURL,
		Auth:    httpcore.Bearer(token),
	})
	if err != nil {
		return nil, err
	}
	return &configProvider{client: c}, nil
}

// Scheme returns the URL scheme this provider handles.
func (p *configProvider) Scheme() string { return "example-config" }

// Resolve fetches ref.Path from the backend and selects ref.Key when present.
func (p *configProvider) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
	resp, err := p.client.Do(ctx, httpcore.Request{Path: ref.Path})
	if err != nil {
		return mamori.Value{}, err
	}

	b := resp.Body
	if ref.Key != "" {
		b, err = mamori.SelectKey(b, ref.Key)
		if err != nil {
			return mamori.Value{}, err
		}
	}

	return mamori.Value{
		Bytes:   b,
		Version: httpcore.Version(resp, b),
	}, nil
}

// ExampleClient_writingAProvider demonstrates configProvider end to end
// against a fake backend.
func ExampleClient_writingAProvider() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", "v1")
		w.Write([]byte(`{"level":"debug"}`))
	}))
	defer srv.Close()

	p, err := newConfigProvider(srv.URL, "token")
	if err != nil {
		panic(err)
	}

	v, err := p.Resolve(context.Background(), mamori.Ref{Path: "/config", Key: "level"})
	if err != nil {
		panic(err)
	}
	fmt.Println(string(v.Bytes), v.Version)
	// Output: debug v1
}
