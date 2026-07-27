package mamori

import (
	"context"
	"errors"
	"fmt"
)

// resolved pairs a fieldSpec with the value obtained for it.
type resolved struct {
	spec  fieldSpec
	value Value // populated bytes (from provider or default)
	found bool  // true if the provider returned a value (not a default)
	set   bool  // true if the field should be set (found or default applied)
}

// resolveAll resolves every spec, applying defaults and optional handling. It
// batches refs per provider when the provider implements BatchProvider. It fails
// fast on any non-not-found error.
//
// Batch grouping (spec 10.6) only covers single-ref specs: it groups
// spec.Refs[0] by scheme exactly as it did before chains existed. A fully
// correct implementation would group by (scheme, chain position) - batching
// all position-0 refs of a scheme together, then feeding the not-found
// remainder into a second round for position-1 refs, and so on - but that
// turns resolveBatchScheme into a multi-round process for comparatively rare
// input. Single-ref fields are the overwhelming common case and keep the
// existing batch optimization byte-for-byte; a field with more than one ref
// (an opt-in chain) is instead resolved ref-by-ref through resolveChain,
// paying one extra round trip per provider call instead of joining a batch.
// Correct now beats clever later.
func resolveAll(ctx context.Context, specs []fieldSpec, o *options) ([]resolved, error) {
	out := make([]resolved, len(specs))
	for i := range specs {
		out[i] = resolved{spec: specs[i]}
	}

	// Group single-ref spec indices by scheme so batch providers get one call.
	byScheme := map[string][]int{}
	var chained []int
	for i, s := range specs {
		if len(s.Refs) == 1 {
			byScheme[s.Refs[0].Scheme] = append(byScheme[s.Refs[0].Scheme], i)
			continue
		}
		chained = append(chained, i)
	}

	for scheme, idxs := range byScheme {
		p, ok := o.provider(scheme)
		if !ok {
			return nil, fmt.Errorf("mamori: no provider registered for scheme %q (field %s)", scheme, specs[idxs[0]].Path)
		}

		if bp, ok := p.(BatchProvider); ok {
			if err := resolveBatchScheme(ctx, bp, scheme, idxs, out, o); err != nil {
				return nil, err
			}
			continue
		}
		for _, i := range idxs {
			if err := resolveOne(ctx, &out[i], o); err != nil {
				return nil, err
			}
		}
	}

	for _, i := range chained {
		if err := resolveOne(ctx, &out[i], o); err != nil {
			return nil, err
		}
	}

	return out, nil
}

// resolveOne resolves r's precedence chain (a single ref, in the common case,
// or a multi-ref chain) and applies default/optional/onfail handling to
// whatever terminal condition resolveChain reports. It is the one-shot Load
// counterpart to probeField, which walks the same chain without applying any
// of that policy.
func resolveOne(ctx context.Context, r *resolved, o *options) error {
	val, _, err := resolveChain(ctx, r.spec.Refs, o)
	if err == nil {
		setResolved(r, val)
		return nil
	}
	if errors.Is(err, ErrNotFound) {
		return applyDefault(r)
	}
	return applyOnFail(r, err)
}

// resolveChain walks refs in declaration order and returns the winning value
// together with the index of the ref that produced it, or a terminal error
// describing why the walk stopped without a winner. It is the single walk
// shared by resolveOne (Load), probeField (Doctor), and, later, chain
// watching: every caller that needs to know which source wins a field's
// precedence chain goes through this function so the rule is defined once.
//
// A chain expresses precedence, not failover (spec 10.3, decision D2):
//
//  1. A ref that yields a value wins outright; the walk stops there.
//  2. A ref reporting ErrNotFound falls through to the next ref.
//  3. A ref reporting any other error (permission denied, unavailable, rate
//     limited, an unregistered scheme, ...) stops the walk immediately. Lower-
//     priority refs are deliberately NOT tried: sliding down to a
//     lower-precedence source because a higher-priority one is transiently
//     broken would make config resolution depend on backend health instead of
//     the order the caller declared, and would make it non-deterministic
//     under partial failure. That error is returned as the terminal error;
//     the caller decides what to do with it (e.g. applying the field's
//     onfail policy). resolveChain itself makes no such policy decision.
//  4. Every ref reports ErrNotFound: the returned error wraps ErrNotFound
//     (attributed to refs[0], the field's primary/highest-precedence ref) so
//     a caller can apply default:/optional handling exactly as it does today
//     for a single ref.
//
// On success the returned int is the winning ref's index into refs; on
// failure it is -1.
func resolveChain(ctx context.Context, refs []Ref, o *options) (Value, int, error) {
	for i, ref := range refs {
		val, err := resolveRef(ctx, ref, o)
		if err == nil {
			return val, i, nil
		}
		if errors.Is(err, ErrNotFound) {
			continue
		}
		return Value{}, -1, err
	}
	first := refs[0]
	return Value{}, -1, &ProviderError{Scheme: first.Scheme, Ref: redactRef(first), Err: ErrNotFound}
}

// resolveRef resolves a single ref through its provider, recording
// tracer/meter observability for the call and wrapping any failure -
// including an unregistered scheme, which can never resolve for this process
// - as a *ProviderError tagged with that ref's scheme and redacted form.
//
// It applies the ref's ?decode= pipeline before returning, so every caller
// downstream (resolveChain, and through it Load, Doctor, and the reconciler's
// re-resolves) sees decoded bytes and nothing has to remember to decode. The
// decode happens after the observability calls deliberately: the tracer span
// and the meter's latency both describe the provider round trip, and folding a
// local CPU-bound transform into that measurement would misattribute it to the
// backend. A decode failure is reported as a *ProviderError for the same ref,
// because from every caller's point of view this ref did not yield a usable
// value - and because ErrInvalid is a non-not-found error, it stops a
// precedence chain's walk rather than falling through to a lower-priority
// source (resolveChain case 3). Sliding down to a different source because the
// declared encoding is wrong would hide the misconfiguration behind whatever
// answered next.
func resolveRef(ctx context.Context, ref Ref, o *options) (Value, error) {
	p, ok := o.provider(ref.Scheme)
	if !ok {
		// A ref naming a provider that is not wired into this process is a
		// malformed configuration for this process, not a missing value: it
		// can never resolve regardless of default/optional handling, so it is
		// reported as ErrInvalid rather than ErrNotFound (matches Doctor's
		// existing classification for the single-ref case).
		return Value{}, &ProviderError{Scheme: ref.Scheme, Ref: redactRef(ref), Err: ErrInvalid}
	}
	start := o.clock.Now()
	sctx, finish := o.tracer.StartResolve(ctx, ref.Scheme, ref.Raw)
	val, err := p.Resolve(sctx, ref)
	finish(err)
	o.meter.RecordResolve(ref.Scheme, o.clock.Now().Sub(start), err)
	if err != nil {
		return Value{}, &ProviderError{Scheme: ref.Scheme, Ref: redactRef(ref), Err: err}
	}
	val, err = applyDecode(ref, val)
	if err != nil {
		return Value{}, &ProviderError{Scheme: ref.Scheme, Ref: redactRef(ref), Err: err}
	}
	return val, nil
}

// applyOnFail applies spec.OnFail to a terminal, non-not-found chain error
// (resolveChain case 3) on the one-shot Load path.
//
// The `default:` tag applies ONLY on genuine absence (every ref in the chain
// reporting ErrNotFound); it must never be silently triggered by an error. A
// permission-denied, unavailable, or misconfigured-provider error masked by a
// default is exactly the footgun that classifying a missing exec binary as
// KindUnknown (rather than ErrNotFound) exists to prevent elsewhere in this
// package: an error must fail loudly unless the field explicitly opts in to
// tolerating it.
//
// onFailKeepLast (the default, no `onfail` tag) therefore keeps the last
// applied value when Watch has one; on an initial Load there is no last value
// to keep, so it fails, exactly like onFailFail. onFailUseDefault is the only
// way to have an error fall back to `default:`, and doing so is an explicit,
// visible opt-in on the field's tag (`onfail:"default"`) rather than the
// silent, implicit behavior a bare `default:` tag would otherwise produce.
func applyOnFail(r *resolved, terminal error) error {
	if r.spec.OnFail == onFailUseDefault {
		// walkSpecs rejects onfail:"default" without a default: tag at
		// spec-parse time, so HasDefault is always true here.
		r.value = Value{Bytes: []byte(r.spec.Default), Sensitive: r.spec.Sensitive, Version: "default"}
		r.found = false
		r.set = true
		return nil
	}
	// onFailKeepLast and onFailFail both fail on an initial Load: keeplast has
	// no prior value to fall back to, and fail always rejects.
	return terminal
}

// resolveBatchScheme resolves every single-ref spec of one batch-capable
// scheme in a single provider call, then applies each ref's own ?decode=
// pipeline to its result.
//
// The decode cannot be inherited from resolveRef here: a single-ref field
// whose scheme implements BatchProvider never goes through resolveRef at all
// (see resolveAll's grouping), so this is an independent entry point for a
// Value and needs its own wiring. Decoding is per-ref rather than per-batch
// because the batch groups by scheme only - two fields of the same scheme can
// declare completely different codings, or none.
//
// A decode failure is routed through applyOnFail, exactly as resolveOne routes
// the terminal error of a non-batch resolve. Whether a scheme's provider
// happens to implement BatchProvider is an implementation detail the operator
// did not choose and cannot see from the struct tag, so it must not decide
// whether the field's `onfail` policy is honored: without this, a field tagged
// onfail:"default" would fall back to its default on a provider that only
// implements Resolve and fail the whole Load on one that also implements
// ResolveBatch, for the same tag and the same undecodable payload.
//
// applyOnFail, not applyDefault, is the right counterpart here. The two are
// not interchangeable: applyDefault applies `default:` on genuine absence,
// which is the !ok branch above, while applyOnFail applies `default:` to an
// error only when the field explicitly opted in with onfail:"default". A
// decode failure is an error, not an absence, so a bare `default:` tag must
// not silently absorb it - that is the same footgun applyOnFail's own doc
// comment exists to prevent. With the default onFailKeepLast (or
// onfail:"fail") applyOnFail returns the error unchanged and the Load still
// fails fast, which is the previous behavior for every field that did not opt
// in.
func resolveBatchScheme(ctx context.Context, bp BatchProvider, scheme string, idxs []int, out []resolved, o *options) error {
	refs := make([]Ref, 0, len(idxs))
	for _, i := range idxs {
		refs = append(refs, out[i].spec.Refs[0])
	}
	start := o.clock.Now()
	got, err := bp.ResolveBatch(ctx, refs)
	o.meter.RecordResolve(scheme, o.clock.Now().Sub(start), err)
	if err != nil {
		return &ProviderError{Scheme: scheme, Ref: fmt.Sprintf("batch(%d)", len(refs)), Err: err}
	}
	for _, i := range idxs {
		r := &out[i]
		val, ok := got[r.spec.Refs[0].Raw]
		if !ok {
			if err := applyDefault(r); err != nil {
				return err
			}
			continue
		}
		dec, derr := applyDecode(r.spec.Refs[0], val)
		if derr != nil {
			pe := &ProviderError{Scheme: scheme, Ref: redactRef(r.spec.Refs[0]), Err: derr}
			if err := applyOnFail(r, pe); err != nil {
				return err
			}
			continue
		}
		setResolved(r, dec)
	}
	return nil
}

func setResolved(r *resolved, val Value) {
	if r.spec.Sensitive {
		val.Sensitive = true
	}
	r.value = val
	r.found = true
	r.set = true
}

func applyDefault(r *resolved) error {
	switch {
	case r.spec.HasDefault:
		r.value = Value{Bytes: []byte(r.spec.Default), Sensitive: r.spec.Sensitive, Version: "default"}
		r.found = false
		r.set = true
		return nil
	case r.spec.Optional:
		r.found = false
		r.set = false
		return nil
	default:
		return &ProviderError{Scheme: r.spec.Refs[0].Scheme, Ref: redactRef(r.spec.Refs[0]), Err: ErrNotFound}
	}
}
