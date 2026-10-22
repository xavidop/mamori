package mamori

import (
	"context"
	"errors"
	"fmt"
)

// unknownSchemeError reports a ref whose scheme has no provider, neither
// per-call (WithProvider) nor registered. It wraps ErrInvalid so ErrorKind
// reports KindInvalid, matching how Load and Doctor classify the same mistake.
type unknownSchemeError struct{ scheme string }

func (e *unknownSchemeError) Error() string {
	return fmt.Sprintf("mamori: no provider registered for scheme %q", e.scheme)
}

func (e *unknownSchemeError) Unwrap() error { return ErrInvalid }

// Resolve resolves one ref string to a Value, without a config struct. It is
// the one-shot counterpart of Load for callers that hold a ref in a flag or a
// config file, such as "env:PGPASS" or "file:///run/secrets/pg".
//
// ref is parsed with [ParseRefs], so a precedence chain "a,b" is accepted and
// follows the same rule as a struct field's source tag: refs are tried in
// order, a not-found moves on to the next, and any other error stops the walk
// and is returned. If every ref is not found the error wraps [ErrNotFound]. A
// ${VAR} in ref expands from [WithRefVars] only, never the ambient
// environment, exactly as in a source tag.
//
// A provider is looked up per ref: one supplied with [WithProvider] overrides
// the global registry for its scheme. A scheme with no provider yields an
// error wrapping [ErrInvalid] (so [ErrorKind] reports [KindInvalid]).
// Selecting a #key or JSON Pointer fragment, and applying ?decode=, behave as
// in Load: the provider applies the fragment (see [SelectKey]) and the core
// applies the decode pipeline.
//
// A cancelled or expired ctx returns the context's error before any provider
// is called. Only options that affect a single resolve have an effect
// (WithProvider, WithRefVars, WithClock, WithMeter, WithTracer, WithLogger).
// Options that only mean something to a long-lived Watch, such as the poll
// interval, jitter, debounce, history, stale, derive, and OnError, are
// accepted and ignored.
//
// Errors name the ref in redacted form and never include resolved bytes.
func Resolve(ctx context.Context, ref string, opts ...Option) (Value, error) {
	if err := ctx.Err(); err != nil {
		return Value{}, err
	}
	o := defaultOptions()
	for _, opt := range opts {
		opt(o)
	}
	expanded, err := expandRefVars(ref, o.refVars)
	if err != nil {
		return Value{}, err
	}
	refs, err := ParseRefs(expanded)
	if err != nil {
		return Value{}, err
	}
	for _, r := range refs {
		if _, ok := o.provider(r.Scheme); !ok {
			return Value{}, &unknownSchemeError{scheme: r.Scheme}
		}
		val, err := resolveRef(ctx, r, o)
		if err == nil {
			return val, nil
		}
		if errors.Is(err, ErrNotFound) {
			continue
		}
		return Value{}, err
	}
	return Value{}, &ProviderError{Scheme: refs[0].Scheme, Ref: redactRef(refs[0]), Err: ErrNotFound}
}
