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
func resolveAll(ctx context.Context, specs []fieldSpec, o *options) ([]resolved, error) {
	out := make([]resolved, len(specs))
	for i := range specs {
		out[i] = resolved{spec: specs[i]}
	}

	// Group spec indices by scheme so batch providers get one call.
	byScheme := map[string][]int{}
	for i, s := range specs {
		byScheme[s.Ref.Scheme] = append(byScheme[s.Ref.Scheme], i)
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
			if err := resolveOne(ctx, p, scheme, &out[i], o); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

func resolveOne(ctx context.Context, p Provider, scheme string, r *resolved, o *options) error {
	start := o.clock.Now()
	sctx, finish := o.tracer.StartResolve(ctx, scheme, r.spec.Ref.Raw)
	val, err := p.Resolve(sctx, r.spec.Ref)
	finish(err)
	o.meter.RecordResolve(scheme, o.clock.Now().Sub(start), err)

	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return applyDefault(r)
		}
		return &ProviderError{Scheme: scheme, Ref: r.spec.Ref.Raw, Err: err}
	}
	setResolved(r, val)
	return nil
}

func resolveBatchScheme(ctx context.Context, bp BatchProvider, scheme string, idxs []int, out []resolved, o *options) error {
	refs := make([]Ref, 0, len(idxs))
	for _, i := range idxs {
		refs = append(refs, out[i].spec.Ref)
	}
	start := o.clock.Now()
	got, err := bp.ResolveBatch(ctx, refs)
	o.meter.RecordResolve(scheme, o.clock.Now().Sub(start), err)
	if err != nil {
		return &ProviderError{Scheme: scheme, Ref: fmt.Sprintf("batch(%d)", len(refs)), Err: err}
	}
	for _, i := range idxs {
		r := &out[i]
		val, ok := got[r.spec.Ref.Raw]
		if !ok {
			if err := applyDefault(r); err != nil {
				return err
			}
			continue
		}
		setResolved(r, val)
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
		return &ProviderError{Scheme: r.spec.Ref.Scheme, Ref: r.spec.Ref.Raw, Err: ErrNotFound}
	}
}
