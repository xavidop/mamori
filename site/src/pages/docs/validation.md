---
layout: ../../layouts/DocsLayout.astro
title: Validation
---

# Validation

Add a `validate` tag to any field and mamori enforces it - on the initial `Load` **and on every reconciled update**. If a new value would make the struct invalid, the update is rejected atomically: `Get()` keeps returning the last good config and `OnError` receives a `*ValidationError`.

```go
type Config struct {
	LogLevel string `source:"env:LOG_LEVEL" default:"info" validate:"oneof=debug info warn error"`
	Workers  int    `source:"env:WORKERS" default:"4" validate:"gte=1,lte=256"`
	AdminURL string `source:"env:ADMIN_URL" validate:"omitempty,url"`
	Port     int    `source:"env:PORT" validate:"required,min=1,max=65535"`
}
```

## Which validators are available

The `validate` tag is [go-playground/validator/v10](https://github.com/go-playground/validator) syntax - the same library used by Gin, Echo, and many others. Any validator it ships works here. The most common ones:

| Tag | Meaning |
| --- | --- |
| `required` | Value must be non-zero. |
| `omitempty` | Skip the remaining rules if the value is empty. |
| `gte=` / `lte=` / `gt=` / `lt=` | Numeric (or length) bounds, e.g. `gte=1,lte=256`. |
| `min=` / `max=` / `len=` | Length for strings/slices/maps, value for numbers. |
| `oneof=a b c` | Must equal one of the space-separated values. |
| `eq=` / `ne=` | Equal / not equal to a value. |
| `email` | Valid email address. |
| `url` / `uri` / `http_url` | Valid URL / URI. |
| `hostname` / `fqdn` | Valid host name. |
| `ip` / `ipv4` / `ipv6` / `cidr` | IP address / CIDR. |
| `uuid` / `uuid4` | Valid UUID. |
| `alpha` / `alphanum` / `numeric` | Character-class checks. |
| `contains=` / `startswith=` / `endswith=` | Substring checks. |
| `e164` | Phone number in E.164 form. |
| `datetime=2006-01-02` | Parseable with the given Go time layout. |
| `dive` | Descend into a slice/map/array and apply the following rules to each element. |
| `eqfield=Other` / `nefield=Other` | Compare against another field on the struct. |

Combine rules with commas (AND): `validate:"required,gte=1,lte=256"`. The full catalogue is in the [validator docs](https://pkg.go.dev/github.com/go-playground/validator/v10#hdr-Baked_In_Validators_and_Tags).

## Nested structs and slices

Validation runs over the whole decoded struct, so nested structs are validated too. Use `dive` to validate slice or map elements:

```go
type Config struct {
	Redis    RedisConfig `source:"aws-sm://prod/redis" flatten:"json"` // RedisConfig's own validate tags run
	Origins  []string    `source:"env:ORIGINS" validate:"required,dive,url"` // each origin must be a URL
}
```

## Custom validation

Swap the validator entirely with `WithValidator`. Any type implementing `Validate(any) error` works, so you can register custom go-playground validators, or plug in a different engine:

```go
v := validator.New(validator.WithRequiredStructEnabled())
_ = v.RegisterValidation("dns1123", func(fl validator.FieldLevel) bool {
	return isDNS1123Label(fl.Field().String())
})

cfg, err := mamori.Load[Config](ctx,
	mamori.WithValidator(myAdapter{v}), // Validate(any) error wrapping v.Struct
)
```

## What failure looks like

- **At load**: `Load` / `Watch` return a `*ValidationError` and the zero value. Startup fails fast.
- **At runtime**: the update is discarded, `Get()` is unchanged, and `OnError` receives a `*ValidationError`. Unwrap it to reach the underlying validator error:

```go
mamori.OnError(func(err error) {
	var ve *mamori.ValidationError
	if errors.As(err, &ve) {
		log.Printf("rejected invalid config update: %v", ve)
	}
})
```
