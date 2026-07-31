// Package scalewaysm implements a mamori provider for Scaleway Secret Manager
// (https://www.scaleway.com/en/secret-manager/), Scaleway's regional secret
// store for API keys, database credentials, and other sensitive values.
//
// Scaleway publishes a Go SDK (scaleway-sdk-go), but it is not used here: the
// read path is a single authenticated GET against a documented HTTPS API, so
// this provider uses net/http and the standard library only, keeping the
// SDK's dependency tree, and its transitive requirements, out of every
// consumer's build.
//
// # Scheme
//
//	scaleway-sm://<name>                  secret at the root path, latest enabled revision
//	scaleway-sm://<path>/<name>            secret at an explicit path
//	scaleway-sm://<name>?revision=<n>      an explicit revision number
//	scaleway-sm://<name>?revision=latest   the newest revision, even if disabled
//
// The LAST path segment is always the secret name; everything before it is
// the path, a real slash-delimited directory Scaleway secrets are organized
// under (a secret's full location is effectively path plus name). This
// differs deliberately from providers/cloudflare-kv, where the ENTIRE ref
// path is one key: Workers KV keys may themselves legally contain slashes,
// so splitting there would silently misread a key like
// "config/prod/log-level" as a namespace plus a shorter key. Scaleway's path
// segments are real directories rather than characters inside the secret's
// own name, so splitting on the ref's last slash here is correct, not a bug
// to reconcile against that sibling.
//
// The ?revision option defaults to "latest_enabled" rather than "latest".
// Disabling a revision is how a Scaleway operator revokes a leaked
// credential without deleting its history, and "latest" ignores that state
// entirely: it returns the newest revision even when it has just been
// disabled. Defaulting to "latest" would mean mamori keeps serving a secret
// an operator explicitly revoked, which is the opposite of what disabling a
// revision is for. A caller who genuinely wants the newest revision
// regardless of its enabled state can still ask for it with
// ?revision=latest.
//
// # Authentication
//
// Reading a secret requires a Scaleway API secret key, a project id, and a
// region. Each may be set explicitly (WithSecretKey, WithProjectID,
// WithRegion) or read from the environment (SCW_SECRET_KEY,
// SCW_DEFAULT_PROJECT_ID, SCW_DEFAULT_REGION); an explicit option wins over
// its environment variable. The region additionally falls back to "fr-par"
// when neither is set, since Secret Manager is a regional product with no
// account-wide default.
//
// # Watching
//
// The Secret Manager REST API exposes no streaming or blocking read, so this
// provider deliberately does not implement mamori.WatchableProvider and
// mamori wraps it in the polling adapter instead.
//
// # Batching
//
// Secret Manager's access-secret-version endpoint returns one revision of
// one secret; there is no bulk endpoint that returns many secrets' payloads
// in a single call. So this provider deliberately does not implement
// mamori.BatchProvider, and each Resolve costs its own request.
package scalewaysm

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/xavidop/mamori"
)

const (
	scheme = "scaleway-sm"

	// defaultRegion is used when neither WithRegion nor SCW_DEFAULT_REGION is
	// set. Secret Manager is regional, so a region is always required to
	// build a request; fr-par is Scaleway's default region across its API
	// and CLI.
	defaultRegion = "fr-par"

	// defaultRevision is the ?revision value used when a ref names none. See
	// the package doc comment for why this is "latest_enabled" and not
	// "latest".
	defaultRevision = "latest_enabled"

	defaultBaseURL = "https://api.scaleway.com/secret-manager/v1beta1"
)

// Provider resolves scaleway-sm:// refs against the Secret Manager REST API.
// It is safe for concurrent use.
type Provider struct {
	secretKey string
	projectID string
	region    string
	baseURL   string

	httpClient *http.Client
}

// settings is the resolved, per-provider configuration needed to authenticate
// against Secret Manager: the API secret key, the project id, and the
// region.
type settings struct {
	secretKey string
	projectID string
	region    string
}

// Option configures a Provider.
type Option func(*Provider)

// WithSecretKey sets the Scaleway API secret key used to authenticate
// requests.
func WithSecretKey(key string) Option { return func(p *Provider) { p.secretKey = key } }

// WithProjectID sets the Scaleway project id that owns the secrets.
func WithProjectID(id string) Option { return func(p *Provider) { p.projectID = id } }

// WithRegion sets the Scaleway region Secret Manager requests are sent to.
func WithRegion(region string) Option { return func(p *Provider) { p.region = region } }

// WithBaseURL overrides the API origin, for an httptest.Server or a proxy. A
// trailing slash is trimmed so that joining it with a path never produces a
// double slash. An empty string is a no-op, leaving New's default in place.
func WithBaseURL(u string) Option {
	return func(p *Provider) {
		if u != "" {
			p.baseURL = strings.TrimRight(u, "/")
		}
	}
}

// WithHTTPClient injects a custom *http.Client. A nil client is a no-op.
func WithHTTPClient(c *http.Client) Option {
	return func(p *Provider) {
		if c != nil {
			p.httpClient = c
		}
	}
}

// New constructs a Secret Manager provider. Without options it reads
// SCW_SECRET_KEY, SCW_DEFAULT_PROJECT_ID, and SCW_DEFAULT_REGION lazily at
// resolve time, so it is safe to register from init even when no
// credentials exist at process start.
func New(opts ...Option) *Provider {
	p := &Provider{
		baseURL:    defaultBaseURL,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

// Registration is deferred to Task 2: mamori.Register takes a
// mamori.Provider, an interface requiring Resolve, which this provider does
// not implement yet.
//
// func init() { mamori.Register(New()) }

// Scheme returns "scaleway-sm".
func (p *Provider) Scheme() string { return scheme }

// settingsFor resolves the credentials and region for this provider, reading
// the environment lazily so registering from init is safe with no
// credentials at process start. Precedence is the explicit option, then the
// environment; the region additionally falls back to defaultRegion when
// neither is set.
func (p *Provider) settingsFor() (settings, error) {
	s := settings{
		secretKey: firstNonEmpty(p.secretKey, os.Getenv("SCW_SECRET_KEY")),
		projectID: firstNonEmpty(p.projectID, os.Getenv("SCW_DEFAULT_PROJECT_ID")),
		region:    firstNonEmpty(p.region, os.Getenv("SCW_DEFAULT_REGION"), defaultRegion),
	}
	// Each message names both the option and the environment variable that
	// would supply the value, and never echoes any value that IS set: the
	// secret key and the project id must never reach an error message, so
	// neither is interpolated into these strings, even the one it names.
	switch {
	case s.secretKey == "":
		return settings{}, errors.New("mamori/scaleway-sm: no API secret key; set SCW_SECRET_KEY or use WithSecretKey")
	case s.projectID == "":
		return settings{}, errors.New("mamori/scaleway-sm: no project id; set SCW_DEFAULT_PROJECT_ID or use WithProjectID")
	}
	return s, nil
}

// parseRef splits ref into the secret's path, its name, and the revision
// selector to request.
//
// The LAST path segment is the secret name; everything before it is the
// path, returned slash-prefixed (e.g. "/prod", or "/" for the root). This
// diverges deliberately from providers/cloudflare-kv, where the ENTIRE ref
// path is one key: Workers KV keys may themselves contain slashes, so
// splitting there would silently misread a key like
// "config/prod/log-level" as a namespace plus a shorter key. Scaleway
// organizes secrets under a real, slash-delimited directory structure, so
// splitting on the ref's last slash here is correct, not a bug to reconcile
// against that sibling.
//
// Only leading slashes are trimmed before splitting. A trailing slash is
// preserved so that "prod/" splits into a path segment and an empty name
// segment, reported as a missing secret name rather than silently treating
// "prod" as the name.
//
// revision defaults to defaultRevision ("latest_enabled") when the ref
// carries no ?revision option; see the package doc comment for why that
// default is not "latest".
func parseRef(ref mamori.Ref) (path, name, revision string, err error) {
	trimmed := strings.TrimLeft(ref.Path, "/")
	if trimmed == "" {
		return "", "", "", fmt.Errorf("mamori/scaleway-sm: ref %q requires a secret name", ref.Raw)
	}

	dir := ""
	if i := strings.LastIndexByte(trimmed, '/'); i >= 0 {
		dir, name = trimmed[:i], trimmed[i+1:]
	} else {
		name = trimmed
	}
	if name == "" {
		return "", "", "", fmt.Errorf("mamori/scaleway-sm: ref %q requires a secret name", ref.Raw)
	}

	revision = ref.Opt("revision")
	if revision == "" {
		revision = defaultRevision
	}
	return "/" + dir, name, revision, nil
}

// firstNonEmpty returns the first non-empty string among vals, or "" if all
// are empty.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
