package httpcore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/xavidop/mamori"
)

// Request is one call against a Client's backend.
type Request struct {
	// Method defaults to GET when empty.
	Method string
	// Path is joined onto the Client's BaseURL.
	Path string
	// Query is merged into the URL.
	Query url.Values
	// Header is merged into the request headers, before Auth is applied.
	Header http.Header
	// Body is the request payload, for the backends whose read path is a POST.
	Body []byte
	// IfNoneMatch sets If-None-Match, making this a conditional GET.
	IfNoneMatch string
	// IfModifiedSince sets If-Modified-Since, making this a conditional GET.
	IfModifiedSince string
}

// Response is a completed round trip.
type Response struct {
	// Status is the HTTP status code.
	Status int
	// Body is the response payload, read up to the Client's MaxBody. It is nil
	// when NotModified is true.
	Body []byte
	// ETag is the response ETag, for the next conditional GET.
	ETag string
	// LastModified is the response Last-Modified, for the next conditional GET.
	LastModified string
	// NotModified reports a 304, meaning the caller's cached copy is current.
	NotModified bool
}

// Do performs one round trip. It applies Auth, bounds and always drains the
// response body, and classifies the status through ClassifyStatus.
//
// A 304 returns a Response with NotModified set and a nil Body, and no error: a
// successful conditional GET is not a failure.
//
// The response body never appears in a returned error. A body can contain the
// resolved value, so classification carries the status only.
func (c *Client) Do(ctx context.Context, r Request) (*Response, error) {
	req, err := c.build(ctx, r)
	if err != nil {
		return nil, err
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("httpcore: %s %s: %w: %w",
			req.Method, redactURL(req.URL), mamori.ErrUnavailable, redactTransportError(err, req.URL))
	}
	// Drain and close on every path so the connection is reused rather than
	// abandoned. This is the hygiene issue #107 found missing across providers.
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	out := &Response{
		Status:       resp.StatusCode,
		ETag:         resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"),
		NotModified:  resp.StatusCode == http.StatusNotModified,
	}

	if err := ClassifyStatus(resp.StatusCode, ""); err != nil {
		return nil, fmt.Errorf("httpcore: %s %s: %w", req.Method, redactURL(req.URL), err)
	}
	if out.NotModified {
		return out, nil
	}

	body, err := readBounded(resp.Body, c.maxBody)
	if err != nil {
		return nil, fmt.Errorf("httpcore: %s %s: %w", req.Method, redactURL(req.URL), err)
	}
	out.Body = body
	return out, nil
}

// build assembles the *http.Request, applying headers, query, and Auth in that
// order so an Authenticator can see and override anything set before it.
func (c *Client) build(ctx context.Context, r Request) (*http.Request, error) {
	method := r.Method
	if method == "" {
		method = http.MethodGet
	}

	u := *c.base
	u.Path = joinPath(c.base.Path, r.Path)
	if len(r.Query) > 0 {
		q := u.Query()
		for k, vs := range r.Query {
			for _, v := range vs {
				q.Add(k, v)
			}
		}
		u.RawQuery = q.Encode()
	}

	var body io.Reader
	if len(r.Body) > 0 {
		body = bytes.NewReader(r.Body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, fmt.Errorf("httpcore: building %s %s: %w: %w", method, redactURL(&u), mamori.ErrInvalid, err)
	}

	for k, vs := range r.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if c.userAgent != "" {
		req.Header.Set("User-Agent", c.userAgent)
	}
	if r.IfNoneMatch != "" {
		req.Header.Set("If-None-Match", r.IfNoneMatch)
	}
	if r.IfModifiedSince != "" {
		req.Header.Set("If-Modified-Since", r.IfModifiedSince)
	}
	if c.auth != nil {
		if err := c.auth.Apply(ctx, req); err != nil {
			return nil, fmt.Errorf("httpcore: applying credentials: %w: %w", mamori.ErrUnauthenticated, err)
		}
	}
	return req, nil
}

// joinPath joins a base path and a request path with exactly one separator.
func joinPath(base, p string) string {
	switch {
	case p == "":
		return base
	case base == "" || base == "/":
		return "/" + strings.TrimPrefix(p, "/")
	default:
		return strings.TrimSuffix(base, "/") + "/" + strings.TrimPrefix(p, "/")
	}
}

// readBounded reads at most max bytes, returning ErrInvalid if the body is
// larger. Reading max+1 is what distinguishes "exactly at the ceiling", which is
// fine, from "over the ceiling", which is truncation and must not be silent.
func readBounded(r io.Reader, max int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, fmt.Errorf("reading response: %w: %w", mamori.ErrUnavailable, err)
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("response exceeds %d byte limit: %w", max, mamori.ErrInvalid)
	}
	return b, nil
}

// redactURL renders a URL for an error message with its query and userinfo
// stripped, because QueryAuth puts a credential in the query and a URL can
// carry userinfo.
func redactURL(u *url.URL) string {
	c := *u
	c.RawQuery = ""
	c.User = nil
	return c.String()
}

// redactTransportError strips the credential that net/http leaks into the error
// it wraps a transport failure in.
//
// http.Client.Do returns a *url.Error whose URL field is stripPassword(req.URL),
// and stripPassword masks only a userinfo password: the query string survives
// intact. A QueryAuth credential therefore reaches err.Error() through the
// wrapped cause, bypassing redactURL entirely, and renders as:
//
//	Get "https://api/cfg?access_token=SUPERSECRET": dial tcp: connection refused
//
// The wrapper is rebuilt rather than discarded so the chain stays whole:
// errors.As still reaches *url.Error, and errors.Is still reaches the underlying
// net.OpError, timeout, or TLS verification error.
func redactTransportError(err error, u *url.URL) error {
	var ue *url.Error
	if !errors.As(err, &ue) {
		return err
	}
	return &url.Error{Op: ue.Op, URL: redactURL(u), Err: ue.Err}
}
