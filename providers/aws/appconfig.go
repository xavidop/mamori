package aws

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/appconfigdata"
	"github.com/xavidop/mamori"
)

// schemeAppConfig is the URL scheme handled by AppConfigProvider.
const schemeAppConfig = "aws-appconfig"

// appConfigAPI is the minimal subset of the AppConfig Data client this
// provider uses. The real *appconfigdata.Client satisfies it; tests inject an
// in-memory fake that models the session protocol.
type appConfigAPI interface {
	StartConfigurationSession(ctx context.Context, params *appconfigdata.StartConfigurationSessionInput, optFns ...func(*appconfigdata.Options)) (*appconfigdata.StartConfigurationSessionOutput, error)
	GetLatestConfiguration(ctx context.Context, params *appconfigdata.GetLatestConfigurationInput, optFns ...func(*appconfigdata.Options)) (*appconfigdata.GetLatestConfigurationOutput, error)
}

// AppConfigProvider resolves
// aws-appconfig://<application>/<environment>/<profile>[#key] refs against AWS
// AppConfig. It is safe for concurrent use.
//
// AppConfig is a session protocol, not a request/response API, and one of its
// properties shapes this whole type: GetLatestConfiguration returns an empty
// payload when the calling session already holds the current version. A
// provider that opened one session and reused it would therefore return the
// configuration on its first Resolve and empty bytes on every Resolve after,
// and mamori would apply those empty bytes over a live configuration field.
// The failure would be silent and would look like a config wipe.
//
// Resolve therefore starts a session, makes exactly one call, and discards the
// session. A session created moments ago holds no version at all, so it cannot
// be considered current, so the empty-payload case is structurally unreachable
// on this path. The cost is two API calls per resolve, which is the price of a
// stateless Resolve and is worth paying.
type AppConfigProvider struct {
	opts   options
	mu     sync.Mutex
	client appConfigAPI
}

// Compile-time interface check.
var _ mamori.Provider = (*AppConfigProvider)(nil)

// NewAppConfig constructs an AWS AppConfig provider. The underlying client is
// built lazily on first Resolve using the default credential chain, so
// construction never performs I/O and never fails.
func NewAppConfig(opts ...Option) *AppConfigProvider {
	return &AppConfigProvider{opts: newOptions(opts)}
}

// newAppConfigWithClient returns a provider backed by a caller-supplied
// client. It is the injection seam used by tests.
func newAppConfigWithClient(c appConfigAPI) *AppConfigProvider {
	return &AppConfigProvider{client: c}
}

// Scheme returns "aws-appconfig".
func (p *AppConfigProvider) Scheme() string { return schemeAppConfig }

// getClient returns the cached client, building the real one on first use.
func (p *AppConfigProvider) getClient(ctx context.Context) (appConfigAPI, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.client != nil {
		return p.client, nil
	}
	cfg, err := loadConfig(ctx, p.opts)
	if err != nil {
		return nil, fmt.Errorf("aws-appconfig: load config: %w", err)
	}
	p.client = appconfigdata.NewFromConfig(cfg)
	return p.client, nil
}

// parseAppConfigPath splits a ref path into its three required identifiers.
// Each may be an ID or a name; the provider passes them through verbatim and
// lets the service resolve them.
func parseAppConfigPath(ref mamori.Ref) (app, env, profile string, err error) {
	parts := strings.Split(ref.Path, "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", "", "", fmt.Errorf(
			"aws-appconfig: ref %q must be aws-appconfig://<application>/<environment>/<profile>[#key]: %w",
			ref.Raw, mamori.ErrInvalid)
	}
	return parts[0], parts[1], parts[2], nil
}

// startSession opens a configuration session and returns its initial token.
func (p *AppConfigProvider) startSession(ctx context.Context, client appConfigAPI, ref mamori.Ref) (string, error) {
	app, env, profile, err := parseAppConfigPath(ref)
	if err != nil {
		return "", err
	}
	in := &appconfigdata.StartConfigurationSessionInput{
		ApplicationIdentifier:          awssdk.String(app),
		EnvironmentIdentifier:          awssdk.String(env),
		ConfigurationProfileIdentifier: awssdk.String(profile),
	}
	if secs, ok := appConfigMinPoll(ref); ok {
		in.RequiredMinimumPollIntervalInSeconds = awssdk.Int32(secs)
	}
	out, err := client.StartConfigurationSession(ctx, in)
	if err != nil {
		return "", fmt.Errorf("aws-appconfig: start session for %q: %w", ref.Path, classifyAWS(err))
	}
	return awssdk.ToString(out.InitialConfigurationToken), nil
}

// Resolve fetches the current configuration. It starts a session, makes one
// GetLatestConfiguration call, and discards the session; see the type comment
// for why a session is never reused across calls.
func (p *AppConfigProvider) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
	client, err := p.getClient(ctx)
	if err != nil {
		return mamori.Value{}, err
	}
	token, err := p.startSession(ctx, client, ref)
	if err != nil {
		return mamori.Value{}, err
	}
	out, err := client.GetLatestConfiguration(ctx, &appconfigdata.GetLatestConfigurationInput{
		ConfigurationToken: awssdk.String(token),
	})
	if err != nil {
		return mamori.Value{}, fmt.Errorf("aws-appconfig: resolve %q: %w", ref.Path, classifyAWS(err))
	}

	// An empty payload means "the calling session already holds the current
	// version". This session was created one call ago and holds nothing, so
	// reaching here contradicts the documented protocol. Fail rather than
	// return a zero-length value: mamori would apply that value over a live
	// configuration field, and a silent wipe is far worse than a loud error.
	if len(out.Configuration) == 0 {
		return mamori.Value{}, fmt.Errorf(
			"aws-appconfig: %q returned an empty configuration on a newly created session: %w",
			ref.Path, mamori.ErrUnavailable)
	}

	return appConfigValue(ref.Key, out.Configuration, awssdk.ToString(out.VersionLabel))
}

// appConfigValue assembles a mamori.Value from a configuration payload,
// applying #key selection when key is non-empty.
func appConfigValue(key string, data []byte, versionLabel string) (mamori.Value, error) {
	if key != "" {
		sel, err := mamori.SelectKey(data, key)
		if err != nil {
			return mamori.Value{}, err
		}
		data = sel
	}
	// VersionLabel only applies to AppConfig-hosted configuration versions and
	// is empty for every other source, so the hash fallback is the common path
	// rather than a defensive one.
	version := versionLabel
	if version == "" {
		version = mamori.VersionHash(data)
	}
	return mamori.Value{
		Bytes:     data,
		Version:   version,
		Sensitive: false, // a configuration service, not a secret store
	}, nil
}

// appConfigMinPoll reads the optional ?minPoll=<seconds> query option, which
// sets RequiredMinimumPollIntervalInSeconds on the session. It is meaningful
// only on the Watch path, where it raises the floor the service enforces on
// how often this client may poll; Resolve accepts it and it has no effect,
// since that session is discarded before a second call could be rate-limited.
//
// An unparseable or non-positive value is ignored rather than rejected. The
// service supplies its own default, and refusing to resolve a configuration
// field over a malformed tuning hint would be a worse outcome than ignoring
// the hint.
func appConfigMinPoll(ref mamori.Ref) (int32, bool) {
	raw := ref.Opt("minPoll")
	if raw == "" {
		return 0, false
	}
	secs, err := strconv.Atoi(raw)
	if err != nil || secs <= 0 {
		return 0, false
	}
	return int32(secs), true
}
