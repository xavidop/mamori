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
	"time"

	spf "github.com/spf13/viper"
	"github.com/xavidop/mamori"
)

// Scheme is the URL scheme handled by this provider.
const Scheme = "viper"

func init() { mamori.Register(New()) }

// Provider resolves viper:// refs against a *viper.Viper.
//
// Concurrency: Viper v1.21.0 itself is NOT safe for concurrent read and
// write. Its internal config/override/defaults maps carry no mutex of their
// own, so mamori's background polling goroutine calling Resolve (which reads
// through IsSet and Get) races with any concurrent Set, SetDefault, or a
// reload triggered by the application's own viper.WatchConfig() - confirmed
// under go test -race with the write in Viper's Set and the read in
// Resolve's IsSet. This provider adds no locking of its own to paper over
// that, deliberately: the writes happen in application code this package
// never sees, so nothing here could serialize them correctly.
//
// Do not call viper.WatchConfig() (or otherwise mutate the instance from
// another goroutine) on a *viper.Viper that mamori is polling through this
// provider. Let mamori's own poller detect changes instead. If file-level
// change detection is what you actually want, mamori's built-in file://
// provider already watches a file natively via fsnotify, with no such race.
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
// plain text form for the same reason. time.Duration and time.Time get their
// own cases below for the same reason, one type switch step later, since
// json.Marshal would corrupt both in ways this package has already seen in
// real config files. Everything else becomes JSON, which is also what a
// #json-key fragment selects against.
func render(v any) ([]byte, error) {
	switch x := v.(type) {
	case nil:
		return nil, fmt.Errorf("value is nil: %w", mamori.ErrNotFound)
	case string:
		return []byte(x), nil
	case bool:
		return []byte(strconv.FormatBool(x)), nil
	case time.Duration:
		// v.SetDefault("timeout", 30*time.Second) is canonical Viper wiring.
		// time.Duration's underlying type is int64, but a type switch matches
		// the named type exactly, so it does not fall into the int64 case
		// below and must be handled here first. Rendered as its String() form
		// ("30s"), matching what a YAML/TOML file already gives as a plain
		// string for the same setting, so both paths decode with
		// time.ParseDuration. Falling through to the int64 case (or to
		// json.Marshal) would instead render a bare nanosecond count
		// ("30000000000"), which ParseDuration rejects for missing a unit.
		return []byte(x.String()), nil
	case time.Time:
		// gopkg.in/yaml.v3 decodes a bare YAML timestamp (e.g.
		// "expires: 2026-07-29T00:00:00Z") into a time.Time when unmarshaling
		// into `any`, so an ordinary YAML config takes this path, not the
		// string case above. Falling through to json.Marshal here would wrap
		// it in quotes - the exact defect the string case exists to prevent,
		// just arriving through a different Go type. RFC 3339 without quotes
		// keeps it plain text a string field or time.Parse can consume as-is.
		return []byte(x.Format(time.RFC3339)), nil
	// int8/int16/uint8/uint16/uint32 have no case of their own: Viper never
	// produces them (Get's own type coercion only ever yields the widths
	// listed below, plus the two below that Viper doesn't produce but Go
	// programs sometimes pass through Set), and any that do turn up fall
	// through to json.Marshal, which renders them as the same plain decimal
	// digits strconv would. Nothing to fix; this is deliberate, not a gap.
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
		// 'f' rather than 'g': Viper's JSON decoding stores every number,
		// including whole numbers like a byte size or timeout, as float64.
		// 'g' switches to exponent notation once the exponent reaches 6, so
		// an entirely ordinary value like 10485760 would render as
		// "1.048576e+07", which mamori's own int decode path
		// (strconv.ParseInt) rejects outright. 'f' always renders plain
		// decimal digits, matching what a YAML/TOML source (which preserves
		// int) already gives for the same value.
		return []byte(strconv.FormatFloat(float64(x), 'f', -1, 32)), nil
	case float64:
		return []byte(strconv.FormatFloat(x, 'f', -1, 64)), nil
	case []byte:
		// Copied rather than returned directly, unlike a bare slice
		// reference: every other branch above already produces bytes owned
		// solely by this call, and a caller that mutates the returned
		// Value.Bytes must not be able to reach back into Viper's own stored
		// value through this one.
		cp := make([]byte, len(x))
		copy(cp, x)
		return cp, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("value of type %T is not JSON-encodable: %w: %w", v, mamori.ErrInvalid, err)
	}
	return b, nil
}
