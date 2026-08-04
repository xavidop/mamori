// Package posthog implements a mamori provider for PostHog feature flags
// (https://posthog.com), resolving configuration values from PostHog's
// server-side flag evaluation endpoint.
//
// It registers the "posthog" scheme. Refs take the form:
//
//	posthog://<flag-key>[#enabled|#variant|#payload]
//
// where <flag-key> is the key of a feature flag in the PostHog project and the
// optional URL fragment selects which facet of the evaluated result is
// returned:
//
//	KillSwitch bool   `source:"posthog://new-checkout"`          // "true"/"false", or the variant key
//	Enabled    bool   `source:"posthog://new-checkout#enabled"`  // always "true"/"false"
//	Variant    string `source:"posthog://pricing-test#variant"`  // the variant key
//	Payload    string `source:"posthog://pricing-test#payload"`  // the flag's JSON payload
//
// # Evaluation
//
// Evaluation is a POST, not a GET: PostHog evaluates flags for a *distinct id*,
// so there is a request body to carry it. Each Resolve posts to
//
//	POST <host>/flags?v=2
//
// with a JSON body naming the project API key and the distinct id. See the
// module README for the documented request and response shapes and the
// documentation URLs they were taken from.
//
// One evaluation returns *every* flag for the distinct id, and this provider
// selects one of them by key. Resolving ten refs therefore performs ten
// evaluations, exactly as providers/growthbook and providers/flagsmith each
// re-read their whole feature set per resolve. That is deliberate: mamori's
// contract is that a Resolve observes the backend now, and a shared cache would
// make one field's poll interval silently govern another's freshness.
//
// # Value mapping
//
// PostHog's own SDKs return a boolean for a boolean flag and the variant key
// (a string) for a multivariate one, and the no-fragment form matches that
// convention exactly, so a ref reads the way the vendor's own client would:
//
//	flag shape           fragment    Value.Bytes
//	-----------------    --------    -----------------------------------------
//	boolean, enabled     (none)      "true"
//	boolean, disabled    (none)      "false"
//	multivariate         (none)      the variant key, e.g. "control"
//	any                  #enabled    "true"/"false", from the enabled field
//	any                  #variant    the variant key; empty for a boolean flag
//	any                  #payload    the flag's payload; empty when it has none
//
// A boolean flag is distinguished from a multivariate one by the presence of
// the response's "variant" field, which PostHog sends only for a multivariate
// flag. A disabled multivariate flag carries no variant either, so it renders
// as "false" like any other disabled flag rather than as an empty string.
//
// # Sensitivity and versioning
//
// Feature flags hold rollout and configuration state, not managed secrets, so
// resolved values are never marked Sensitive, matching providers/launchdarkly,
// providers/flagsmith, providers/growthbook and providers/unleash.
//
// Value.Version is a content hash of the resolved bytes (mamori.VersionHash).
// PostHog does expose a per-flag revision, metadata.version, but this provider
// deliberately does not use it: metadata.version counts edits to the flag's
// *definition*, while Value.Version must change whenever the *resolved bytes*
// change. A flag whose evaluation flips for this distinct id without a
// definition edit - a percentage rollout the id crosses, a person property that
// changed elsewhere, an experiment reassignment - keeps its metadata.version,
// so using it would make mamori miss a real change. The hash cannot.
//
// # Not found and other failures
//
// A flag PostHog did not return for this distinct id resolves to an error
// satisfying errors.Is(err, mamori.ErrNotFound), so mamori applies your default
// or optional handling. Two response-body conditions are deliberately NOT
// reported as not-found, because in both the flag's absence says nothing about
// whether it exists:
//
//   - "quotaLimited" naming feature_flags: the project is over its billing
//     quota and PostHog has paused flag evaluation, answering 200 with an empty
//     flags object. Reported as mamori.ErrRateLimited.
//   - "errorsWhileComputingFlags": PostHog could not compute some flags for
//     this request. Reported as mamori.ErrUnavailable when the requested flag is
//     among the missing.
//
// Transport and status failures are classified by httpcore.ClassifyStatus, so
// a 401 is unauthenticated, a 403 permission denied, a 429 rate limited, and so
// on. No response body ever reaches an error message: httpcore's ErrorDetail
// hook is left nil, which is what guarantees a flag payload cannot be
// exfiltrated through a log line.
//
// # Authentication
//
// PostHog's flag endpoint takes no Authorization header. The credential is the
// *project API key*, a public client-side token, sent in the request body as
// "api_key". It is supplied with WithProjectAPIKey or, when unset, read lazily
// from POSTHOG_PROJECT_API_KEY at first resolve. It is never placed in a URL,
// an error, a log line, or a mamori Report.
//
// # Watch
//
// PostHog exposes no per-flag change notification for this endpoint, so this
// provider is intentionally not watchable; mamori wraps it in its polling
// adapter automatically.
package posthog

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"sync"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providers/httpcore"
)

// scheme is the URL scheme this provider handles.
const scheme = "posthog"

// DefaultHost is the PostHog US Cloud ingestion host, used when neither
// WithHost nor POSTHOG_HOST names one. PostHog documents three choices -
// https://us.i.posthog.com, https://eu.i.posthog.com, and a self-hosted domain
// - and US Cloud is the default a new project is created on.
const DefaultHost = "https://us.i.posthog.com"

// DefaultDistinctID is the distinct id flags are evaluated for when neither
// WithDistinctID nor POSTHOG_DISTINCT_ID supplies one.
//
// A stable, non-empty id gives deterministic evaluations for the
// configuration-style flags a mamori ref names: a percentage rollout hashes the
// distinct id, so a random id per process would put two replicas of the same
// service on different sides of the same 50% rollout. It matches
// providers/launchdarkly's defaultContextKey, which is "mamori" for the same
// reason.
const DefaultDistinctID = "mamori"

// flagsPath is the evaluation endpoint. PostHog has changed this endpoint over
// time; /flags with v=2 is the current documented form, superseding /decide.
const flagsPath = "/flags"

// flagsVersion is the value of the "v" query parameter that selects the v2
// response envelope, the one whose per-flag objects carry enabled, variant and
// metadata. Without it PostHog answers with the older flat featureFlags map,
// which cannot distinguish a disabled flag from an absent one.
const flagsVersion = "2"

// quotaLimitedFlags is the value PostHog puts in the response's quotaLimited
// array when a project is over its billing quota and flag evaluation has been
// paused.
const quotaLimitedFlags = "feature_flags"

// Fragments naming a facet of the evaluated flag. Any other fragment is
// rejected rather than guessed at, so a typo fails loudly instead of resolving
// to an empty string.
const (
	// fragEnabled selects the flag's enabled state regardless of its shape.
	fragEnabled = "enabled"
	// fragVariant selects a multivariate flag's variant key.
	fragVariant = "variant"
	// fragPayload selects the flag's payload.
	fragPayload = "payload"
)

// flagResult is one flag's evaluated state in a v2 /flags response.
type flagResult struct {
	Key     string `json:"key"`
	Enabled bool   `json:"enabled"`
	// Variant is present only for a multivariate flag that evaluated to a
	// variant, which is exactly what distinguishes the two flag shapes.
	Variant  string `json:"variant"`
	Metadata struct {
		// Payload is documented as a JSON-encoded string, but is kept raw so a
		// backend that inlines the JSON object instead is not mangled; see
		// payloadBytes.
		Payload json.RawMessage `json:"payload"`
	} `json:"metadata"`
}

// flagsEnvelope is a v2 /flags response.
type flagsEnvelope struct {
	Flags map[string]flagResult `json:"flags"`
	// ErrorsWhileComputingFlags reports that PostHog failed to compute some of
	// this request's flags, which makes an absent flag inconclusive rather than
	// missing.
	ErrorsWhileComputingFlags bool `json:"errorsWhileComputingFlags"`
	// QuotaLimited names resources paused for billing reasons. PostHog answers
	// 200 with an empty flags object in that state, so this field is the only
	// thing distinguishing it from a project with no flags.
	QuotaLimited []string `json:"quotaLimited"`
}

// flagsRequest is the POST body PostHog's flag endpoint documents.
type flagsRequest struct {
	// APIKey is the project API key. It is the only credential this endpoint
	// takes; there is no Authorization header.
	APIKey     string `json:"api_key"`
	DistinctID string `json:"distinct_id"`
	// Groups maps a PostHog group type to a group id, for group-based flags.
	Groups map[string]string `json:"groups,omitempty"`
	// PersonProperties are evaluated against a flag's person-property
	// conditions without requiring PostHog to have seen the person before.
	PersonProperties map[string]any `json:"person_properties,omitempty"`
	// GroupProperties are the same, per group type.
	GroupProperties map[string]map[string]any `json:"group_properties,omitempty"`
}

// Provider resolves posthog:// refs by evaluating PostHog feature flags. It is
// safe for concurrent use. The HTTP client is built lazily on first Resolve, so
// registering the provider never contacts PostHog and never fails for lack of
// configuration.
type Provider struct {
	apiKey           string
	host             string
	distinctID       string
	groups           map[string]string
	personProperties map[string]any
	groupProperties  map[string]map[string]any
	httpClient       *http.Client
	maxBody          int64

	mu  sync.Mutex
	cli *httpcore.Client
}

// Option configures a Provider.
type Option func(*Provider)

// WithProjectAPIKey sets the PostHog project API key sent as the request body's
// api_key field. It overrides POSTHOG_PROJECT_API_KEY.
func WithProjectAPIKey(key string) Option {
	return func(p *Provider) { p.apiKey = key }
}

// WithHost points the provider at a PostHog host: https://us.i.posthog.com,
// https://eu.i.posthog.com, or a self-hosted domain. It overrides POSTHOG_HOST.
// Sending flag evaluations to the wrong region answers as though every flag were
// absent, so naming the region explicitly is worth doing.
func WithHost(host string) Option {
	return func(p *Provider) { p.host = host }
}

// WithDistinctID sets the distinct id flags are evaluated for. It overrides
// POSTHOG_DISTINCT_ID and defaults to DefaultDistinctID.
//
// It is provider-level rather than part of a ref because a distinct id
// identifies the evaluation context, not the flag: every ref this provider
// resolves is evaluated for the same subject, so repeating it in each ref would
// be noise that could silently disagree between two fields.
func WithDistinctID(id string) Option {
	return func(p *Provider) {
		if id != "" {
			p.distinctID = id
		}
	}
}

// WithGroups sets the groups (PostHog group type to group id) that group-based
// flags are evaluated against. Without it, a group-based flag evaluates as
// though the subject belonged to no group.
func WithGroups(groups map[string]string) Option {
	return func(p *Provider) { p.groups = cloneStringMap(groups) }
}

// WithPersonProperties supplies person properties for flags whose release
// conditions target them. Sending them explicitly lets a flag evaluate
// correctly for a distinct id PostHog has never seen an event from, which is
// the normal case for a service process.
func WithPersonProperties(props map[string]any) Option {
	return func(p *Provider) { p.personProperties = cloneAnyMap(props) }
}

// WithGroupProperties supplies per-group-type properties for group-based flags
// whose release conditions target group properties.
func WithGroupProperties(props map[string]map[string]any) Option {
	return func(p *Provider) {
		if props == nil {
			p.groupProperties = nil
			return
		}
		out := make(map[string]map[string]any, len(props))
		for k, v := range props {
			out[k] = cloneAnyMap(v)
		}
		p.groupProperties = out
	}
}

// WithHTTPClient injects the HTTP client used for evaluations, for a custom
// transport, proxy, or timeout.
func WithHTTPClient(c *http.Client) Option {
	return func(p *Provider) { p.httpClient = c }
}

// WithMaxResponseBytes caps how much of an evaluation response is read,
// overriding httpcore.DefaultMaxBody (1 MiB).
//
// This provider needs the knob where a single-key provider does not: one
// evaluation returns EVERY flag in the project, so the response grows with the
// project rather than with the ref, and a project whose flag payloads exceed the
// ceiling would fail every resolve with no way to raise it.
func WithMaxResponseBytes(n int64) Option {
	return func(p *Provider) { p.maxBody = n }
}

// New constructs a PostHog provider. Configuration missing at construction is
// read from the environment at first resolve, so New never fails and never
// contacts PostHog.
//
// Users who need explicit configuration call
// mamori.WithProvider(posthog.New(posthog.WithProjectAPIKey("phc_..."))).
func New(opts ...Option) *Provider {
	p := &Provider{}
	for _, o := range opts {
		o(p)
	}
	return p
}

// init registers a lazily-initialized provider so `import _` wiring works from
// the ambient POSTHOG_* environment.
func init() { mamori.Register(New()) }

// Scheme returns "posthog".
func (p *Provider) Scheme() string { return scheme }

// client returns the HTTP client, building it lazily from the configured host
// on first use. Concurrent callers share one client.
func (p *Provider) client() (*httpcore.Client, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cli != nil {
		return p.cli, nil
	}
	host := p.host
	if host == "" {
		host = os.Getenv("POSTHOG_HOST")
	}
	if host == "" {
		host = DefaultHost
	}
	c, err := httpcore.New(httpcore.Config{
		BaseURL:    host,
		HTTPClient: p.httpClient,
		MaxBody:    p.maxBody,
		// Auth is nil and ErrorDetail is nil, both deliberately. PostHog's flag
		// endpoint authenticates from the request body, not a header, and a
		// nil ErrorDetail is the guarantee that no response body - which for
		// this endpoint is a set of flag payloads - can reach an error message.
	})
	if err != nil {
		return nil, fmt.Errorf("mamori/posthog: building the HTTP client for host %q: %w", host, err)
	}
	p.cli = c
	return c, nil
}

// projectAPIKey returns the configured project API key, falling back to
// POSTHOG_PROJECT_API_KEY. The key itself never appears in the error, which is
// the whole point of naming the two ways to supply it instead.
func (p *Provider) projectAPIKey() (string, error) {
	if p.apiKey != "" {
		return p.apiKey, nil
	}
	if k := os.Getenv("POSTHOG_PROJECT_API_KEY"); k != "" {
		return k, nil
	}
	return "", fmt.Errorf("mamori/posthog: no project API key; set POSTHOG_PROJECT_API_KEY or use posthog.WithProjectAPIKey: %w", mamori.ErrInvalid)
}

// subject returns the distinct id flags are evaluated for.
func (p *Provider) subject() string {
	if p.distinctID != "" {
		return p.distinctID
	}
	if id := os.Getenv("POSTHOG_DISTINCT_ID"); id != "" {
		return id
	}
	return DefaultDistinctID
}

// Resolve evaluates the flag named by ref.Path for the provider's distinct id
// and returns the facet named by ref.Key. A flag PostHog did not return
// resolves to an error satisfying errors.Is(err, mamori.ErrNotFound).
func (p *Provider) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
	if err := ctx.Err(); err != nil {
		return mamori.Value{}, err
	}
	key := ref.Path
	if key == "" {
		return mamori.Value{}, fmt.Errorf("mamori/posthog: ref %q must be of the form posthog://<flag-key>[#enabled|#variant|#payload]: %w", ref.Raw, mamori.ErrInvalid)
	}
	// Validate the fragment before spending a round trip on a ref that cannot
	// be served whatever the backend answers.
	switch ref.Key {
	case "", fragEnabled, fragVariant, fragPayload:
	default:
		return mamori.Value{}, fmt.Errorf("mamori/posthog: ref %q has unsupported fragment %q (use #enabled, #variant or #payload): %w", ref.Raw, ref.Key, mamori.ErrInvalid)
	}

	env, err := p.evaluate(ctx)
	if err != nil {
		return mamori.Value{}, err
	}

	// Quota exhaustion is answered with 200 and an empty flags object, so it
	// must be checked before absence is read as not-found: reporting not-found
	// here would have mamori quietly apply a default in place of a live flag.
	for _, q := range env.QuotaLimited {
		if q == quotaLimitedFlags {
			return mamori.Value{}, fmt.Errorf("mamori/posthog: flag evaluation is paused for this project (quotaLimited names %s): %w", quotaLimitedFlags, mamori.ErrRateLimited)
		}
	}

	flag, ok := env.Flags[key]
	if !ok {
		// An absent flag means "does not exist" only when PostHog computed the
		// whole set successfully. Otherwise the flag may exist perfectly well
		// and simply not have been computed, and calling that not-found would
		// substitute a default for a value the backend never denied.
		if env.ErrorsWhileComputingFlags {
			return mamori.Value{}, fmt.Errorf("mamori/posthog: flag %q was not computed (errorsWhileComputingFlags): %w", key, mamori.ErrUnavailable)
		}
		return mamori.Value{}, fmt.Errorf("mamori/posthog: flag %q not found: %w", key, mamori.ErrNotFound)
	}

	b, kind, err := facet(flag, ref.Key)
	if err != nil {
		return mamori.Value{}, fmt.Errorf("mamori/posthog: flag %q: %w", key, err)
	}

	return mamori.Value{
		Bytes: b,
		// Hash the bytes actually returned, not the whole flag object: the
		// object carries an evaluation reason and condition index that can
		// change without the value changing, and a Version that moves on those
		// would cost a spurious PreApply and OnChange on every poll.
		Version:   mamori.VersionHash(b),
		Sensitive: false,
		Metadata: map[string]string{
			"flag": key,
			"kind": kind,
		},
	}, nil
}

// evaluate performs one POST /flags?v=2 and decodes the envelope.
func (p *Provider) evaluate(ctx context.Context) (*flagsEnvelope, error) {
	apiKey, err := p.projectAPIKey()
	if err != nil {
		return nil, err
	}
	cli, err := p.client()
	if err != nil {
		return nil, err
	}

	body, err := json.Marshal(flagsRequest{
		APIKey:           apiKey,
		DistinctID:       p.subject(),
		Groups:           p.groups,
		PersonProperties: p.personProperties,
		GroupProperties:  p.groupProperties,
	})
	if err != nil {
		// Deliberately not wrapped with the marshalling error: its message can
		// quote the value it failed on, and this struct holds the project API
		// key. The field set is small and fixed, so naming it is enough.
		return nil, fmt.Errorf("mamori/posthog: encoding the flag evaluation request: %w", mamori.ErrInvalid)
	}

	resp, err := cli.Do(ctx, httpcore.Request{
		Method: http.MethodPost,
		Path:   flagsPath,
		Query:  map[string][]string{"v": {flagsVersion}},
		Header: http.Header{"Content-Type": []string{"application/json"}},
		Body:   body,
	})
	if err != nil {
		// One %w: httpcore.Do already classified this, and adding a second
		// sentinel here would replace its kind rather than add to it.
		return nil, fmt.Errorf("mamori/posthog: evaluating flags: %w", err)
	}

	var env flagsEnvelope
	if err := json.Unmarshal(resp.Body, &env); err != nil {
		// The decode error can quote the malformed body, which is a set of flag
		// payloads, so it is named rather than wrapped.
		return nil, fmt.Errorf("mamori/posthog: the flag evaluation response was not valid JSON: %w", mamori.ErrUnavailable)
	}
	return &env, nil
}

// facet renders the requested facet of an evaluated flag, returning the bytes
// and a short name for Value.Metadata["kind"].
//
// The empty fragment reproduces what PostHog's own SDKs return from
// getFeatureFlag: the variant key for a multivariate flag, and "true"/"false"
// for a boolean one. A flag is multivariate exactly when PostHog sent a
// variant, so a disabled flag of either shape renders as "false" rather than as
// an empty string.
func facet(f flagResult, fragment string) ([]byte, string, error) {
	switch fragment {
	case "":
		if f.Variant != "" {
			return []byte(f.Variant), "variant", nil
		}
		return strconv.AppendBool(nil, f.Enabled), "enabled", nil
	case fragEnabled:
		return strconv.AppendBool(nil, f.Enabled), "enabled", nil
	case fragVariant:
		return []byte(f.Variant), "variant", nil
	case fragPayload:
		return payloadBytes(f.Metadata.Payload), "payload", nil
	default:
		// Unreachable: Resolve validates the fragment before the request. Kept
		// so this function is total rather than relying on its only caller.
		return nil, "", fmt.Errorf("unsupported fragment %q: %w", fragment, mamori.ErrInvalid)
	}
}

// payloadBytes renders a flag payload.
//
// PostHog documents metadata.payload as a JSON-encoded STRING - the example is
// "{\"example\": \"json\"}" - so the useful bytes are the string's contents,
// not its quoted form; returning the quoted form would give a caller a
// double-encoded document that no JSON decode would accept. A payload that is
// not a JSON string is passed through as its raw JSON instead of being rejected,
// because a self-hosted or future PostHog that inlines the object is answering
// something usable and mangling it would be worse than accepting it.
func payloadBytes(raw json.RawMessage) []byte {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []byte(s)
	}
	return raw
}

// cloneStringMap copies a map so a caller mutating theirs after New cannot
// change what this provider sends.
func cloneStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// cloneAnyMap copies a property map for the same reason as cloneStringMap.
func cloneAnyMap(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
