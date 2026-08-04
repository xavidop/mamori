// This file implements the admin-endpoint client shared by doctor and
// status (decision D1's other half: the live commands never load or read
// source, they only ever GET / on a running process's admin endpoint and
// render whatever mamori.Report comes back). Endpoint parsing mirrors
// providers/mamori's parseEndpoint (unix:///path, https://host:port,
// http://host:port only with --insecure) since both clients are reaching
// the same admin surface (handler.go) over the same three endpoint forms.
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/xavidop/mamori"
)

// Exit codes for the live commands (doctor, status). fetchReport is the
// single place that decides which of these applies to a given GET /
// attempt; doctorCmd/statusCmd return whatever it (or doFetch, which it
// wraps) produces.
const (
	exitHealthy      = 0 // 200, decodes as a Report, Healthy true
	exitUnhealthy    = 1 // 200, decodes as a Report, Healthy false
	exitNotAdminAPI  = 2 // reachable, but 404 or a 200 that is not a Report
	exitUnreachable  = 3 // never got an HTTP response at all
	exitUnauthorized = 4 // 401: reachable mamori admin API, auth failed
)

// dummyUnixHost is the fixed host used in request URLs when the endpoint is
// a Unix domain socket. The transport's DialContext ignores the address it
// is given and always dials the configured socket path, so the host in the
// URL is never actually resolved; it exists only because net/http requires
// every request URL to have one. Named identically to providers/mamori's
// own constant of the same purpose, since both exist for the same reason.
const dummyUnixHost = "unix"

// fetchTimeout bounds a single GET / attempt. The admin endpoint's own
// routes are cheap, bounded reads (see handler.go), so there is no
// legitimate reason for a healthy admin process to take long to answer;
// this exists so an unresponsive (not merely refused) endpoint still fails
// in bounded time instead of hanging the CLI forever.
const fetchTimeout = 10 * time.Second

// parseEndpoint parses a live command's --endpoint flag into a base URL and
// an *http.Transport suitable for reaching it. It accepts exactly the three
// forms documented on liveFlags.endpoint (see flags.go), mirroring
// providers/mamori's parseEndpoint (mamori.go) since both are clients of
// the same admin surface:
//
//   - "unix:///path/to.sock": a transport whose DialContext always dials
//     the Unix socket at that path, and the fixed base URL "http://unix"
//     (the real path is baked into DialContext; the host in request URLs
//     is never actually used to resolve anything).
//   - "https://host:port": the endpoint itself as the base URL and a bare
//     *http.Transport (the caller may attach a *tls.Config for mTLS).
//   - "http://host:port": the endpoint as the base URL and a bare
//     transport, but ONLY when insecure is true; otherwise an error.
//
// An empty or unparseable endpoint, or any other scheme, is also an error.
func parseEndpoint(endpoint string, insecure bool) (baseURL string, transport *http.Transport, err error) {
	if endpoint == "" {
		return "", nil, fmt.Errorf("mamori: empty --endpoint")
	}

	u, err := url.Parse(endpoint)
	if err != nil {
		return "", nil, fmt.Errorf("mamori: parsing --endpoint %q: %w", endpoint, err)
	}

	switch u.Scheme {
	case "unix":
		socketPath := u.Path
		if socketPath == "" {
			return "", nil, fmt.Errorf("mamori: unix endpoint %q has no socket path", endpoint)
		}
		dialer := &net.Dialer{}
		t := &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dialer.DialContext(ctx, "unix", socketPath)
			},
		}
		return "http://" + dummyUnixHost, t, nil

	case "https":
		return strings.TrimRight(endpoint, "/"), &http.Transport{}, nil

	case "http":
		if !insecure {
			return "", nil, fmt.Errorf("mamori: plaintext --endpoint %q refused; pass --insecure to allow it", endpoint)
		}
		return strings.TrimRight(endpoint, "/"), &http.Transport{}, nil

	default:
		return "", nil, fmt.Errorf("mamori: unsupported --endpoint scheme %q in %q (want unix://, https://, or http://)", u.Scheme, endpoint)
	}
}

// reportFields is the set of top-level JSON keys every mamori.Report has
// carried since the admin endpoint existed: Report has no struct json tags
// and none of these fields carries "omitempty" (see status.go in the core
// module), so encoding/json emits all six unconditionally, regardless of
// value. That makes their presence a reliable sentinel for "this body is a
// Report", including the all-zero-value case: a genuine, all-unhealthy
// Report still has all six keys, while a bare {} has none and an unrelated
// document (e.g. {"hello":"world"}) has the wrong ones. decodeReport uses
// this to reject both without mistaking either for a real (if unhealthy)
// Report.
var reportFields = map[string]struct{}{
	"Fields": {}, "Snapshot": {}, "Live": {}, "Pinned": {}, "Healthy": {}, "GeneratedAt": {},
}

// reportOptionalFields are top-level keys a Report may or may not carry,
// because the mamori version running in the target process decides.
//
// The CLI and the process it points at are separate binaries on separate
// release cadences: an operator debugging an incident runs whichever mamori
// they have against whichever the pod was built with. Requiring an exact key
// set made every field ever added to Report a hard break in both directions -
// a new CLI would reject a running process for lacking a key it never had,
// and an old one would reject a healthy report for carrying a key it does not
// know. Splitting required from optional keeps the sentinel (a bare {} or an
// unrelated payload still has none of the required keys) while letting the
// two versions differ. A key in neither set is still rejected, so this is not
// a blanket "ignore what you do not understand".
//
// Add a new Report field here, not to reportFields above, unless every
// supported mamori version emits it.
var reportOptionalFields = map[string]struct{}{
	"Source": {}, "Bootstrap": {},
}

// decodeReport strictly decodes body as a mamori.Report: it first checks that
// the top-level JSON object carries every key in reportFields and nothing
// outside reportFields plus reportOptionalFields, then decodes for real. This
// is what lets fetchReport tell a genuine admin Report (any Healthy value,
// including the current process being unhealthy) apart from a 200 that merely
// happens to also be a JSON object: a bare {}, or an unrelated service's
// unrelated JSON payload, both fail the key check and are reported as "not a
// Report" (exit 2) rather than silently decoding into a zero-value Report that
// would misclassify as unhealthy (exit 1) or, worse, healthy (exit 0).
func decodeReport(body []byte) (*mamori.Report, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("response body is not a JSON object: %w", err)
	}
	for k := range reportFields {
		if _, ok := raw[k]; !ok {
			return nil, fmt.Errorf("response body is missing top-level field %q, which every mamori.Report carries", k)
		}
	}
	for k := range raw {
		if _, ok := reportFields[k]; ok {
			continue
		}
		if _, ok := reportOptionalFields[k]; ok {
			continue
		}
		return nil, fmt.Errorf("response body has unexpected top-level field %q, not part of the mamori.Report contract", k)
	}

	var rep mamori.Report
	if err := json.Unmarshal(body, &rep); err != nil {
		return nil, fmt.Errorf("decoding response body as mamori.Report: %w", err)
	}
	return &rep, nil
}

// fetchResult is the outcome of one doFetch attempt: the classified exit
// code and error (see the exit* constants), the decoded Report when one
// could be decoded, and the exact raw response body doFetch read (nil only
// when no HTTP response was ever obtained at all, i.e. exit 3). doctorCmd's
// --json passthrough uses body directly rather than re-marshaling rep, so
// what it prints is byte-for-byte what the server sent, not the CLI's own
// re-encoding of its parsed understanding of that body.
type fetchResult struct {
	body []byte
	rep  *mamori.Report
	exit int
	err  error
}

// doFetch performs a single GET / against f's endpoint and classifies the
// outcome. fetchReport (below) is the public wrapper matching the task's
// specified signature; doctorCmd and statusCmd call doFetch directly so
// they can also access the raw response body (for --json) without a second
// round trip.
func doFetch(ctx context.Context, f liveFlags) fetchResult {
	baseURL, transport, err := parseEndpoint(f.endpoint, f.insecure)
	if err != nil {
		// No client could even be built: this is the same "never got an
		// HTTP response" outcome as a refused connection or a TLS
		// handshake failure, just discovered earlier.
		return fetchResult{exit: exitUnreachable, err: err}
	}

	header, tlsCerts, err := f.resolveCredential()
	if err != nil {
		return fetchResult{exit: exitUnreachable, err: err}
	}
	if len(tlsCerts) > 0 {
		if transport.TLSClientConfig == nil {
			transport.TLSClientConfig = &tls.Config{}
		}
		transport.TLSClientConfig.Certificates = tlsCerts
	}

	client := &http.Client{Timeout: fetchTimeout, Transport: transport}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/", nil)
	if err != nil {
		return fetchResult{exit: exitUnreachable, err: fmt.Errorf("mamori: building request: %w", err)}
	}
	if header != "" {
		req.Header.Set("Authorization", header)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fetchResult{exit: exitUnreachable, err: fmt.Errorf("mamori: connecting to %s: %w", f.endpoint, err)}
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fetchResult{exit: exitUnreachable, err: fmt.Errorf("mamori: reading response from %s: %w", f.endpoint, err)}
	}

	switch resp.StatusCode {
	case http.StatusOK:
		rep, derr := decodeReport(body)
		if derr != nil {
			return fetchResult{
				body: body,
				exit: exitNotAdminAPI,
				err: fmt.Errorf(
					"mamori: %s returned 200 but the body is not a mamori admin Report (%v); "+
						"this usually means the target process has no mamori.WithAdminHTTP configured, "+
						"or is not a mamori process at all", f.endpoint, derr),
			}
		}
		exit := exitHealthy
		if !rep.Healthy {
			exit = exitUnhealthy
		}
		return fetchResult{body: body, rep: rep, exit: exit}

	case http.StatusNotFound:
		return fetchResult{
			body: body,
			exit: exitNotAdminAPI,
			err: fmt.Errorf(
				"mamori: %s returned 404 on GET / -- the admin API is not enabled on the target process; "+
					"configure mamori.WithAdminHTTP", f.endpoint),
		}

	case http.StatusUnauthorized:
		return fetchResult{
			body: body,
			exit: exitUnauthorized,
			err: fmt.Errorf(
				"mamori: %s returned 401 -- reachable mamori admin API, but authentication failed; "+
					"check --bearer/--bearer-file, --basic/--basic-file, or --client-cert/--client-key", f.endpoint),
		}

	default:
		return fetchResult{
			body: body,
			exit: exitNotAdminAPI,
			err: fmt.Errorf(
				"mamori: %s returned unexpected status %d -- reachable, but not a usable admin Report",
				f.endpoint, resp.StatusCode),
		}
	}
}

// fetchReport GETs / on f's admin endpoint and classifies the result into
// one of the five live-command exit codes (see the exit* constants above):
// 0/1 for a decodable Report (healthy/unhealthy), 2 for a reachable
// endpoint that is not a usable mamori admin API (404, or a 200 that does
// not decode as a Report), 3 for never getting an HTTP response at all
// (refused connection, missing socket, TLS failure, or an endpoint/
// credential that could not even be turned into a request), and 4 for a
// 401.
func fetchReport(ctx context.Context, f liveFlags) (*mamori.Report, int, error) {
	res := doFetch(ctx, f)
	return res.rep, res.exit, res.err
}
