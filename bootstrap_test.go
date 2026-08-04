package mamori

import (
	"errors"
	"testing"
	"time"
)

// applyOpts runs opts against a fresh options value, as Watch does.
func applyOpts(t *testing.T, opts ...Option) *options {
	t.Helper()
	o := &options{}
	for _, opt := range opts {
		opt(o)
	}
	return o
}

func TestWithBootstrapCacheDefaults(t *testing.T) {
	o := applyOpts(t, WithBootstrapCache("/tmp/snap", testKey(t)))
	if o.bootstrap == nil {
		t.Fatal("bootstrap config not set")
	}
	if o.bootstrap.maxAge != DefaultBootstrapMaxAge {
		t.Fatalf("maxAge = %v, want %v", o.bootstrap.maxAge, DefaultBootstrapMaxAge)
	}
	if o.bootstrap.err != nil {
		t.Fatalf("unexpected error: %v", o.bootstrap.err)
	}
}

func TestBootstrapMaxAgeOverrides(t *testing.T) {
	o := applyOpts(t, WithBootstrapCache("/tmp/snap", testKey(t), BootstrapMaxAge(2*time.Hour)))
	if o.bootstrap.maxAge != 2*time.Hour {
		t.Fatalf("maxAge = %v, want 2h", o.bootstrap.maxAge)
	}
}

// TestWithBootstrapCacheRejectsABadKey pins that a wrong-sized key fails at
// construction. Deferring it to the first write means the process learns its
// fallback was never viable only once it needs it.
func TestWithBootstrapCacheRejectsABadKey(t *testing.T) {
	for _, n := range []int{0, 16, 31, 33} {
		o := applyOpts(t, WithBootstrapCache("/tmp/snap", make([]byte, n)))
		if o.bootstrap == nil || o.bootstrap.err == nil {
			t.Fatalf("a %d-byte key was accepted", n)
		}
		if !errors.Is(o.bootstrap.err, ErrInvalid) {
			t.Fatalf("err = %v, want ErrInvalid", o.bootstrap.err)
		}
	}
}

func TestWithBootstrapCacheRejectsAnEmptyPath(t *testing.T) {
	o := applyOpts(t, WithBootstrapCache("", testKey(t)))
	if o.bootstrap == nil || !errors.Is(o.bootstrap.err, ErrInvalid) {
		t.Fatalf("an empty path was accepted: %+v", o.bootstrap)
	}
}

// TestBootstrapMaxAgeZeroIsUnbounded pins that zero is an explicit opt-out
// rather than an accident, since the option must be written to reach it.
func TestBootstrapMaxAgeZeroIsUnbounded(t *testing.T) {
	o := applyOpts(t, WithBootstrapCache("/tmp/snap", testKey(t), BootstrapMaxAge(0)))
	if o.bootstrap.maxAge != 0 {
		t.Fatalf("maxAge = %v, want 0", o.bootstrap.maxAge)
	}
}

// TestBootstrapMaxAgeRejectsANegativeDuration pins that a negative bound fails
// at construction rather than being clamped. Clamping it to zero would turn a
// sign typo into the unbounded mode BootstrapMaxAge insists on writing out.
func TestBootstrapMaxAgeRejectsANegativeDuration(t *testing.T) {
	for _, d := range []time.Duration{-time.Nanosecond, -time.Hour} {
		o := applyOpts(t, WithBootstrapCache("/tmp/snap", testKey(t), BootstrapMaxAge(d)))
		if o.bootstrap == nil || o.bootstrap.err == nil {
			t.Fatalf("a %s max age was accepted", d)
		}
		if !errors.Is(o.bootstrap.err, ErrInvalid) {
			t.Fatalf("err = %v, want ErrInvalid", o.bootstrap.err)
		}
	}
}

// TestBootstrapKeyIsCopied pins that mutating the caller's key slice after the
// option is built cannot change what the snapshot is sealed with.
func TestBootstrapKeyIsCopied(t *testing.T) {
	key := testKey(t)
	o := applyOpts(t, WithBootstrapCache("/tmp/snap", key))
	before := append([]byte(nil), o.bootstrap.key...)
	for i := range key {
		key[i] = 0xff
	}
	for i := range before {
		if o.bootstrap.key[i] != before[i] {
			t.Fatal("mutating the caller's slice changed the stored key")
		}
	}
}
