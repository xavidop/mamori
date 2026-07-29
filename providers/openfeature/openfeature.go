// Package openfeature implements a mamori Provider backed by OpenFeature
// (https://openfeature.dev), the vendor-neutral feature-flag standard.
//
// It resolves refs of the form:
//
//	openfeature://<flag-key>[#json-key][?type=bool|string|int|float|object]
//
// where <flag-key> is a flag in whichever OpenFeature provider the application
// has installed with openfeature.SetProvider. The resolved Value is the
// evaluated flag rendered as text:
//
//	NewCheckout bool   `source:"openfeature://new-checkout?type=bool"`
//	Limits      string `source:"openfeature://limits?type=object"`
//	MaxUpload   int    `source:"openfeature://limits#/upload/maxMB?type=object"`
//
// Evaluation identity is fixed for the process. Every ref is evaluated against
// one evaluation context, whose targeting key defaults to "mamori" and is
// overridable with WithTargetingKey, plus any static attributes given to
// WithAttributes. This provider therefore does NOT do per-user targeting: a
// mamori field holds one value for the whole process, not one value per
// request. Targeting that varies per end user belongs in the application's own
// OpenFeature calls, not in a config field.
//
// Values are never marked Sensitive: a feature flag is configuration, not a
// secret. OpenFeature has no native change notification exposed here, so this
// provider does not implement WatchableProvider and mamori polls it.
package openfeature

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	of "github.com/open-feature/go-sdk/openfeature"
	"github.com/xavidop/mamori"
)

// Scheme is the URL scheme handled by this provider.
const Scheme = "openfeature"

// defaultTargetingKey is the evaluation context's targeting key when none is
// given. It matches the "mamori" default the launchdarkly provider uses for
// the same purpose.
const defaultTargetingKey = "mamori"

// clientName is the name given to the SDK client this provider creates when no
// client is injected.
const clientName = "mamori"

func init() { mamori.Register(New()) }

// flagType is which OpenFeature evaluation method a ref selects.
type flagType int

const (
	// typeAuto tries object, then bool, then string. See evaluateAuto.
	typeAuto flagType = iota
	typeBool
	typeString
	typeInt
	typeFloat
	typeObject
)

func (t flagType) String() string {
	switch t {
	case typeBool:
		return "bool"
	case typeString:
		return "string"
	case typeInt:
		return "int"
	case typeFloat:
		return "float"
	case typeObject:
		return "object"
	}
	return "auto"
}

// Provider resolves openfeature:// refs by evaluating flags through an
// OpenFeature client. It is safe for concurrent use. When no client is
// injected with WithClient, the default openfeature.NewClient(...) client is
// built lazily on first Resolve rather than in New, matching every other
// mamori provider's "New never touches a live resource" convention: the
// OpenFeature SDK's global API singleton starts a background event-listener
// goroutine the first time any client touches it, and New is called
// unconditionally from this package's init, so building it eagerly there
// would start that goroutine merely by importing the package.
type Provider struct {
	mu      sync.Mutex
	client  of.IClient // nil until conn() builds it, unless WithClient injected one
	evalCtx of.EvaluationContext
}

// Compile-time interface check.
var _ mamori.Provider = (*Provider)(nil)

// Option configures a Provider.
type Option func(*providerConfig)

type providerConfig struct {
	client       of.IClient
	targetingKey string
	attributes   map[string]any
}

// WithClient injects the OpenFeature client to evaluate through, instead of
// the default one this provider creates. Use it to supply an application's own
// named client, or an in-memory fake in tests.
func WithClient(c of.IClient) Option {
	return func(cfg *providerConfig) { cfg.client = c }
}

// WithTargetingKey sets the evaluation context's targeting key, which
// identifies the subject every flag is evaluated for. It defaults to "mamori".
//
// This is a per-process identity, not a per-request one. See the package doc.
func WithTargetingKey(k string) Option {
	return func(cfg *providerConfig) { cfg.targetingKey = k }
}

// WithAttributes adds static evaluation-context attributes, so a deployment can
// be targeted by region, tier, or any other dimension that is constant for the
// process.
func WithAttributes(m map[string]any) Option {
	return func(cfg *providerConfig) { cfg.attributes = m }
}

// New constructs an OpenFeature provider. With no options it evaluates through
// openfeature.NewClient("mamori"), which resolves against whatever provider the
// application installed with openfeature.SetProvider. That client is built
// lazily on first Resolve, not here; see the Provider doc comment for why.
//
// Construction performs no I/O and never fails, so registering it from init is
// safe even before an OpenFeature provider has been set. An evaluation made
// before one is ready reports PROVIDER_NOT_READY, which maps to
// mamori.ErrUnavailable and is retried like any other transient failure.
func New(opts ...Option) *Provider {
	cfg := providerConfig{targetingKey: defaultTargetingKey}
	for _, opt := range opts {
		opt(&cfg)
	}
	return &Provider{
		client:  cfg.client,
		evalCtx: of.NewEvaluationContext(cfg.targetingKey, cfg.attributes),
	}
}

// Scheme returns "openfeature".
func (p *Provider) Scheme() string { return Scheme }

// conn returns the OpenFeature client to evaluate through, building the
// default named client lazily on first use if none was injected with
// WithClient. Concurrent callers share one client.
func (p *Provider) conn() of.IClient {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.client == nil {
		p.client = of.NewClient(clientName)
	}
	return p.client
}

// parseFlagType reads the optional ?type= option. An unrecognized or empty
// value is rejected rather than silently falling back to the auto chain: a
// typo like ?type=boolean would otherwise resolve through a different code
// path than the author intended and only show up as a wrong value.
// Ref.Opts is a url.Values, so an absent option and one present but empty are
// distinguishable: ?type= with no value is a mistake worth reporting, not a
// request for the auto chain.
func parseFlagType(ref mamori.Ref) (flagType, error) {
	if _, ok := ref.Opts["type"]; !ok {
		return typeAuto, nil
	}
	switch strings.TrimSpace(ref.Opt("type")) {
	case "bool":
		return typeBool, nil
	case "string":
		return typeString, nil
	case "int":
		return typeInt, nil
	case "float":
		return typeFloat, nil
	case "object":
		return typeObject, nil
	}
	return typeAuto, fmt.Errorf(
		"openfeature: ref %q has unsupported ?type=%q; want bool, string, int, float, or object: %w",
		ref.Raw, ref.Opt("type"), mamori.ErrInvalid)
}

// Resolve evaluates the ref's flag and renders the result as text.
func (p *Provider) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
	if err := ctx.Err(); err != nil {
		return mamori.Value{}, err
	}
	if ref.Path == "" {
		return mamori.Value{}, fmt.Errorf(
			"openfeature: ref %q must be openfeature://<flag-key>[#json-key][?type=t]: %w",
			ref.Raw, mamori.ErrInvalid)
	}
	typ, err := parseFlagType(ref)
	if err != nil {
		return mamori.Value{}, err
	}

	data, variant, err := p.evaluate(ctx, ref, typ)
	if err != nil {
		return mamori.Value{}, err
	}

	if ref.Key != "" {
		sel, serr := mamori.SelectKey(data, ref.Key)
		if serr != nil {
			return mamori.Value{}, serr
		}
		data = sel
	}

	version := variant
	if version == "" {
		version = mamori.VersionHash(data)
	}
	return mamori.Value{
		Bytes:     data,
		Version:   version,
		Sensitive: false, // a feature flag is configuration, not a secret
	}, nil
}

// evaluate dispatches to the pinned evaluation, or runs the auto chain. The
// client is resolved once here (conn() takes a mutex) rather than once per
// candidate type, so the untyped auto chain locks it at most once per
// Resolve instead of once per evaluation attempt.
func (p *Provider) evaluate(ctx context.Context, ref mamori.Ref, typ flagType) ([]byte, string, error) {
	cli := p.conn()
	if typ == typeAuto {
		return p.evaluateAuto(ctx, cli, ref)
	}
	return p.evaluateOne(ctx, cli, ref, typ)
}

// autoChain is the order the untyped fallback tries. object comes first
// because it is the only one that can carry a structured payload, and a
// provider that answers it can answer for any flag shape; bool is next because
// it is the most common flag type by far.
var autoChain = []flagType{typeObject, typeBool, typeString}

// evaluateAuto tries each type in autoChain, stopping at the first that does
// not report TYPE_MISMATCH.
//
// Stopping matters as much as ordering: every attempt is a real evaluation
// against the configured vendor, which may rate-limit or bill per call, so an
// author who knows the type should pin ?type= and pay exactly one.
func (p *Provider) evaluateAuto(ctx context.Context, cli of.IClient, ref mamori.Ref) ([]byte, string, error) {
	var lastErr error
	for _, typ := range autoChain {
		data, variant, err := p.evaluateOne(ctx, cli, ref, typ)
		if err == nil {
			return data, variant, nil
		}
		lastErr = err
		// Only a type mismatch is worth retrying with another shape. A missing
		// flag, an unready provider, or a bad context would give the same
		// answer for every type, so retrying would just multiply the failure.
		if !isTypeMismatch(err) {
			return nil, "", err
		}
	}
	// lastErr is never nil here (autoChain is non-empty, and the loop above
	// only falls through to this line after every iteration's error passed
	// isTypeMismatch, i.e. every one of them already classifies as
	// mamori.ErrInvalid), so it needs no further classification before being
	// wrapped.
	return nil, "", fmt.Errorf(
		"openfeature: flag %q could not be evaluated as object, bool, or string; pin the flag's type with ?type= (bool, string, int, float, object): %w",
		ref.Path, lastErr)
}

// isTypeMismatch reports whether err is a TYPE_MISMATCH from this provider's
// own evaluation - the only failure evaluateAuto retries with a different
// shape - by recovering the OpenFeature error code wrap attached with
// errors.As.
//
// This deliberately does not string-match err.Error(): a flag key or message
// that happens to contain another code's literal text (e.g. a key named
// "...TYPE_MISMATCH..." that actually fails with PARSE_ERROR) would
// substring-match and be retried as a type mismatch it is not, silently
// tripling the evaluation count and misclassifying the underlying failure.
func isTypeMismatch(err error) bool {
	var ee *evalError
	return errors.As(err, &ee) && ee.code == of.TypeMismatchCode
}

// evaluateOne performs a single typed evaluation and renders the result.
func (p *Provider) evaluateOne(ctx context.Context, cli of.IClient, ref mamori.Ref, typ flagType) ([]byte, string, error) {
	flag := ref.Path
	switch typ {
	case typeBool:
		d, err := cli.BooleanValueDetails(ctx, flag, false, p.evalCtx)
		if err != nil {
			return nil, "", p.wrap(flag, typ, d.ResolutionDetail, err)
		}
		return []byte(strconv.FormatBool(d.Value)), d.Variant, nil
	case typeString:
		d, err := cli.StringValueDetails(ctx, flag, "", p.evalCtx)
		if err != nil {
			return nil, "", p.wrap(flag, typ, d.ResolutionDetail, err)
		}
		return []byte(d.Value), d.Variant, nil
	case typeInt:
		d, err := cli.IntValueDetails(ctx, flag, 0, p.evalCtx)
		if err != nil {
			return nil, "", p.wrap(flag, typ, d.ResolutionDetail, err)
		}
		return []byte(strconv.FormatInt(d.Value, 10)), d.Variant, nil
	case typeFloat:
		d, err := cli.FloatValueDetails(ctx, flag, 0, p.evalCtx)
		if err != nil {
			return nil, "", p.wrap(flag, typ, d.ResolutionDetail, err)
		}
		return []byte(strconv.FormatFloat(d.Value, 'g', -1, 64)), d.Variant, nil
	case typeObject:
		d, err := cli.ObjectValueDetails(ctx, flag, nil, p.evalCtx)
		if err != nil {
			return nil, "", p.wrap(flag, typ, d.ResolutionDetail, err)
		}
		b, jerr := json.Marshal(d.Value)
		if jerr != nil {
			return nil, "", fmt.Errorf(
				"openfeature: flag %q evaluated to a value that is not JSON-encodable: %w: %w",
				flag, mamori.ErrInvalid, jerr)
		}
		return b, d.Variant, nil
	}
	// Unreachable: evaluate never calls evaluateOne with typeAuto (it routes
	// that to evaluateAuto instead), and the switch above exhaustively
	// handles every other flagType value. This panics rather than returning a
	// mamori.ErrInvalid because reaching it would be this package's own bug,
	// not a malformed ref or an OpenFeature-side failure - the two things
	// ErrInvalid is for.
	panic(fmt.Sprintf("openfeature: evaluateOne called with unhandled flag type %v", typ))
}

// evalError wraps an evaluation failure with the OpenFeature error code it
// came from, so a caller can recover the code with errors.As instead of
// searching the formatted message for a code's literal text - the latter
// false-positives whenever the flag key or an intermediate message happens to
// contain another code's name (a flag key containing "TYPE_MISMATCH" that
// actually fails with PARSE_ERROR, for instance). isTypeMismatch is the one
// caller that needs this.
type evalError struct {
	code of.ErrorCode
	err  error
}

func (e *evalError) Error() string { return e.err.Error() }
func (e *evalError) Unwrap() error { return e.err }

// wrap annotates an evaluation failure with the ref, classifies it from the
// resolution detail's ErrorCode, and returns it as an *evalError carrying that
// code for isTypeMismatch to recover.
//
// The code is read from the ResolutionDetail rather than the error, because
// openfeature.ResolutionError keeps its code unexported and offers no
// accessor; the detail struct is the only place the code is readable.
func (p *Provider) wrap(flag string, typ flagType, rd of.ResolutionDetail, err error) error {
	sentinel := classifyOF(rd.ErrorCode)
	var msg error
	if sentinel == nil {
		msg = fmt.Errorf("openfeature: evaluate %q as %s: %w", flag, typ, err)
	} else {
		msg = fmt.Errorf("openfeature: evaluate %q as %s [%s]: %w: %w",
			flag, typ, rd.ErrorCode, sentinel, err)
	}
	return &evalError{code: rd.ErrorCode, err: msg}
}

// classifyOF maps an OpenFeature error code onto a mamori classification
// sentinel, returning nil for a code mamori does not classify.
//
// GeneralCode is deliberately unmapped. It is OpenFeature's catch-all and says
// nothing about whether the cause is transient, a permission problem, or a
// bug, so guessing would send an operator down the wrong path. This mirrors
// how the aws provider leaves DecryptionFailure unclassified for the same
// reason.
func classifyOF(code of.ErrorCode) error {
	switch code {
	case of.FlagNotFoundCode:
		return mamori.ErrNotFound
	case of.TypeMismatchCode, of.ParseErrorCode, of.InvalidContextCode, of.TargetingKeyMissingCode:
		return mamori.ErrInvalid
	case of.ProviderNotReadyCode, of.ProviderFatalCode:
		return mamori.ErrUnavailable
	}
	return nil
}
