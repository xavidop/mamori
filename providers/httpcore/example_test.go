package httpcore_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providers/httpcore"
)

// ExampleNew is the README's "Client" block, verbatim.
func ExampleNew() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"level":"debug"}`))
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

// ExampleClient_Do_escapedSegment is the README's "Request.Path is an escaped
// path" block, verbatim. The backend echoes the request URI it actually
// received, which is the only thing that settles whether an escape survived.
func ExampleClient_Do_escapedSegment() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, r.URL.RequestURI())
	}))
	defer srv.Close()

	c, err := httpcore.New(httpcore.Config{BaseURL: srv.URL + "/v1"})
	if err != nil {
		panic(err)
	}

	resp, err := c.Do(context.Background(), httpcore.Request{
		Path: url.PathEscape("config/prod/log-level"),
	})
	if err != nil {
		panic(err)
	}
	fmt.Println(string(resp.Body))
	// Output: /v1/config%2Fprod%2Flog-level
}

// ExampleClient_Do_dotSegments is the README's "Do refuses a path that escapes
// the BaseURL" block, verbatim. No server is needed: every one of these is
// refused before a request is built.
func ExampleClient_Do_dotSegments() {
	c, err := httpcore.New(httpcore.Config{
		BaseURL: "https://api.example.com/v1/tenants/acme",
	})
	if err != nil {
		panic(err)
	}

	for _, path := range []string{
		"../../other-tenant/cfg",
		`a\..\..\secrets`,
		"%2e%2e/secrets",
	} {
		_, err := c.Do(context.Background(), httpcore.Request{Path: path})
		fmt.Println(errors.Is(err, mamori.ErrInvalid))
	}
	// Output:
	// true
	// true
	// true
}

// ExampleOAuth2ClientCredentials is the README's "Authenticators" block,
// verbatim.
func ExampleOAuth2ClientCredentials() {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"abc123","expires_in":3600}`))
	}))
	defer tokenSrv.Close()

	auth, err := httpcore.OAuth2ClientCredentials(httpcore.OAuth2Config{
		TokenURL:     tokenSrv.URL,
		ClientID:     "client-id",
		ClientSecret: "client-secret",
		// httptest serves http://, which is exactly what AllowInsecure is for.
		// A real identity provider needs https:// and must not set this.
		AllowInsecure: true,
	})
	if err != nil {
		panic(err)
	}

	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, r.Header.Get("Authorization"))
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

// ExampleConfig_errorDetail is the README's "Supplying detail" block, verbatim
// apart from the fake backend it is exercised against. It shows the shape the
// hook is for: parse the envelope, return only the field you have decided
// cannot carry the resolved value.
func ExampleConfig_errorDetail() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":"token_scope_missing","value":"s3cr3t"}}`))
	}))
	defer srv.Close()

	c, err := httpcore.New(httpcore.Config{
		BaseURL: srv.URL,
		ErrorDetail: func(status int, body []byte) string {
			// Only the fields you have decided cannot carry the resolved value.
			var env struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if json.Unmarshal(body, &env) != nil {
				return ""
			}
			return env.Error.Code
		},
	})
	if err != nil {
		panic(err)
	}

	_, err = c.Do(context.Background(), httpcore.Request{Path: "/config"})
	fmt.Println(errors.Is(err, mamori.ErrPermissionDenied))
	fmt.Println(strings.Contains(err.Error(), "token_scope_missing"))
	// The sibling field the hook did not select never reaches the message.
	fmt.Println(strings.Contains(err.Error(), "s3cr3t"))
	// Output:
	// true
	// true
	// false
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
		_, _ = w.Write([]byte(`{"level":"debug"}`))
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
//
// ref.Path needs no traversal check here: Do rejects a "." or ".." segment,
// literal or percent-encoded, before it sends anything.
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
		_, _ = w.Write([]byte(`{"level":"debug"}`))
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
