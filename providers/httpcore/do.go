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
	// Path is joined onto the Client's BaseURL. It is an ESCAPED path, the
	// same form net/url calls RawPath, so a caller can name one segment whose
	// own name contains a slash:
	//
	//	Path: url.PathEscape("config/prod/log-level")
	//
	// reaches the wire as config%2Fprod%2Flog-level, one segment, rather than
	// as three segments or as a double-encoded config%252Fprod%252Flog-level.
	// Backends whose keys may contain slashes, Cloudflare Workers KV among
	// them, cannot address a key at all without that distinction.
	//
	// The cost of that contract is that a literal percent sign must be written
	// "%25". A Path that is not a valid escaped path is rejected with
	// mamori.ErrInvalid rather than guessed at, because guessing would make
	// the meaning of a ref depend on whether its escapes happened to parse.
	//
	// A "." or ".." segment is rejected: see [Client.Do].
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
// response body, and classifies the status through [ClassifyStatus].
//
// A 304 returns a Response with NotModified set and a nil Body, and no error: a
// successful conditional GET is not a failure.
//
// A Request.Path containing a "." or ".." segment, literal or percent-encoded,
// is rejected with mamori.ErrInvalid before anything is sent, so a path cannot
// escape the prefix the BaseURL declares. That check lives here rather than in
// each provider so no provider can forget it; the unexported hasDotSegment
// carries the full reasoning, including why backslash counts as a separator.
//
// The response body reaches a returned error only through
// [Config.ErrorDetail], and only on a failing status. A body can contain the
// resolved value, so the default is that classification carries the status
// alone.
func (c *Client) Do(ctx context.Context, r Request) (*Response, error) {
	req, err := c.build(ctx, r)
	if err != nil {
		return nil, err
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("httpcore: %s %s: %w: %w",
			req.Method, c.redactURL(req.URL), mamori.ErrUnavailable, c.redactTransportError(err, req.URL))
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

	if err := classify(resp.StatusCode, ""); err != nil {
		// Only now, on a status this package has already decided is a failure,
		// is reading the body for a detail safe: on a success the body IS the
		// resolved value and belongs to the caller, not to an error message.
		if c.errorDetail != nil {
			err = classify(resp.StatusCode, c.errorDetail(resp.StatusCode, readErrorBody(resp.Body, c.maxBody)))
		}
		return nil, fmt.Errorf("httpcore: %s %s: %w", req.Method, c.redactURL(req.URL), err)
	}
	if out.NotModified {
		return out, nil
	}

	body, err := readBounded(resp.Body, c.maxBody)
	if err != nil {
		return nil, fmt.Errorf("httpcore: %s %s: %w", req.Method, c.redactURL(req.URL), err)
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
	if err := setPath(&u, c.base.EscapedPath(), r.Path); err != nil {
		return nil, err
	}
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
		return nil, fmt.Errorf("httpcore: building %s %s: %w: %w", method, c.redactURL(&u), mamori.ErrInvalid, err)
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
			return nil, authError(err)
		}
	}
	return req, nil
}

// authError wraps a failure from Authenticator.Apply, adding
// mamori.ErrUnauthenticated only when the Authenticator supplied no
// classification of its own.
//
// Adding the sentinel unconditionally would not merely duplicate a kind, it
// would REPLACE one. mamori.ErrorKind walks kindSentinels in a fixed order and
// tests KindUnauthenticated before KindUnavailable and KindRateLimited, so a
// credential fetch that already classified itself, say
// [OAuth2ClientCredentials] taking a 503 or a 429 from the token endpoint, comes
// back out of Do as "unauthenticated" instead.
//
// That distinction is load bearing rather than cosmetic. fieldUnhealthy in the
// core module treats KindUnauthenticated as terminal, while KindUnavailable and
// KindRateLimited are the kinds mamori expects to heal on their own. Collapsing
// them makes a passing blip at the identity provider report the field unhealthy
// in Status, Health and `mamori doctor`, when the honest answer is that the
// backend is briefly unreachable and the next poll will very likely succeed.
//
// An Authenticator that returns a bare, unclassified error still gets
// ErrUnauthenticated, because a credential that could not be applied is exactly
// that and an unclassified error would otherwise reach mamori as KindUnknown.
func authError(err error) error {
	if mamori.ErrorKind(err) == mamori.KindUnknown {
		return fmt.Errorf("httpcore: applying credentials: %w: %w", mamori.ErrUnauthenticated, err)
	}
	// One %w, because the cause already carries the sentinel that classifies it.
	return fmt.Errorf("httpcore: applying credentials: %w", err)
}

// setPath joins reqPath onto basePath and assigns the result to u, setting both
// Path and RawPath so a percent escape the caller wrote survives to the wire.
//
// net/url stores a path twice: Path holds the decoded form, RawPath the encoded
// one, and EscapedPath returns RawPath only when it is a valid encoding of Path,
// falling back to escaping Path otherwise. Writing the joined path into Path
// alone, as this package once did, therefore re-escapes every percent sign the
// caller wrote, turning url.PathEscape("config/prod/log-level") into
// "config%252Fprod%252Flog-level" on the wire: a key no backend has. Backends
// whose key names may themselves contain slashes cannot be addressed at all
// without this.
//
// It rejects before it assigns, because both rejections describe a path that
// must not be sent at all.
func setPath(u *url.URL, basePath, reqPath string) error {
	decodedReq, err := url.PathUnescape(reqPath)
	if err != nil {
		return fmt.Errorf("httpcore: request path %q is not a valid escaped path, write a literal percent sign as %%25: %w: %w",
			reqPath, mamori.ErrInvalid, err)
	}
	if hasDotSegment(decodedReq) {
		return fmt.Errorf("httpcore: request path %q contains a dot segment, which would escape the path prefix the BaseURL declares: %w",
			reqPath, mamori.ErrInvalid)
	}

	raw := joinPath(basePath, reqPath)
	decoded, err := url.PathUnescape(raw)
	if err != nil {
		return fmt.Errorf("httpcore: request path %q joined onto the BaseURL is not a valid escaped path: %w: %w",
			reqPath, mamori.ErrInvalid, err)
	}
	u.Path = decoded
	// Mirror net/url's own (*URL).setPath: keep RawPath only when the default
	// escaping of Path would not reproduce it. A redundant RawPath is dead
	// weight, and a RawPath that does not decode back to Path is silently
	// ignored, so the two must be set together or not at all.
	u.RawPath = ""
	if (&url.URL{Path: decoded}).EscapedPath() != raw {
		u.RawPath = raw
	}
	return nil
}

// hasDotSegment reports whether p, a DECODED path, contains a "." or ".."
// segment.
//
// This check lives in httpcore, not in each provider, so that no provider can
// forget it: every path any provider sends goes through [Client.Do]. joinPath
// does not resolve dot segments, so "../.." in a caller-supplied path reaches
// outside the path prefix the BaseURL declares. For a client scoped to
// https://api/v1/tenants/acme that is another tenant's configuration, reached
// without ever leaving the declared host, and therefore without tripping
// whatever host or endpoint restriction the provider relies on to contain
// exactly this.
//
// It is reachable rather than theoretical. A provider's path comes from a
// mamori ref, and a ref path is not only what the struct tag says:
// expandRefVars substitutes ${VAR} from WithRefVars (decode.go in the core
// module), whose values the application supplies at runtime, so a ref of
// https://billing/${TENANT}/cfg carries whatever TENANT holds.
//
// Rejecting is the loud option. Cleaning silently would change which value a ref
// names, and a ref that quietly means something other than it says is worse than
// one that fails. `mamori doctor` resolves every ref before deployment, so this
// surfaces there rather than in production.
//
// Backslash counts as a separator here, not only '/'. Splitting on '/' alone
// leaves `a\..\..\secrets` as a single segment that matches neither "." nor
// "..", so the check passes and the request goes out. url.URL percent encodes
// the backslashes, so the wire carries "%5C", which most backends treat as an
// ordinary character. IIS and ASP.NET are the well known exceptions: they decode
// it and honour '\' as a directory separator, which is the classic backslash
// traversal bypass. A BaseURL is operator supplied with no platform restriction,
// so this package cannot assume the backend is not one of them. Splitting on
// both keeps `a\b` usable as an ordinary key while refusing `a\..\b`.
//
// The caller passes the DECODED path, which is what makes "%2e%2e" fail here
// too. It did not have to before: writing a path into url.URL.Path alone
// re-escaped the percent sign and left the backend with "%252e%252e". Now that
// setPath preserves a caller's escapes, so that an encoded slash can name one
// segment, an encoded traversal would be preserved with it.
//
// Segments of three or more dots are deliberately not matched. RFC 3986 section
// 5.2.4 defines dot-segment removal over exactly "." and "..", so "..." is an
// ordinary segment name rather than a traversal.
func hasDotSegment(p string) bool {
	isSep := func(r rune) bool { return r == '/' || r == '\\' }
	for _, seg := range strings.FieldsFunc(p, isSep) {
		if seg == "." || seg == ".." {
			return true
		}
	}
	return false
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

// readErrorBody reads at most max bytes of a failing response's body, for
// Config.ErrorDetail.
//
// It truncates where readBounded rejects, and swallows a read error rather than
// reporting it, because the status classification is the answer the caller
// needs: a backend that answers 403 with a gigabyte of HTML must not be able to
// turn that 403 into "response exceeds limit", nor to defeat the memory ceiling
// MaxBody exists to enforce. A detail is a nicety; the kind is not.
func readErrorBody(r io.Reader, max int64) []byte {
	b, err := io.ReadAll(io.LimitReader(r, max))
	if err != nil {
		return nil
	}
	return b
}

// redactURL renders a URL for one of this Client's error messages, applying the
// Client's [Config.RedactPath] to the path.
func (c *Client) redactURL(u *url.URL) string {
	return redactURLWith(u, c.redactPath)
}

// redactURLWith renders a URL for an error message with its query and userinfo
// stripped, because QueryAuth puts a credential in the query and a URL can
// carry userinfo, and with its path passed through redactPath.
//
// A nil redactPath renders the path verbatim, exactly as this did before the
// hook existed. It takes the hook as an argument rather than reading it off a
// Client because checkTokenURLScheme renders a URL before
// OAuth2ClientCredentials has built one, and that site needs the hook too:
// [OAuth2Config.RedactPath] hands it over directly.
//
// The hook's result is concatenated rather than assigned back to the copy's
// Path, because url.URL.String escapes Path on the way out: a placeholder like
// "<account>" assigned there would render as "%3Caccount%3E". Clearing the path
// and appending leaves exactly what the provider wrote. The fragment is cleared
// with it, since keeping it would render it BEFORE the appended path; no URL
// Client.build produces has one, and a fragment never reaches the wire anyway.
func redactURLWith(u *url.URL, redactPath func(string) string) string {
	c := *u
	c.RawQuery = ""
	c.User = nil
	if redactPath == nil {
		return c.String()
	}
	redacted := redactPath(u.EscapedPath())
	c.Path, c.RawPath = "", ""
	c.Fragment, c.RawFragment = "", ""
	return c.String() + redacted
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
//
// The URL it is rebuilt with goes through [Config.RedactPath] like every other
// rendered URL. Skipping it here would leave the hook trivially defeatable: the
// path a provider asked to have hidden would still be one errors.As away, and it
// would render into the very same message through the wrapped cause.
func (c *Client) redactTransportError(err error, u *url.URL) error {
	var ue *url.Error
	if !errors.As(err, &ue) {
		return err
	}
	return &url.Error{Op: ue.Op, URL: c.redactURL(u), Err: ue.Err}
}
