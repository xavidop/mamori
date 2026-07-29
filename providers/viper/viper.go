// Package viper implements a mamori Provider backed by Viper
// (https://github.com/spf13/viper), the configuration library.
//
// It resolves refs of the form:
//
//	viper://<key>[#json-key]
//
// where <key> is a Viper key in its usual dotted form. The resolved Value is
// whatever Viper returns for that key, rendered as text:
//
//	Port     int    `source:"viper://server.port"`
//	LogLevel string `source:"viper://logging.level" default:"info"`
//
// The point of this provider is incremental adoption. Viper resolves a key by
// consulting explicit Set calls, then flags, then the environment, then the
// config file, then key/value stores, then defaults, and returns the winner. A
// viper:// ref returns that winner, so an application with an existing Viper
// setup can move fields into a typed, validated mamori struct one at a time
// without reimplementing Viper's precedence in struct tags, and can put secret
// material in a real secret store immediately.
//
// Values are never marked Sensitive: Viper holds application configuration.
// Move secret material to a secret manager rather than relying on this
// provider to treat Viper's contents as secret.
//
// Viper's read API has no error return (Get returns any, IsSet returns bool),
// so a missing key is the only failure this provider can report. There is
// nothing else to classify. A failure to load a config file happens earlier,
// inside the application's own ReadInConfig call.
//
// mamori polls this provider. A read here is an in-memory map lookup, so there
// is no cost for a native watch to avoid.
package viper

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	spf "github.com/spf13/viper"
	"github.com/xavidop/mamori"
)

// Scheme is the URL scheme handled by this provider.
const Scheme = "viper"

func init() { mamori.Register(New()) }

// Provider resolves viper:// refs against a *viper.Viper.
//
// It is safe for concurrent use to the same degree the underlying Viper
// instance is: the provider adds no state of its own beyond the instance
// pointer, which is set at construction and never mutated.
type Provider struct {
	v *spf.Viper
}

// Compile-time interface check.
var _ mamori.Provider = (*Provider)(nil)

// Option configures a Provider.
type Option func(*Provider)

// WithViper resolves against an explicit Viper instance instead of the global
// one. Use it when the application keeps its own instance, and in tests.
func WithViper(v *spf.Viper) Option {
	return func(p *Provider) { p.v = v }
}

// New constructs a Viper provider. With no options it resolves against Viper's
// global instance, the one the package-level SetConfigFile, AutomaticEnv, and
// BindPFlag calls populate, which is what most Viper codebases use. An
// application that already calls viper.ReadInConfig gets working viper:// refs
// with no wiring.
//
// Construction performs no I/O and never fails, so registering from init is
// safe even before Viper has read anything.
func New(opts ...Option) *Provider {
	p := &Provider{}
	for _, opt := range opts {
		opt(p)
	}
	if p.v == nil {
		p.v = spf.GetViper()
	}
	return p
}

// Scheme returns "viper".
func (p *Provider) Scheme() string { return Scheme }

// Resolve reads the ref's key from Viper and renders the result as text.
//
// The key is passed to Viper verbatim and never split, so an instance
// configured with a non-default key delimiter works unchanged. Viper's own
// reads never block or fail, so the context is only checked for
// cancellation, not threaded any further.
func (p *Provider) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
	if err := ctx.Err(); err != nil {
		return mamori.Value{}, err
	}
	key := ref.Path
	if key == "" {
		return mamori.Value{}, fmt.Errorf(
			"viper: ref %q must be viper://<key>[#json-key]: %w", ref.Raw, mamori.ErrInvalid)
	}

	// IsSet reports true for a key whose only source is SetDefault. That is
	// inherited deliberately: a Viper default is a real configured value, and
	// treating it as missing would substitute mamori's default for Viper's,
	// changing the value under the guise of a lookup.
	if !p.v.IsSet(key) {
		return mamori.Value{}, fmt.Errorf("viper: key %q is not set: %w", key, mamori.ErrNotFound)
	}

	data, err := render(p.v.Get(key))
	if err != nil {
		return mamori.Value{}, fmt.Errorf("viper: key %q: %w", key, err)
	}

	if ref.Key != "" {
		sel, serr := mamori.SelectKey(data, ref.Key)
		if serr != nil {
			return mamori.Value{}, serr
		}
		data = sel
	}

	return mamori.Value{
		Bytes:   data,
		Version: mamori.VersionHash(data), // Viper has no revision concept
		// Viper holds application configuration, not secrets.
		Sensitive: false,
	}, nil
}

// render turns a Viper value into the text form core decodes into a field.
//
// A string passes through unchanged rather than being JSON-encoded, so
// `viper://logging.level` yields info and not "info"; the quotes would survive
// into a string field and into any comparison against it. Scalars get their
// plain text form for the same reason. Everything else becomes JSON, which is
// also what a #json-key fragment selects against.
func render(v any) ([]byte, error) {
	switch x := v.(type) {
	case nil:
		return nil, fmt.Errorf("value is nil: %w", mamori.ErrNotFound)
	case string:
		return []byte(x), nil
	case bool:
		return []byte(strconv.FormatBool(x)), nil
	case int:
		return []byte(strconv.Itoa(x)), nil
	case int32:
		return []byte(strconv.FormatInt(int64(x), 10)), nil
	case int64:
		return []byte(strconv.FormatInt(x, 10)), nil
	case uint:
		return []byte(strconv.FormatUint(uint64(x), 10)), nil
	case uint64:
		return []byte(strconv.FormatUint(x, 10)), nil
	case float32:
		return []byte(strconv.FormatFloat(float64(x), 'g', -1, 32)), nil
	case float64:
		return []byte(strconv.FormatFloat(x, 'g', -1, 64)), nil
	case []byte:
		return x, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("value of type %T is not JSON-encodable: %w: %w", v, mamori.ErrInvalid, err)
	}
	return b, nil
}
