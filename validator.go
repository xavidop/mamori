package mamori

import "github.com/go-playground/validator/v10"

// Validator validates a fully-decoded config value. It is invoked on the initial
// load and on every reconciled update. The default implementation wraps
// go-playground/validator, reading `validate` struct tags. Supply an alternative
// with WithValidator.
type Validator interface {
	// Validate returns nil if v is valid, or a non-nil error describing the
	// failure (which mamori wraps in *ValidationError).
	Validate(v any) error
}

type playgroundValidator struct {
	v *validator.Validate
}

func defaultValidator() Validator {
	return &playgroundValidator{v: validator.New(validator.WithRequiredStructEnabled())}
}

func (p *playgroundValidator) Validate(v any) error { return p.v.Struct(v) }
