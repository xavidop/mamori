package httpcore

import (
	"context"
	"errors"
	"fmt"
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

// credentialMarker is a stand-in credential value used by tests that assert a
// credential never reaches a returned error. QueryAuth puts this in the URL
// query, and any error path that renders req.URL without redaction would leak
// it. Like bodyMarker, it must not be a word that appears in any mamori
// sentinel's own text.
const credentialMarker = "s3cr3t-cred-marker-7c1d"

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

// TestDoPreservesEscapedSegment proves a caller can name one path segment whose
// own name contains a slash. url.PathEscape("config/prod/log-level") must reach
// the wire as a single %2F-bearing segment: not double-encoded to %252F, which
// names a key no backend has, and not decoded into three real segments, which
// names a different key. Workers KV keys routinely contain slashes, so a
// provider for one cannot migrate onto httpcore without this.
func TestDoPreservesEscapedSegment(t *testing.T) {
	var gotURI, gotPath string
	c, err := New(Config{
		BaseURL: "https://api.test/v1",
		HTTPClient: fakeClient(func(req *http.Request) (*http.Response, error) {
			gotURI = req.URL.RequestURI()
			gotPath = req.URL.Path
			resp, _ := newResponse(http.StatusOK, []byte("ok"), nil)
			return resp, nil
		}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.Do(context.Background(), Request{Path: url.PathEscape("config/prod/log-level")}); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if want := "/v1/config%2Fprod%2Flog-level"; gotURI != want {
		t.Fatalf("request URI = %q, want %q", gotURI, want)
	}
	// The decoded view must still be the decoded view: RawPath is a rendering
	// hint, not a second path.
	if want := "/v1/config/prod/log-level"; gotPath != want {
		t.Fatalf("URL.Path = %q, want %q", gotPath, want)
	}
}

// TestDoRejectsMalformedEscapedPath pins the cost of Request.Path being an
// escaped path: a bare percent sign is not one. Guessing, by falling back to
// treating the path as literal, would make what a ref means depend on whether
// its escapes happened to parse, so it is refused instead.
func TestDoRejectsMalformedEscapedPath(t *testing.T) {
	sent := false
	c, err := New(Config{
		BaseURL: "https://api.test/v1",
		HTTPClient: fakeClient(func(*http.Request) (*http.Response, error) {
			sent = true
			resp, _ := newResponse(http.StatusOK, nil, nil)
			return resp, nil
		}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = c.Do(context.Background(), Request{Path: "100%"})
	if !errors.Is(err, mamori.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
	if sent {
		t.Fatal("a malformed path reached the transport")
	}
}

// TestDoRejectsDotSegments pins that no provider built on httpcore can send a
// path that escapes the prefix its BaseURL declares.
//
// The check lives here rather than in each provider precisely so that a provider
// author cannot omit it: seven more providers and sixteen migrations are meant
// to be written against this package, and the one that forgets is the one that
// ships a traversal. A ref path is not only what a struct tag says, since
// ${VAR} interpolation substitutes application-supplied values at runtime.
//
// The encoded forms matter as much as the literal ones now that setPath
// preserves a caller's escapes, and the backslash forms matter because IIS and
// ASP.NET decode %5C and honour it as a directory separator.
func TestDoRejectsDotSegments(t *testing.T) {
	paths := []string{
		"../secrets", "a/../../b", "./cfg", "a/./b",
		`..\secrets`, `a\..\..\secrets`, `a/..\b`,
		"%2e%2e/secrets", "a/%2E%2E/b", `a%5C..%5Cb`,
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			sent := false
			c, err := New(Config{
				BaseURL: "https://api.test/v1/tenants/acme",
				HTTPClient: fakeClient(func(*http.Request) (*http.Response, error) {
					sent = true
					resp, _ := newResponse(http.StatusOK, nil, nil)
					return resp, nil
				}),
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			_, err = c.Do(context.Background(), Request{Path: path})
			if !errors.Is(err, mamori.ErrInvalid) {
				t.Fatalf("Do(%q) err = %v, want ErrInvalid", path, err)
			}
			if errors.Is(err, mamori.ErrNotFound) {
				t.Fatalf("Do(%q) reported ErrNotFound, which would hide the traversal behind a field default", path)
			}
			if sent {
				t.Fatalf("Do(%q) reached the transport; the rejection must precede the round trip", path)
			}
		})
	}
}

// TestDoAllowsBackslashInAnOrdinaryPath pins the scope of the backslash rule: a
// backslash is a separator when looking for dot segments, but a path that merely
// contains one is still an ordinary path. Rejecting every backslash outright
// would be simpler and would break a legitimate Windows-style key name.
//
// Three-dot segments are here for the same reason: RFC 3986 section 5.2.4
// defines dot-segment removal over exactly "." and "..", so "..." is an ordinary
// name and matching it would be over-reach.
func TestDoAllowsBackslashInAnOrdinaryPath(t *testing.T) {
	for _, path := range []string{`a\b`, ".../cfg", "a/...b/c"} {
		t.Run(path, func(t *testing.T) {
			c, err := New(Config{
				BaseURL: "https://api.test/v1",
				HTTPClient: fakeClient(func(*http.Request) (*http.Response, error) {
					resp, _ := newResponse(http.StatusOK, []byte("ok"), nil)
					return resp, nil
				}),
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if _, err := c.Do(context.Background(), Request{Path: path}); err != nil {
				t.Fatalf("Do(%q): %v", path, err)
			}
		})
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

// TestDoErrorDetailReachesTheMessage proves Config.ErrorDetail is the channel
// through which a provider's own error-envelope text reaches the classified
// error. Without it there is no API at all for the detail ClassifyStatus
// documents, and providers/doppler, providers/cloudflare-kv, providers/vercel-gc
// and providers/scaleway-sm all embed a bounded error body today; a migration
// onto httpcore has to be able to keep doing so.
func TestDoErrorDetailReachesTheMessage(t *testing.T) {
	var gotStatus int
	var gotBody []byte
	c, err := New(Config{
		BaseURL: "https://api.test",
		ErrorDetail: func(status int, body []byte) string {
			gotStatus, gotBody = status, body
			return "token lacks secrets:read"
		},
		HTTPClient: fakeClient(func(*http.Request) (*http.Response, error) {
			resp, _ := newResponse(http.StatusForbidden, []byte(`{"message":"nope"}`), nil)
			return resp, nil
		}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = c.Do(context.Background(), Request{Path: "x"})
	if err == nil {
		t.Fatal("Do with 403 returned nil error")
	}
	if !strings.Contains(err.Error(), "token lacks secrets:read") {
		t.Fatalf("detail missing from %q", err.Error())
	}
	if !errors.Is(err, mamori.ErrPermissionDenied) {
		t.Fatalf("err = %v, want ErrPermissionDenied", err)
	}
	if gotStatus != http.StatusForbidden {
		t.Fatalf("ErrorDetail saw status %d, want 403", gotStatus)
	}
	if string(gotBody) != `{"message":"nope"}` {
		t.Fatalf("ErrorDetail saw body %q, want the failing response's body", gotBody)
	}
}

// TestDoWithoutErrorDetailLeaksNoBody pins the safe default. A nil ErrorDetail
// must mean no detail, because a response body can be the resolved value
// itself, and it must also mean the body is never even offered to anything that
// could render it.
func TestDoWithoutErrorDetailLeaksNoBody(t *testing.T) {
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
	if err == nil {
		t.Fatal("Do with 403 returned nil error")
	}
	if strings.Contains(err.Error(), bodyMarker) {
		t.Fatalf("response body reached the error %q with no ErrorDetail configured", err.Error())
	}
}

// TestDoErrorDetailIsBoundedByMaxBody proves a huge error body cannot defeat the
// ceiling MaxBody exists to enforce. The failing path reads a body that the
// success path would have rejected outright, so without an explicit bound it
// would be the one way to make httpcore hold an unbounded response in memory.
func TestDoErrorDetailIsBoundedByMaxBody(t *testing.T) {
	var seen int
	var rb *recordingBody
	c, err := New(Config{
		BaseURL: "https://api.test",
		MaxBody: 64,
		ErrorDetail: func(_ int, body []byte) string {
			seen = len(body)
			return ""
		},
		HTTPClient: fakeClient(func(*http.Request) (*http.Response, error) {
			var resp *http.Response
			resp, rb = newResponse(http.StatusInternalServerError, []byte(strings.Repeat("x", 100000)), nil)
			return resp, nil
		}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = c.Do(context.Background(), Request{Path: "x"})
	if !errors.Is(err, mamori.ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	if seen > 64 {
		t.Fatalf("ErrorDetail saw %d bytes, want at most MaxBody (64)", seen)
	}
	// An oversized error body must not become a "response exceeds limit"
	// error either: the status is the answer the caller needs.
	if errors.Is(err, mamori.ErrInvalid) {
		t.Fatalf("err = %v: an oversized error body displaced the status classification", err)
	}
	// The drain-and-close guarantee, issue #107's subject, must survive the
	// extra read on this path.
	if rb == nil || !rb.closed {
		t.Fatal("body not closed on the ErrorDetail path")
	}
}

// TestDoErrorDetailNotCalledOnSuccess pins that the hook only ever sees a
// failing response. Calling it on a 200 would consume the body the caller is
// waiting for, and that body is the resolved value.
func TestDoErrorDetailNotCalledOnSuccess(t *testing.T) {
	called := false
	c, err := New(Config{
		BaseURL: "https://api.test",
		ErrorDetail: func(int, []byte) string {
			called = true
			return "should not appear"
		},
		HTTPClient: fakeClient(func(*http.Request) (*http.Response, error) {
			resp, _ := newResponse(http.StatusOK, []byte("payload"), nil)
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
	if called {
		t.Fatal("ErrorDetail was called for a 2xx")
	}
	if string(resp.Body) != "payload" {
		t.Fatalf("Body = %q, want payload: the detail hook consumed it", resp.Body)
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

// TestDoAllowsBodyExactlyAtCeiling proves the other half of readBounded's
// max+1 read: a body of exactly MaxBody bytes is not truncation and must
// succeed with every byte intact. TestDoBoundsBody only proves the
// over-ceiling half; without this case, an off-by-one that rejects a body at
// the exact boundary would ship green.
func TestDoAllowsBodyExactlyAtCeiling(t *testing.T) {
	exact := strings.Repeat("y", 100)
	c, err := New(Config{
		BaseURL: "https://api.test",
		MaxBody: 100,
		HTTPClient: fakeClient(func(*http.Request) (*http.Response, error) {
			resp, _ := newResponse(http.StatusOK, []byte(exact), nil)
			return resp, nil
		}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	resp, err := c.Do(context.Background(), Request{Path: "x"})
	if err != nil {
		t.Fatalf("Do with body exactly at MaxBody: %v", err)
	}
	if string(resp.Body) != exact {
		t.Fatalf("Body = %q, want the full %d-byte body", resp.Body, len(exact))
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

	// A fourth, distinct return path: readBounded rejects an oversized body.
	// It needs its own small MaxBody, so it cannot join the loop above as a
	// fourth status sharing one Client.
	var rb *recordingBody
	c, err := New(Config{
		BaseURL: "https://api.test",
		MaxBody: 100,
		HTTPClient: fakeClient(func(*http.Request) (*http.Response, error) {
			var resp *http.Response
			resp, rb = newResponse(http.StatusOK, []byte(strings.Repeat("x", 5000)), nil)
			return resp, nil
		}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, _ = c.Do(context.Background(), Request{Path: "x"})
	if rb == nil || !rb.closed {
		t.Fatal("oversized body: body not closed")
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

// TestDoRedactsCredentialFromClassifyError proves a QueryAuth credential does
// not reach the error from the classify-status wrap site. redactURL strips
// RawQuery and User for exactly this reason; nothing previously exercised
// that path with a credential actually present in the URL.
func TestDoRedactsCredentialFromClassifyError(t *testing.T) {
	c, err := New(Config{
		BaseURL: "https://api.test",
		Auth:    QueryAuth("access_token", credentialMarker),
		HTTPClient: fakeClient(func(*http.Request) (*http.Response, error) {
			resp, _ := newResponse(http.StatusForbidden, nil, nil)
			return resp, nil
		}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = c.Do(context.Background(), Request{Path: "x"})
	if err == nil {
		t.Fatal("Do with 403 returned nil error")
	}
	if strings.Contains(err.Error(), credentialMarker) {
		t.Fatalf("credential leaked into classify error %q", err.Error())
	}
}

// TestDoRedactsCredentialFromTransportError proves the same for the
// transport-error wrap site, which renders req.URL through a different
// fmt.Errorf call than the classify path.
func TestDoRedactsCredentialFromTransportError(t *testing.T) {
	sentinel := errors.New("dial tcp: connection refused")
	c, err := New(Config{
		BaseURL: "https://api.test",
		Auth:    QueryAuth("access_token", credentialMarker),
		HTTPClient: fakeClient(func(*http.Request) (*http.Response, error) {
			return nil, sentinel
		}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = c.Do(context.Background(), Request{Path: "x"})
	if err == nil {
		t.Fatal("Do with transport error returned nil error")
	}
	if strings.Contains(err.Error(), credentialMarker) {
		t.Fatalf("credential leaked into transport error %q", err.Error())
	}
}

// TestDoTransportErrorRedactsURLWithoutBreakingChain proves redactTransportError
// rebuilds the *url.Error rather than discarding it: the credential must be
// gone from the URL it carries, but errors.Is and errors.As must still reach
// everything they reached before redaction was added. This is the property
// that distinguishes the approved fix (rebuild the wrapper) from the simpler,
// rejected one (discard the wrapped cause entirely), so it needs its own test
// rather than relying on the credential-absence checks above.
func TestDoTransportErrorRedactsURLWithoutBreakingChain(t *testing.T) {
	sentinel := errors.New("dial tcp: connection refused")
	c, err := New(Config{
		BaseURL: "https://api.test",
		Auth:    QueryAuth("access_token", credentialMarker),
		HTTPClient: fakeClient(func(*http.Request) (*http.Response, error) {
			return nil, sentinel
		}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = c.Do(context.Background(), Request{Path: "x"})
	if err == nil {
		t.Fatal("Do with transport error returned nil error")
	}

	// errors.Is must still reach mamori.ErrUnavailable, the sentinel this
	// package wraps every transport failure in.
	if !errors.Is(err, mamori.ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	// errors.Is must still reach the raw cause the fake RoundTripper returned,
	// through the rebuilt *url.Error, not just the *url.Error itself.
	if !errors.Is(err, sentinel) {
		t.Fatalf("cause lost from %v", err)
	}

	// errors.As must still reach a *url.Error: rebuilding it, rather than
	// discarding it for a plain string, is what keeps this working.
	var urlErr *url.Error
	if !errors.As(err, &urlErr) {
		t.Fatalf("errors.As(err, *url.Error) = false for %v", err)
	}
	if strings.Contains(urlErr.URL, "?") {
		t.Fatalf("urlErr.URL = %q, want no query string", urlErr.URL)
	}
	if strings.Contains(urlErr.URL, credentialMarker) {
		t.Fatalf("urlErr.URL = %q, credential leaked", urlErr.URL)
	}
}

// pathIDMarker stands in for an identifier a provider carries as a path
// segment: an account id, a namespace id, an organisation. Like the two markers
// above it must not appear in any mamori sentinel's own text, so an assertion
// trips on a real leak rather than on the sentinel.
const pathIDMarker = "s3cr3t-path-id-4b8e"

// pathIDPlaceholder is what hidePathID substitutes for pathIDMarker. It is
// asserted on as well as the marker's absence, so a hook that silently dropped
// the whole path, or was never called at all, cannot pass.
const pathIDPlaceholder = "<id>"

// hidePathID is the Config.RedactPath hook the tests below install: the shape a
// path-identified provider writes, substituting a placeholder for an identifier
// rather than discarding the path.
func hidePathID(path string) string {
	return strings.ReplaceAll(path, pathIDMarker, pathIDPlaceholder)
}

// TestDoRedactPathHidesPathFromClassifiedStatusError covers the wrap site that
// renders a URL once ClassifyStatus has rejected the response: the commonest
// error any provider returns, and the one an operator is most likely to paste
// somewhere.
func TestDoRedactPathHidesPathFromClassifiedStatusError(t *testing.T) {
	c, err := New(Config{
		BaseURL:    "https://api.test",
		RedactPath: hidePathID,
		HTTPClient: fakeClient(func(*http.Request) (*http.Response, error) {
			resp, _ := newResponse(http.StatusForbidden, nil, nil)
			return resp, nil
		}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = c.Do(context.Background(), Request{Path: "accounts/" + pathIDMarker + "/values/k"})
	if err == nil {
		t.Fatal("Do with 403 returned nil error")
	}
	if strings.Contains(err.Error(), pathIDMarker) {
		t.Fatalf("path identifier leaked into the classified-status error %q", err.Error())
	}
	if !strings.Contains(err.Error(), pathIDPlaceholder) {
		t.Fatalf("RedactPath was not applied to the classified-status error %q", err.Error())
	}
}

// TestDoRedactPathHidesPathFromTransportError covers the transport-failure wrap
// site, and with it the subtle half of this hook: Do rebuilds net/http's
// *url.Error with a redacted URL, and that rebuilt URL must go through
// RedactPath too. Skipping it there would leave the path one errors.As away,
// and would render it into the very same message through the wrapped cause, so
// the assertions are made against both the message and the *url.Error itself.
//
// The chain assertions are the other half: the rebuild exists to keep
// errors.As reaching *url.Error and errors.Is reaching the transport's own
// cause, and a hook must not be an excuse to start discarding either.
func TestDoRedactPathHidesPathFromTransportError(t *testing.T) {
	sentinel := errors.New("dial tcp: connection refused")
	c, err := New(Config{
		BaseURL:    "https://api.test",
		RedactPath: hidePathID,
		HTTPClient: fakeClient(func(*http.Request) (*http.Response, error) {
			return nil, sentinel
		}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = c.Do(context.Background(), Request{Path: "accounts/" + pathIDMarker + "/values/k"})
	if err == nil {
		t.Fatal("Do with a transport error returned nil error")
	}
	if strings.Contains(err.Error(), pathIDMarker) {
		t.Fatalf("path identifier leaked into the transport error %q", err.Error())
	}
	if !strings.Contains(err.Error(), pathIDPlaceholder) {
		t.Fatalf("RedactPath was not applied to the transport error %q", err.Error())
	}

	var urlErr *url.Error
	if !errors.As(err, &urlErr) {
		t.Fatalf("errors.As(err, *url.Error) = false for %v", err)
	}
	if strings.Contains(urlErr.URL, pathIDMarker) {
		t.Fatalf("urlErr.URL = %q: RedactPath was skipped on the *url.Error rebuild", urlErr.URL)
	}
	if !strings.Contains(urlErr.URL, pathIDPlaceholder) {
		t.Fatalf("urlErr.URL = %q, want the RedactPath placeholder", urlErr.URL)
	}
	if !errors.Is(err, mamori.ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("cause lost from %v", err)
	}
}

// TestDoRedactPathHidesPathFromRequestBuildError covers the third wrap site,
// the one reached before anything is sent: http.NewRequestWithContext rejects
// the request and httpcore renders the URL it would have called. An invalid
// method is what drives it here, because it fails on a URL that is otherwise
// perfectly well formed.
func TestDoRedactPathHidesPathFromRequestBuildError(t *testing.T) {
	c, err := New(Config{
		BaseURL:    "https://api.test",
		RedactPath: hidePathID,
		HTTPClient: fakeClient(func(*http.Request) (*http.Response, error) {
			t.Error("a request that could not be built reached the transport")
			return newResponseOK()
		}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = c.Do(context.Background(), Request{
		Method: "BAD METHOD",
		Path:   "accounts/" + pathIDMarker + "/values/k",
	})
	if err == nil {
		t.Fatal("Do with an invalid method returned nil error")
	}
	if strings.Contains(err.Error(), pathIDMarker) {
		t.Fatalf("path identifier leaked into the request-build error %q", err.Error())
	}
	if !strings.Contains(err.Error(), pathIDPlaceholder) {
		t.Fatalf("RedactPath was not applied to the request-build error %q", err.Error())
	}
}

// TestDoRedactPathHidesPathFromOversizedBodyError covers the fourth and last
// wrap site, the one a body over MaxBody reaches. It renders the URL through a
// different fmt.Errorf than the classified-status path directly above it, so
// nothing but its own test proves the hook reaches it.
func TestDoRedactPathHidesPathFromOversizedBodyError(t *testing.T) {
	c, err := New(Config{
		BaseURL:    "https://api.test",
		MaxBody:    8,
		RedactPath: hidePathID,
		HTTPClient: fakeClient(func(*http.Request) (*http.Response, error) {
			resp, _ := newResponse(http.StatusOK, []byte(strings.Repeat("x", 64)), nil)
			return resp, nil
		}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = c.Do(context.Background(), Request{Path: "accounts/" + pathIDMarker + "/values/k"})
	if err == nil {
		t.Fatal("Do with an oversized body returned nil error")
	}
	if strings.Contains(err.Error(), pathIDMarker) {
		t.Fatalf("path identifier leaked into the oversized-body error %q", err.Error())
	}
	if !strings.Contains(err.Error(), pathIDPlaceholder) {
		t.Fatalf("RedactPath was not applied to the oversized-body error %q", err.Error())
	}
}

// TestDoNilRedactPathRendersPathVerbatim pins the default. Every provider whose
// path names nothing but a key wants the path in its errors, and that is the
// overwhelming majority of them: a hook that quietly changed how an
// unconfigured Client renders a URL would make every one of their messages
// worse to pay for a guarantee they did not ask for.
func TestDoNilRedactPathRendersPathVerbatim(t *testing.T) {
	c, err := New(Config{
		BaseURL: "https://api.test",
		HTTPClient: fakeClient(func(*http.Request) (*http.Response, error) {
			resp, _ := newResponse(http.StatusForbidden, nil, nil)
			return resp, nil
		}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = c.Do(context.Background(), Request{Path: "accounts/" + pathIDMarker + "/values/k"})
	if err == nil {
		t.Fatal("Do with 403 returned nil error")
	}
	want := "https://api.test/accounts/" + pathIDMarker + "/values/k"
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q does not render the URL as %q: a nil RedactPath must change nothing", err.Error(), want)
	}
}

// TestDoPreservesAuthenticatorClassification pins that Do does not overwrite a
// kind an Authenticator already chose.
//
// mamori.ErrorKind tests KindUnauthenticated before KindUnavailable and
// KindRateLimited, so wrapping every Apply failure in ErrUnauthenticated would
// silently reclassify a 503 or a 429 from a token endpoint. The core module
// treats KindUnauthenticated as terminal and the other two as self-healing, so
// the difference decides whether a passing identity-provider blip reports the
// field unhealthy in Status, Health and `mamori doctor`.
func TestDoPreservesAuthenticatorClassification(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
		want mamori.Kind
	}{
		{"unclassified becomes unauthenticated", errors.New("no credential file"), mamori.KindUnauthenticated},
		{"unavailable survives", fmt.Errorf("token endpoint: %w", mamori.ErrUnavailable), mamori.KindUnavailable},
		{"rate limited survives", fmt.Errorf("token endpoint: %w", mamori.ErrRateLimited), mamori.KindRateLimited},
		{"permission denied survives", fmt.Errorf("token endpoint: %w", mamori.ErrPermissionDenied), mamori.KindPermissionDenied},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c, err := New(Config{
				BaseURL: "https://api.test",
				Auth: AuthenticatorFunc(func(context.Context, *http.Request) error {
					return tt.err
				}),
				HTTPClient: fakeClient(func(*http.Request) (*http.Response, error) {
					t.Error("request reached the transport despite a failing Authenticator")
					return newResponseOK()
				}),
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			_, err = c.Do(context.Background(), Request{Path: "cfg"})
			if err == nil {
				t.Fatal("Do with a failing Authenticator returned nil error")
			}
			if got := mamori.ErrorKind(err); got != tt.want {
				t.Fatalf("ErrorKind = %q, want %q (err = %v)", got, tt.want, err)
			}
			// The cause must stay reachable either way, so a caller can still
			// see what the Authenticator actually reported.
			if !errors.Is(err, tt.err) {
				t.Fatalf("cause lost from %v", err)
			}
		})
	}
}

// newResponseOK is the placeholder response for a transport that must never be
// reached.
func newResponseOK() (*http.Response, error) {
	resp, _ := newResponse(http.StatusOK, nil, nil)
	return resp, nil
}
