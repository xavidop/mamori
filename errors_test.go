package mamori

import (
	"errors"
	"testing"
)

func TestProviderErrorIsNotFound(t *testing.T) {
	err := &ProviderError{Scheme: "aws-sm", Ref: "prod/db", Err: ErrNotFound}
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("errors.Is(ProviderError{ErrNotFound}, ErrNotFound) = false, want true")
	}
	var pe *ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("errors.As did not match *ProviderError")
	}
	if pe.Scheme != "aws-sm" {
		t.Errorf("Scheme = %q, want aws-sm", pe.Scheme)
	}
}

func TestValidationErrorUnwrap(t *testing.T) {
	base := errors.New("field Workers must be <= 256")
	err := &ValidationError{Err: base}
	if !errors.Is(err, base) {
		t.Fatalf("ValidationError does not unwrap to base")
	}
}

func TestStaleErrorUnwrap(t *testing.T) {
	err := &StaleError{Ref: "vault://x", Err: ErrNotFound}
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("StaleError does not unwrap to ErrNotFound")
	}
}
