package cloudflarekv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providers/httpcore"
)

// bulkMaxKeys is the maximum number of keys Cloudflare's bulk/get endpoint
// accepts in a single request. ResolveBatch chunks any namespace with more
// keys than this into multiple requests and merges the results: silently
// truncating at this ceiling would drop every key past the 100th with no
// error at all.
const bulkMaxKeys = 100

// errBodyLimit bounds how much of a failing response body reaches an error
// string, so a hostile or broken upstream cannot put an unbounded response
// into one. httpcore hands errorDetail at most Config.MaxBody bytes and always
// drains the rest on every branch, so the connection-reuse discipline this
// file used to hand-roll per branch now comes from one place; this constant is
// only about how much of that body is worth quoting.
const errBodyLimit = 4096

// maxBodyBytes is the response ceiling for a successful read. It is set
// explicitly rather than left at httpcore's 1 MiB default because a Workers KV
// value may be up to 25 MiB, and this provider read the value body unbounded
// before it moved onto httpcore: a smaller ceiling would turn a legal, if
// large, value into a resolve failure.
const maxBodyBytes = 25 << 20

// Placeholders substituted for the account and namespace ids in an error
// message. See redactPath.
const (
	accountPlaceholder   = "<account>"
	namespacePlaceholder = "<namespace>"
)

// Resolve fetches the value for ref from Workers KV.
//
// There is deliberately no cache and no TTL: mamori.Refresh and mamori.Doctor
// both call Resolve directly, and unlike providers/vercel-gc - which gates a
// held snapshot on a store digest - Workers KV exposes no digest or ETag for
// this provider to gate a cache on. Every call is a live GET against the
// current value.
func (p *Provider) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
	s, err := p.settingsFor(ref)
	if err != nil {
		return mamori.Value{}, err
	}
	key, err := keyOf(ref)
	if err != nil {
		return mamori.Value{}, err
	}
	b, err := p.get(ctx, s, key)
	if err != nil {
		return mamori.Value{}, err
	}
	return valueFor(b, ref, s.namespace)
}

// bulkGetRequestBody is the body POSTed to the bulk/get endpoint. Type is
// always "text": without it Cloudflare may attempt to JSON-parse a stored
// value and return an object rather than the string bytes this provider
// treats as opaque.
type bulkGetRequestBody struct {
	Keys []string `json:"keys"`
	Type string   `json:"type"`
}

// bulkGetResponseBody is the JSON envelope every bulk/get response wraps its
// values in - unlike the single-key GET endpoint's raw bytes (see get's doc
// comment). A key absent from the namespace is simply absent from Values,
// with no per-key error, which is what lets ResolveBatch treat an absent key
// as a plain map miss.
type bulkGetResponseBody struct {
	Success bool `json:"success"`
	Result  struct {
		Values map[string]string `json:"values"`
	} `json:"result"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// ResolveBatch resolves every ref in one bulk request per namespace, each
// chunked to at most bulkMaxKeys keys since that is the largest the bulk/get
// endpoint accepts. mamori calls it automatically on the Load path so a Load
// with many refs costs a handful of requests rather than one GET per field.
//
// A key absent from a namespace's bulk response is omitted from the result
// map rather than failing the batch, per the BatchProvider contract, so
// mamori applies that field's default. The same holds for a ref whose #field
// selection is absent from an otherwise-present JSON value: valueFor reports
// that case as an error satisfying mamori.ErrNotFound, and it is treated
// exactly like an absent key here, matching Resolve. This split is
// deliberate: providers/vercel-gc originally returned that error verbatim
// from its ResolveBatch, which failed the entire batch over one missing
// optional field and took every sibling ref down with it. An ErrInvalid from
// selection (for example selecting a #field of a value that is not a JSON
// object) is a different class of problem - a malformed request against the
// payload, not an absence - and still fails the batch, as does a namespace
// that fails for any reason other than a plain 404.
//
// A 404 on a namespace's bulk request is that same not-an-absence-failure
// split applied one level up: bulkGet reports it as mamori.ErrNotFound, and
// the chunk loop below skips that chunk's keys - leaving their refs absent
// from the result map, exactly like an absent key - instead of returning the
// error and failing every sibling ref in the batch, in this namespace or any
// other. A single misconfigured namespace among many refs must not be able to
// take the whole batch down with it, any more than a missing optional field
// can.
func (p *Provider) ResolveBatch(ctx context.Context, refs []mamori.Ref) (map[string]mamori.Value, error) {
	if len(refs) == 0 {
		return map[string]mamori.Value{}, nil
	}

	// Group refs by namespace, deduplicating keys within a namespace so two
	// refs selecting different #fields of the same key (e.g. api-config#a and
	// api-config#b) cost one bulk-request slot rather than two.
	type namespaceGroup struct {
		settings settings
		keys     []string // deduplicated, first-seen order
		byKey    map[string][]mamori.Ref
	}
	groups := map[string]*namespaceGroup{}

	for _, r := range refs {
		s, err := p.settingsFor(r)
		if err != nil {
			return nil, err
		}
		key, err := keyOf(r)
		if err != nil {
			return nil, err
		}
		g, ok := groups[s.namespace]
		if !ok {
			g = &namespaceGroup{settings: s, byKey: map[string][]mamori.Ref{}}
			groups[s.namespace] = g
		}
		if _, seen := g.byKey[key]; !seen {
			g.keys = append(g.keys, key)
		}
		g.byKey[key] = append(g.byKey[key], r)
	}

	out := make(map[string]mamori.Value, len(refs))
	for ns, g := range groups {
		for start := 0; start < len(g.keys); start += bulkMaxKeys {
			end := min(start+bulkMaxKeys, len(g.keys))
			chunkKeys := g.keys[start:end]

			values, err := p.bulkGet(ctx, g.settings, chunkKeys)
			if err != nil {
				if errors.Is(err, mamori.ErrNotFound) {
					// The namespace itself does not exist (see bulkGet's 404
					// branch); skip this chunk's keys so their refs fall back
					// to defaults, exactly as a namespace-not-found Resolve
					// lets its own caller's onfail/default handling take
					// over. This is a namespace-level decision, distinct from
					// the per-ref #field-not-found swallow around valueFor
					// below: one bad namespace must not fail every sibling
					// ref sharing this batch, in any other namespace or in
					// this one.
					continue
				}
				return nil, err
			}
			for _, key := range chunkKeys {
				b, ok := values[key]
				if !ok {
					continue // absent key; mamori applies the default
				}
				for _, r := range g.byKey[key] {
					v, err := valueFor(b, r, ns)
					if err != nil {
						if errors.Is(err, mamori.ErrNotFound) {
							continue // an absent selected field is still not-found; mamori applies the default
						}
						return nil, err
					}
					out[r.Raw] = v
				}
			}
		}
	}
	return out, nil
}

// bulkGet issues one authenticated POST against the bulk/get endpoint for
// keys (at most bulkMaxKeys of them) and returns the values that came back,
// keyed by the requested key. A key absent from the namespace is simply
// absent from the returned map; the caller (ResolveBatch) treats that as an
// omission, not an error.
func (p *Provider) bulkGet(ctx context.Context, s settings, keys []string) (map[string][]byte, error) {
	reqBody, err := json.Marshal(bulkGetRequestBody{Keys: keys, Type: "text"})
	if err != nil {
		return nil, fmt.Errorf("mamori/cloudflare-kv: encoding bulk request: %w", err)
	}

	client, err := p.clientFor(s)
	if err != nil {
		return nil, err
	}

	resp, err := client.Do(ctx, httpcore.Request{
		Method: http.MethodPost,
		Path:   namespacePath(s) + "/bulk/get",
		Header: http.Header{"Content-Type": {"application/json"}},
		Body:   reqBody,
	})
	if err != nil {
		if errors.Is(err, mamori.ErrNotFound) {
			// Unlike get, an absent key never reaches this branch: the bulk
			// endpoint has no per-key 404 - a missing key is simply omitted
			// from a successful response's Values map (see
			// bulkGetResponseBody's doc comment). A 404 here can therefore
			// only mean the namespace itself does not exist, and it gets its
			// own message for that, exactly as get's namespace-not-found case
			// does. ResolveBatch's call site treats this the same way it
			// treats an absent key: this chunk's keys are skipped so their
			// refs fall back to their defaults, rather than one bad namespace
			// failing every sibling ref in the batch.
			return nil, fmt.Errorf("mamori/cloudflare-kv: namespace not found for bulk get of %d key(s): %w", len(keys), mamori.ErrNotFound)
		}
		// One %w: the cause already carries the sentinel httpcore classified
		// it with, and adding a second would replace that kind rather than
		// duplicate it.
		return nil, fmt.Errorf("mamori/cloudflare-kv: bulk get of %d key(s): %w", len(keys), err)
	}

	var body bulkGetResponseBody
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		return nil, fmt.Errorf("mamori/cloudflare-kv: decoding bulk response: %w: %w", mamori.ErrInvalid, err)
	}
	if !body.Success {
		// A 200 status carrying "success":false is Cloudflare's own reported
		// failure, distinct from an HTTP-level status code, and is a malformed
		// request against the API rather than an absence.
		msg := "bulk get reported failure"
		if len(body.Errors) > 0 {
			msg = body.Errors[0].Message
		}
		return nil, fmt.Errorf("mamori/cloudflare-kv: %s: %w", msg, mamori.ErrInvalid)
	}

	values := make(map[string][]byte, len(body.Result.Values))
	for k, v := range body.Result.Values {
		values[k] = []byte(v)
	}
	return values, nil
}

// get issues one authenticated GET against the single-key value endpoint and
// returns the response body.
//
// This endpoint is the API's first surprise: a single-key GET responds with
// the value's raw stored bytes, not a JSON envelope. Task 3's bulk endpoint,
// by contrast, wraps every value inside {"result":{"values":{...}}}. There is
// no unwrapping step here because there is no envelope to unwrap: whatever
// bytes Cloudflare stored are the bytes this returns, verbatim.
//
// key is url.PathEscape'd before it is built into the URL. Workers KV keys
// are up to 512 bytes of any printable, non-whitespace character, so a key
// like "config/prod/log-level" must travel as one escaped path segment
// (config%2Fprod%2Flog-level) rather than as three, which is why keyOf takes
// the entire ref path as one key and never splits it on '/'.
func (p *Provider) get(ctx context.Context, s settings, key string) ([]byte, error) {
	client, err := p.clientFor(s)
	if err != nil {
		return nil, err
	}

	resp, err := client.Do(ctx, httpcore.Request{
		Path: namespacePath(s) + "/values/" + url.PathEscape(key),
	})
	if err != nil {
		if errors.Is(err, mamori.ErrNotFound) {
			// Either the key or the namespace is absent; the response does not
			// reliably distinguish them, which is the caveat this provider's
			// package doc names rather than hides.
			return nil, fmt.Errorf("mamori/cloudflare-kv: key %q not found: %w", key, mamori.ErrNotFound)
		}
		// One %w: the cause already carries the sentinel httpcore classified
		// it with, and adding a second would replace that kind rather than
		// duplicate it.
		return nil, fmt.Errorf("mamori/cloudflare-kv: reading key %q: %w", key, err)
	}
	return resp.Body, nil
}

// namespacePath is the escaped path prefix addressing one namespace, shared by
// the single-key and bulk endpoints.
//
// Both ids are url.PathEscape'd. The namespace is ref-controlled through
// ?namespace=, so without the escape a namespace containing "/" would produce
// a request path addressing an entirely different endpoint. httpcore joins
// Request.Path in its escaped form precisely so that works, and rejects a "."
// or ".." segment (literal or percent-encoded) before anything is sent, so a
// traversal payload cannot escape the base URL's prefix either.
func namespacePath(s settings) string {
	return "/accounts/" + url.PathEscape(s.account) +
		"/storage/kv/namespaces/" + url.PathEscape(s.namespace)
}

// clientFor builds the httpcore client for one read.
//
// It is built per call rather than cached because the token, the account id
// and the namespace are resolved per ref, reading the environment lazily (see
// settingsFor): caching would pin whichever set happened to be resolved first,
// and the namespace in particular is allowed to differ from one ref to the
// next. Construction performs no network call and reuses the provider's
// *http.Client, so the connection pool is shared across every read.
func (p *Provider) clientFor(s settings) (*httpcore.Client, error) {
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if closed {
		return nil, fmt.Errorf("mamori/cloudflare-kv: provider is closed: %w", mamori.ErrUnavailable)
	}

	c, err := httpcore.New(httpcore.Config{
		BaseURL:     p.baseURL,
		HTTPClient:  p.httpClient,
		Auth:        httpcore.Bearer(s.token),
		MaxBody:     maxBodyBytes,
		ErrorDetail: errorDetail,
		RedactPath:  redactPath(s),
	})
	if err != nil {
		// No redaction here: this error echoes the operator's configured
		// BaseURL, which carries neither id, and a BaseURL that failed to parse
		// has no path for a path hook to work on.
		return nil, fmt.Errorf("mamori/cloudflare-kv: building the API client: %w", err)
	}
	return c, nil
}

// errorDetail is httpcore's Config.ErrorDetail hook: it quotes a bounded
// prefix of a failing response's body into the error message.
//
// Embedding the body verbatim, bounded by errBodyLimit, is a deliberate house
// convention shared with providers/doppler, providers/vercel-gc and
// providers/scaleway-sm: the verbatim text is what actually tells an operator
// what the upstream rejected the request for, and the bound is what stops a
// hostile or broken upstream turning that diagnostic into an unbounded string.
// httpcore calls this only for a status it has already decided is a failure,
// never for the 200 whose body IS the stored value.
func errorDetail(_ int, body []byte) string {
	if len(body) > errBodyLimit {
		body = body[:errBodyLimit]
	}
	return strings.TrimSpace(string(body))
}

// redactPath returns httpcore's Config.RedactPath hook for s: it substitutes
// placeholders for the account id and the namespace id in the request path
// httpcore renders into every error it returns.
//
// httpcore strips the query string and any userinfo from that URL but renders
// the path, which is right for a provider whose path carries only a key name
// and wrong for this one: a Workers KV URL carries the account id and the
// namespace id as ordinary path segments. This package guarantees neither
// reaches an error's text (see TestResolveTransportErrorNeverLeaksCredentials),
// and without this hook an ordinary refused connection would put both into a
// log line. The API token needs no such treatment: it travels in the
// Authorization header, which is never rendered.
//
// Substituting before httpcore composes the message, rather than rewriting the
// finished message afterwards as this package used to, is what keeps the error
// chain untouched: errors.Is still reaches httpcore's sentinel and still
// reaches the transport's own cause through the *url.Error httpcore preserves,
// because nothing is rewritten at all. httpcore applies the hook to that
// rebuilt *url.Error too, so the ids cannot be read back out through the chain.
//
// Both the raw and the url.PathEscape'd form of each id are replaced, since the
// path carries the escaped one, and the longest string is replaced first so that
// an id which is a prefix of the other cannot leave a fragment of it behind.
func redactPath(s settings) func(string) string {
	subs := []struct{ from, to string }{
		{s.account, accountPlaceholder},
		{url.PathEscape(s.account), accountPlaceholder},
		{s.namespace, namespacePlaceholder},
		{url.PathEscape(s.namespace), namespacePlaceholder},
	}
	slices.SortFunc(subs, func(a, b struct{ from, to string }) int {
		return len(b.from) - len(a.from)
	})

	return func(path string) string {
		for _, sub := range subs {
			if sub.from != "" {
				path = strings.ReplaceAll(path, sub.from, sub.to)
			}
		}
		return path
	}
}

// valueFor turns the raw response body into a mamori.Value, applying the
// ref's #key selection when present.
//
// There is no unwrapping step analogous to providers/vercel-gc's JSON-item
// decoding: Workers KV stores opaque bytes, not a JSON envelope around each
// value, so whatever SelectKey returns (or the untouched body, when ref.Key
// is empty) is the final byte payload.
func valueFor(b []byte, ref mamori.Ref, namespace string) (mamori.Value, error) {
	if ref.Key != "" {
		sel, err := mamori.SelectKey(b, ref.Key)
		if err != nil {
			return mamori.Value{}, err
		}
		b = sel
	}
	return mamori.Value{
		Bytes:     b,
		Version:   mamori.VersionHash(b),
		Sensitive: false,
		Metadata: map[string]string{
			"namespace": namespace,
		},
	}, nil
}

// Status classification comes from httpcore.ClassifyStatus, inside
// httpcore.Client.Do, rather than from a mapping copied into this package. 404
// is the one status get and bulkGet each take back, to give it a message of
// their own.
//
// One caveat of that 404 is worth being honest about, because it is the kind
// of thing a misconfiguration hides behind rather than announces: a 404 from
// this API means either an absent key or an absent namespace, and Cloudflare's
// response does not reliably distinguish the two, so a namespace id that is
// simply wrong presents as every key in it silently falling back to its
// default, exactly like a genuinely absent key would. Cloudflare has not
// published a stable enough error-code vocabulary in the response body to key
// anything on but the status.
