package httpcore

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/xavidop/mamori"
)

// bodyMarker is the fake response body used by the tests that assert a payload
// never reaches an error message. It must not be a word that appears in any
// mamori sentinel's own text: mamori.ErrPermissionDenied renders as "mamori:
// permission denied", so a body of "denied" would make the assertion trip on
// the sentinel rather than on a real leak.
const bodyMarker = "s3cr3t-body-marker-9f2a"

func TestNewRejectsEmptyBaseURL(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("New with empty BaseURL returned nil error")
	}
}

func TestNewRejectsUnparsableBaseURL(t *testing.T) {
	if _, err := New(Config{BaseURL: "://nope"}); err == nil {
		t.Fatal("New with unparsable BaseURL returned nil error")
	}
}

func TestDoJoinsPathAndQuery(t *testing.T) {
	var gotURL string
	c, err := New(Config{
		BaseURL: "https://api.test/v1",
		HTTPClient: fakeClient(func(req *http.Request) (*http.Response, error) {
			gotURL = req.URL.String()
			resp, _ := newResponse(http.StatusOK, []byte(`{"a":1}`), nil)
			return resp, nil
		}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = c.Do(context.Background(), Request{
		Path:  "cfg/main",
		Query: url.Values{"env": {"prod"}},
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if want := "https://api.test/v1/cfg/main?env=prod"; gotURL != want {
		t.Fatalf("URL = %q, want %q", gotURL, want)
	}
}

func TestDoAppliesAuthAndUserAgent(t *testing.T) {
	var gotAuth, gotUA string
	c, err := New(Config{
		BaseURL:   "https://api.test",
		UserAgent: "test-agent/1",
		Auth:      Bearer("tok"),
		HTTPClient: fakeClient(func(req *http.Request) (*http.Response, error) {
			gotAuth = req.Header.Get("Authorization")
			gotUA = req.Header.Get("User-Agent")
			resp, _ := newResponse(http.StatusOK, nil, nil)
			return resp, nil
		}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.Do(context.Background(), Request{Path: "x"}); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if gotAuth != "Bearer tok" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if gotUA != "test-agent/1" {
		t.Fatalf("User-Agent = %q", gotUA)
	}
}

func TestDoReturnsValidators(t *testing.T) {
	h := http.Header{}
	h.Set("ETag", `W/"abc"`)
	h.Set("Last-Modified", "Wed, 21 Oct 2026 07:28:00 GMT")

	c, err := New(Config{
		BaseURL: "https://api.test",
		HTTPClient: fakeClient(func(*http.Request) (*http.Response, error) {
			resp, _ := newResponse(http.StatusOK, []byte("payload"), h)
			return resp, nil
		}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	resp, err := c.Do(context.Background(), Request{Path: "x"})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if resp.ETag != `W/"abc"` {
		t.Fatalf("ETag = %q", resp.ETag)
	}
	if resp.LastModified != "Wed, 21 Oct 2026 07:28:00 GMT" {
		t.Fatalf("LastModified = %q", resp.LastModified)
	}
	if string(resp.Body) != "payload" {
		t.Fatalf("Body = %q", resp.Body)
	}
}

func TestDoSendsConditionalHeaders(t *testing.T) {
	var inm, ims string
	c, err := New(Config{
		BaseURL: "https://api.test",
		HTTPClient: fakeClient(func(req *http.Request) (*http.Response, error) {
			inm = req.Header.Get("If-None-Match")
			ims = req.Header.Get("If-Modified-Since")
			resp, _ := newResponse(http.StatusNotModified, nil, nil)
			return resp, nil
		}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	resp, err := c.Do(context.Background(), Request{
		Path:            "x",
		IfNoneMatch:     `W/"abc"`,
		IfModifiedSince: "Wed, 21 Oct 2026 07:28:00 GMT",
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if inm != `W/"abc"` || ims != "Wed, 21 Oct 2026 07:28:00 GMT" {
		t.Fatalf("conditional headers = (%q, %q)", inm, ims)
	}
	if !resp.NotModified {
		t.Fatal("NotModified = false, want true")
	}
}

func TestDoClassifiesStatus(t *testing.T) {
	c, err := New(Config{
		BaseURL: "https://api.test",
		HTTPClient: fakeClient(func(*http.Request) (*http.Response, error) {
			resp, _ := newResponse(http.StatusForbidden, []byte(bodyMarker), nil)
			return resp, nil
		}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = c.Do(context.Background(), Request{Path: "x"})
	if !errors.Is(err, mamori.ErrPermissionDenied) {
		t.Fatalf("err = %v, want ErrPermissionDenied", err)
	}
	// The body must not leak into the message: it can contain the value itself.
	if strings.Contains(err.Error(), bodyMarker) {
		t.Fatalf("response body leaked into error %q", err.Error())
	}
}

func TestDoBoundsBody(t *testing.T) {
	big := strings.Repeat("x", 5000)
	c, err := New(Config{
		BaseURL: "https://api.test",
		MaxBody: 100,
		HTTPClient: fakeClient(func(*http.Request) (*http.Response, error) {
			resp, _ := newResponse(http.StatusOK, []byte(big), nil)
			return resp, nil
		}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = c.Do(context.Background(), Request{Path: "x"})
	if err == nil {
		t.Fatal("Do with oversized body returned nil error")
	}
	if !errors.Is(err, mamori.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}

func TestDoClosesBodyOnEveryPath(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusForbidden, http.StatusNotModified} {
		var rb *recordingBody
		c, err := New(Config{
			BaseURL: "https://api.test",
			HTTPClient: fakeClient(func(*http.Request) (*http.Response, error) {
				var resp *http.Response
				resp, rb = newResponse(status, []byte("body"), nil)
				return resp, nil
			}),
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		_, _ = c.Do(context.Background(), Request{Path: "x"})
		if rb == nil || !rb.closed {
			t.Fatalf("status %d: body not closed", status)
		}
	}
}

func TestDoWrapsTransportError(t *testing.T) {
	sentinel := errors.New("dial tcp: connection refused")
	c, err := New(Config{
		BaseURL: "https://api.test",
		HTTPClient: fakeClient(func(*http.Request) (*http.Response, error) {
			return nil, sentinel
		}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = c.Do(context.Background(), Request{Path: "x"})
	if !errors.Is(err, mamori.ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	// Two %w verbs must preserve the cause for errors.As and errors.Is.
	if !errors.Is(err, sentinel) {
		t.Fatalf("cause lost from %v", err)
	}
}
