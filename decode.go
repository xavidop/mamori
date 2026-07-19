package mamori

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/go-viper/mapstructure/v2"
	"github.com/xavidop/mamori/secret"
	"gopkg.in/yaml.v3"
)

// fieldSpec describes one configurable leaf field discovered by walking a config
// struct: where its value comes from and how to decode it.
type fieldSpec struct {
	Path       string // dotted path, e.g. "Redis.Addr"
	Ref        Ref    // parsed source ref
	Default    string // value used on ErrNotFound
	HasDefault bool
	Flatten    string       // "", "json", "yaml", or "env"
	Optional   bool         // not-found tolerated without a default
	Index      []int        // reflect field index path from the root struct
	Type       reflect.Type // field type
	Sensitive  bool         // field is secret.String / secret.Bytes
}

var (
	secretStringType = reflect.TypeOf(secret.String{})
	secretBytesType  = reflect.TypeOf(secret.Bytes{})
	byteSliceType    = reflect.TypeOf([]byte(nil))
	durationType     = reflect.TypeOf(time.Duration(0))
)

// fieldSpecs walks the exported fields of struct type t and returns the leaf
// specs. Nested structs without a `source` tag are recursed into (their fields
// carry their own sources); a struct field WITH a `source` tag must also carry a
// `flatten` tag and is treated as a single decoded payload.
func fieldSpecs(t reflect.Type) ([]fieldSpec, error) {
	if t.Kind() != reflect.Struct {
		return nil, fmt.Errorf("mamori: config type %s is not a struct", t)
	}
	return walkSpecs(t, "", nil)
}

func walkSpecs(t reflect.Type, prefix string, index []int) ([]fieldSpec, error) {
	var specs []fieldSpec
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		path := f.Name
		if prefix != "" {
			path = prefix + "." + f.Name
		}
		idx := append(append([]int(nil), index...), i)

		source, hasSource := f.Tag.Lookup("source")
		flatten := f.Tag.Get("flatten")
		def, hasDefault := f.Tag.Lookup("default")
		optional := f.Tag.Get("optional") == "true"

		isSecret := f.Type == secretStringType || f.Type == secretBytesType
		isLeafStruct := f.Type.Kind() == reflect.Struct && !isSecret

		// A plain nested struct (no source) is a container: recurse.
		if isLeafStruct && !hasSource {
			child, err := walkSpecs(f.Type, path, idx)
			if err != nil {
				return nil, err
			}
			specs = append(specs, child...)
			continue
		}

		if !hasSource {
			// No source and not a container struct: nothing to resolve. Skip
			// (leaves the zero value / allows manual population).
			continue
		}

		ref, err := ParseRef(source)
		if err != nil {
			return nil, fmt.Errorf("mamori: field %s: %w", path, err)
		}

		if isLeafStruct && flatten == "" {
			return nil, fmt.Errorf("mamori: field %s is a struct with a source but no flatten tag; add flatten:\"json|yaml|env\"", path)
		}

		// Per-field debounce override travels on the ref opts (?debounce=...).
		specs = append(specs, fieldSpec{
			Path:       path,
			Ref:        ref,
			Default:    def,
			HasDefault: hasDefault,
			Flatten:    flatten,
			Optional:   optional,
			Index:      idx,
			Type:       f.Type,
			Sensitive:  isSecret,
		})
	}
	return specs, nil
}

// setField decodes raw bytes into the struct field at spec.Index of the struct
// value root (which must be a settable struct reflect.Value). hooks are extra
// user mapstructure decode hooks applied on the flatten path.
func setField(root reflect.Value, spec fieldSpec, raw []byte, hooks []mapstructure.DecodeHookFunc) error {
	fv := root.FieldByIndex(spec.Index)
	if !fv.CanSet() {
		return fmt.Errorf("mamori: field %s is not settable", spec.Path)
	}

	if spec.Flatten != "" {
		return decodeFlatten(fv, spec, raw, hooks)
	}
	return decodeScalar(fv, spec, raw)
}

func decodeScalar(fv reflect.Value, spec fieldSpec, raw []byte) error {
	switch spec.Type {
	case secretStringType:
		fv.Set(reflect.ValueOf(secret.NewStringBytes(append([]byte(nil), raw...))))
		return nil
	case secretBytesType:
		fv.Set(reflect.ValueOf(secret.NewBytes(append([]byte(nil), raw...))))
		return nil
	case byteSliceType:
		fv.SetBytes(append([]byte(nil), raw...))
		return nil
	case durationType:
		d, err := time.ParseDuration(strings.TrimSpace(string(raw)))
		if err != nil {
			return fmt.Errorf("mamori: field %s: invalid duration %q: %w", spec.Path, raw, err)
		}
		fv.SetInt(int64(d))
		return nil
	}

	s := strings.TrimSpace(string(raw))
	switch fv.Kind() {
	case reflect.String:
		fv.SetString(string(raw))
	case reflect.Bool:
		b, err := strconv.ParseBool(s)
		if err != nil {
			return fmt.Errorf("mamori: field %s: invalid bool %q: %w", spec.Path, s, err)
		}
		fv.SetBool(b)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return fmt.Errorf("mamori: field %s: invalid int %q: %w", spec.Path, s, err)
		}
		fv.SetInt(n)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			return fmt.Errorf("mamori: field %s: invalid uint %q: %w", spec.Path, s, err)
		}
		fv.SetUint(n)
	case reflect.Float32, reflect.Float64:
		n, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return fmt.Errorf("mamori: field %s: invalid float %q: %w", spec.Path, s, err)
		}
		fv.SetFloat(n)
	default:
		return fmt.Errorf("mamori: field %s: unsupported type %s", spec.Path, spec.Type)
	}
	return nil
}

// decodeFlatten decodes a single provider payload into a (possibly nested)
// struct field, per the flatten tag. It supports json, yaml, and env (KEY=VALUE
// lines). Secret and duration fields inside the flattened struct are handled via
// mapstructure decode hooks.
func decodeFlatten(fv reflect.Value, spec fieldSpec, raw []byte, hooks []mapstructure.DecodeHookFunc) error {
	var m map[string]any
	switch spec.Flatten {
	case "json":
		if err := json.Unmarshal(raw, &m); err != nil {
			return fmt.Errorf("mamori: field %s: json flatten: %w", spec.Path, err)
		}
	case "yaml":
		if err := yaml.Unmarshal(raw, &m); err != nil {
			return fmt.Errorf("mamori: field %s: yaml flatten: %w", spec.Path, err)
		}
	case "env":
		m = parseEnvPayload(raw)
	default:
		return fmt.Errorf("mamori: field %s: unknown flatten %q", spec.Path, spec.Flatten)
	}

	// The built-in secret/duration hook runs first, then any user hooks.
	decodeHooks := append([]mapstructure.DecodeHookFunc{flattenHook()}, hooks...)
	dec, err := mapstructure.NewDecoder(&mapstructure.DecoderConfig{
		Result:           fv.Addr().Interface(),
		WeaklyTypedInput: true,
		TagName:          "mapstructure",
		DecodeHook:       mapstructure.ComposeDecodeHookFunc(decodeHooks...),
	})
	if err != nil {
		return fmt.Errorf("mamori: field %s: decoder: %w", spec.Path, err)
	}
	if err := dec.Decode(m); err != nil {
		return fmt.Errorf("mamori: field %s: flatten decode: %w", spec.Path, err)
	}
	return nil
}

// flattenHook converts scalar inputs into secret and duration fields inside a
// flattened struct.
func flattenHook() mapstructure.DecodeHookFunc {
	return func(from reflect.Type, to reflect.Type, data any) (any, error) {
		switch to {
		case secretStringType:
			return secret.NewString(fmt.Sprint(data)), nil
		case secretBytesType:
			return secret.NewBytes([]byte(fmt.Sprint(data))), nil
		case durationType:
			if s, ok := data.(string); ok {
				return time.ParseDuration(s)
			}
		}
		return data, nil
	}
}

func parseEnvPayload(raw []byte) map[string]any {
	m := map[string]any{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		m[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return m
}
