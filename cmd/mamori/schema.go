// This file implements `mamori schema`: it reuses Extract (extract.go) to
// read a config struct's field types and `validate:` tags and emits a JSON
// Schema (draft 2020-12) describing the same shape, without resolving
// anything (decision D1, see extract.go).
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/xavidop/mamori/cmd/mamori/internal/sourcetag"
)

var schemaUsage = `usage: mamori schema [patterns...] [--type=Name] [--secret-schemes=list]

Schema reads Go source (via golang.org/x/tools/go/packages) and emits a
JSON Schema (draft 2020-12) derived from each source: tagged config
struct's field types and validate: tags. It never resolves anything (no
network calls, no secret managers contacted).

  patterns   Go package patterns to load (default: the current directory,
             same as omitting a pattern to "go build"). Example: ./...
  --type     only emit the schema for the struct type with this name
  --secret-schemes  comma-separated extra schemes to treat as secret-bearing
             when deciding which fields are sensitive, added to the built-in
             set. Use this for a custom provider, e.g.
             --secret-schemes=mysecrets,corp-kv

Output shape: if exactly one struct qualifies (a --type filter narrowed it
to one, or only one struct in the loaded packages carries a source: tag),
the output is a single JSON Schema document
({"$schema": "https://json-schema.org/draft/2020-12/schema", ...}), ready
to feed straight into any JSON Schema validator. If more than one struct
qualifies (no --type given and multiple structs match -- the same case
where "mamori explain" lists every matching struct instead of erroring),
the output is a JSON array of such documents, each carrying a "title" of
"package.TypeName" so they can be told apart.
`

// jsonSchemaDialect is the $schema value every emitted document declares:
// draft 2020-12, the dialect the task brief specifies.
const jsonSchemaDialect = "https://json-schema.org/draft/2020-12/schema"

// schemaCmd is the mamori schema subcommand. Like explainCmd, it writes to
// injected stdout/stderr writers (so tests never touch the real
// os.Stdout/os.Stderr) and returns the process exit code: 0 on success, 1
// on a usage or package-load error.
func schemaCmd(args []string, stdout, stderr io.Writer) int {
	if wantsHelp(args) {
		return writeHelp(stdout, schemaUsage)
	}
	patterns, typeName, schemes, err := parseSchemaArgs(args)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		_, _ = fmt.Fprint(stderr, schemaUsage)
		return 1
	}

	structs, err := Extract(patterns, typeName, schemes)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "mamori schema: %v\n", err)
		return 1
	}

	return writeSchema(stdout, stderr, structs)
}

// parseSchemaArgs splits args into package patterns and the --type flag. It
// scans by recognized flag shape rather than using flag.FlagSet, so
// patterns and flags may appear in either order, matching explainCmd's
// parseExplainArgs (explain.go).
// The returned schemes is nil unless --secret-schemes was given (see
// secretschemes.go), so the common case keeps using the built-in set.
func parseSchemaArgs(args []string) (patterns []string, typeName string, schemes sourcetag.SchemeSet, err error) {
	var extra string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if value, consumed, matchErr := matchSecretSchemes("schema", args, i); matchErr != nil {
			return nil, "", nil, matchErr
		} else if consumed > 0 {
			extra = value
			i += consumed - 1
			continue
		}
		switch {
		case a == "--type" || a == "-type":
			i++
			if i >= len(args) {
				return nil, "", nil, fmt.Errorf("mamori schema: %s requires a value", a)
			}
			typeName = args[i]
		case strings.HasPrefix(a, "--type="):
			typeName = strings.TrimPrefix(a, "--type=")
		case strings.HasPrefix(a, "-type="):
			typeName = strings.TrimPrefix(a, "-type=")
		case strings.HasPrefix(a, "-"):
			return nil, "", nil, fmt.Errorf("mamori schema: unknown flag %q", a)
		default:
			patterns = append(patterns, a)
		}
	}
	schemes, err = secretSchemeSet("schema", extra)
	if err != nil {
		return nil, "", nil, err
	}
	return patterns, typeName, schemes, nil
}

// writeSchema builds one schemaDoc per struct and writes them to stdout: a
// single bare document if exactly one struct qualifies, or a JSON array of
// documents (each tagged with a "title") if more than one does -- see
// schemaUsage's "Output shape" paragraph and the task report for why this
// shape was chosen over unconditionally wrapping in an array (matching how
// "mamori explain" never requires --type, it just lists everything found).
// It returns 1 only on an encoding failure, which should not happen in
// practice since schemaDoc/property are plain marshalable data.
func writeSchema(stdout, stderr io.Writer, structs []StructInfo) int {
	docs := make([]schemaDoc, len(structs))
	for i, s := range structs {
		docs[i] = structSchema(s)
	}

	var v any
	if len(docs) == 1 {
		v = docs[0]
	} else {
		v = docs
	}

	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "mamori schema: encoding JSON: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "%s\n", b)
	return 0
}

// schemaDoc is one struct's emitted JSON Schema document. Field order
// (Go struct field order, which encoding/json always preserves for a
// struct -- unlike a map, whose key order it does not) fixes the JSON key
// order: $schema, title, type, properties, required.
type schemaDoc struct {
	Schema     string             `json:"$schema"`
	Title      string             `json:"title"`
	Type       string             `json:"type"`
	Properties *orderedProperties `json:"properties"`
	Required   []string           `json:"required,omitempty"`
}

// structSchema translates one StructInfo (Extract's output: a flat list of
// Fields, dotted paths and all) into a schemaDoc.
func structSchema(s StructInfo) schemaDoc {
	root := newBuilderNode()
	for _, f := range s.Fields {
		root.insert(strings.Split(f.Path, "."), f)
	}
	prop := root.toProperty()
	return schemaDoc{
		Schema:     jsonSchemaDialect,
		Title:      s.Package + "." + s.TypeName,
		Type:       prop.Type,
		Properties: prop.Properties,
		Required:   prop.Required,
	}
}

// property is one JSON Schema node: a field's own translated shape, or (for
// an object node synthesized while re-nesting Extract's dotted paths, see
// builderNode) a nested group of properties. Every field is tagged
// omitempty and the type carries zero value (nil/""/0) whenever unset, so a
// leaf-scalar property and a nested-object property marshal to exactly the
// keys that apply to each, in this fixed order.
type property struct {
	Type       string             `json:"type,omitempty"`
	Items      *property          `json:"items,omitempty"`
	Properties *orderedProperties `json:"properties,omitempty"`
	Required   []string           `json:"required,omitempty"`
	Enum       []string           `json:"enum,omitempty"`
	Minimum    *float64           `json:"minimum,omitempty"`
	Maximum    *float64           `json:"maximum,omitempty"`
	MinLength  *int               `json:"minLength,omitempty"`
	MaxLength  *int               `json:"maxLength,omitempty"`
	Default    any                `json:"default,omitempty"`
}

// orderedProperties is a JSON object whose key order matches the order its
// keys were first set in, not Go's randomized map iteration order.
// encoding/json has no notion of an "ordered map" (a plain map[string]T
// would marshal with keys sorted... except sorting isn't what we want
// either: the brief calls for property order to match Extract's field
// declaration order, so this type implements json.Marshaler directly and
// writes each key/value pair in insertion order. This, plus every
// `required` slice being explicitly sorted (see builderNode.toProperty),
// is what makes schemaCmd's output byte-for-byte deterministic across runs
// (TestSchemaJSON asserts this).
type orderedProperties struct {
	keys   []string
	values map[string]property
}

func newOrderedProperties() *orderedProperties {
	return &orderedProperties{values: make(map[string]property)}
}

// set adds or replaces the value at key, recording key's insertion order
// only the first time it is set.
func (o *orderedProperties) set(key string, v property) {
	if _, exists := o.values[key]; !exists {
		o.keys = append(o.keys, key)
	}
	o.values[key] = v
}

// MarshalJSON writes o as a JSON object with keys in insertion order.
// json.MarshalIndent re-indents whatever compact JSON this returns (that is
// how encoding/json's indenting works for any value, custom marshalers
// included), so it need not build indented output itself.
func (o *orderedProperties) MarshalJSON() ([]byte, error) {
	var buf strings.Builder
	buf.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		kb, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		buf.Write(kb)
		buf.WriteByte(':')
		vb, err := json.Marshal(o.values[k])
		if err != nil {
			return nil, err
		}
		buf.Write(vb)
	}
	buf.WriteByte('}')
	return []byte(buf.String()), nil
}

// builderNode accumulates one level of a struct's dotted-path field list
// (Extract flattens nested, source-less struct fields into dotted paths,
// e.g. "Redis.Addr" -- see extract.go's walkFields doc comment) back into
// the nested-object shape a JSON Schema needs: each dot-separated path
// segment becomes a nested "properties" entry, and the final segment of a
// path carries that field's own translated property.
type builderNode struct {
	keys     []string
	children map[string]*builderNode
	field    *Field // set only on a leaf node (the final segment of some Field's Path)
}

func newBuilderNode() *builderNode {
	return &builderNode{children: make(map[string]*builderNode)}
}

// insert walks/creates the child chain for segments, recording each new
// segment's first-seen order, and attaches f to the final segment.
func (n *builderNode) insert(segments []string, f Field) {
	seg := segments[0]
	child, ok := n.children[seg]
	if !ok {
		child = newBuilderNode()
		n.children[seg] = child
		n.keys = append(n.keys, seg)
	}
	if len(segments) == 1 {
		child.field = &f
		return
	}
	child.insert(segments[1:], f)
}

// toProperty converts n into a property: a leaf node (n.field set)
// translates via fieldToProperty; an internal node becomes an object whose
// properties are its children (in first-seen order) and whose required
// array lists every direct child that is itself a required leaf field
// (a nested object is never, itself, listed as "required" -- only leaf
// fields carry Optional/HasDefault/validate:"required", so only leaf
// children can contribute to required).
func (n *builderNode) toProperty() property {
	if n.field != nil {
		return fieldToProperty(*n.field)
	}

	props := newOrderedProperties()
	var required []string
	for _, k := range n.keys {
		child := n.children[k]
		props.set(k, child.toProperty())
		if child.field != nil && isRequiredField(*child.field) {
			required = append(required, k)
		}
	}
	sort.Strings(required)
	return property{Type: "object", Properties: props, Required: required}
}

// isRequiredField reports whether f belongs in its parent object's
// "required" array: either its validate: tag explicitly says "required",
// or -- independently -- it is neither optional:"true" nor carries a
// default: tag (matching decode.go/core's own notion of what "required"
// means: a field with no default and not marked optional must have its
// source resolve to something, or mamori.New fails).
func isRequiredField(f Field) bool {
	if hasValidateRule(f.Validate, "required") {
		return true
	}
	return !f.Optional && !f.HasDefault
}

// fieldToProperty translates one leaf Field into a property: its Go type
// (via goTypeToProperty) plus whatever validate: constraints apply to that
// JSON type, plus a typed default: value if present.
func fieldToProperty(f Field) property {
	prop := goTypeToProperty(f.GoType)
	rules := parseValidateRules(f.Validate)

	if v, ok := rules["oneof"]; ok {
		prop.Enum = strings.Fields(v)
	}

	// Number-vs-string decision (kept consistent throughout this file):
	// gte/lte are numeric-only in go-playground/validator, so they only
	// ever apply to a number/integer field and always mean minimum/maximum
	// (a true numeric bound). min/max are overloaded by validator based on
	// the field's own kind: on a number/integer field they mean the same
	// thing as gte/lte (an inclusive numeric bound), but on a string field
	// they mean rune-count length, which JSON Schema represents with the
	// entirely different minLength/maxLength keywords, not minimum/maximum
	// (JSON Schema's minimum/maximum are numeric-comparison keywords and
	// are not defined for strings at all). Hence the switch on prop.Type
	// below, not on which tag name was used.
	switch prop.Type {
	case "integer", "number":
		if v, ok := firstRule(rules, "gte", "min"); ok {
			if n, err := strconv.ParseFloat(v, 64); err == nil {
				prop.Minimum = &n
			}
		}
		if v, ok := firstRule(rules, "lte", "max"); ok {
			if n, err := strconv.ParseFloat(v, 64); err == nil {
				prop.Maximum = &n
			}
		}
	case "string":
		if v, ok := rules["min"]; ok {
			if n, err := strconv.Atoi(v); err == nil {
				prop.MinLength = &n
			}
		}
		if v, ok := rules["max"]; ok {
			if n, err := strconv.Atoi(v); err == nil {
				prop.MaxLength = &n
			}
		}
	}

	if f.HasDefault {
		prop.Default = typedDefault(f.Default, prop.Type)
	}
	return prop
}

// goTypeToProperty maps a Go type (Field.GoType, e.g. "string",
// "secret.String", "[]string") to its JSON Schema shape. Slices recurse
// into an "items" schema for the element type; everything else is a leaf
// scalar via mapScalarGoType.
func goTypeToProperty(goType string) property {
	if goType != "[]byte" && strings.HasPrefix(goType, "[]") {
		elem := goTypeToProperty(strings.TrimPrefix(goType, "[]"))
		return property{Type: "array", Items: &elem}
	}
	return property{Type: mapScalarGoType(goType)}
}

// mapScalarGoType maps a non-slice Go type to a JSON Schema primitive type.
//
//   - string, secret.String -> "string"
//   - secret.Bytes, []byte -> "string" (JSON Schema has no byte-string
//     type; the closest fit is "string", documented here as base64-ish
//     since that is the conventional way to put raw bytes in JSON -- this
//     command does not itself encode/decode the value, only describes its
//     shape)
//   - bool -> "boolean"
//   - float32/float64 -> "number"
//   - every sized/unsized int and uint kind (plus the byte/rune aliases) ->
//     "integer"
//   - anything else (a Go type this command does not specifically
//     recognize, e.g. a named type with no special-cased mapping) ->
//     "string", the most permissive JSON Schema primitive, so an unmapped
//     type degrades to "accept any string" rather than the command
//     crashing or omitting the property entirely
func mapScalarGoType(goType string) string {
	switch goType {
	case "string", "secret.String":
		return "string"
	case "secret.Bytes", "[]byte":
		return "string"
	case "bool":
		return "boolean"
	case "float32", "float64":
		return "number"
	case "int", "int8", "int16", "int32", "int64",
		"uint", "uint8", "uint16", "uint32", "uint64", "uintptr",
		"byte", "rune":
		return "integer"
	default:
		return "string"
	}
}

// typedDefault converts a default: tag's raw string value to the JSON type
// its property's jsonType calls for: a JSON number for "integer"/"number"
// fields, the raw string unchanged otherwise (including when jsonType is
// "integer"/"number" but the raw value fails to parse as one -- a
// malformed default: tag should still show up in the emitted schema for a
// human to notice, not vanish or panic).
func typedDefault(raw, jsonType string) any {
	switch jsonType {
	case "integer":
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
			return n
		}
	case "number":
		if n, err := strconv.ParseFloat(raw, 64); err == nil {
			return n
		}
	}
	return raw
}

// parseValidateRules splits a go-playground/validator `validate:` tag into
// its comma-separated rules, each as a name -> param map (param is "" for a
// bare rule like "required"). This is not a full implementation of
// validator's tag grammar (it does not handle "|"-separated OR groups,
// "dive", or a comma escaped inside a param) -- only the handful of rules
// schema.go itself translates (required, oneof, gte, lte, min, max) are
// ever looked up in the result, and none of them need those escapes.
func parseValidateRules(tag string) map[string]string {
	rules := make(map[string]string)
	if tag == "" {
		return rules
	}
	for _, part := range strings.Split(tag, ",") {
		name, param, _ := strings.Cut(part, "=")
		rules[strings.TrimSpace(name)] = param
	}
	return rules
}

// hasValidateRule reports whether tag's parsed rules contain name (used for
// the bare "required" rule, which has no parameter to read).
func hasValidateRule(tag, name string) bool {
	_, ok := parseValidateRules(tag)[name]
	return ok
}

// firstRule returns the parameter of the first name in names present in
// rules, used where two validator tag spellings mean the same thing here
// (gte/min both mean "minimum" on a number; lte/max both mean "maximum").
func firstRule(rules map[string]string, names ...string) (string, bool) {
	for _, name := range names {
		if v, ok := rules[name]; ok {
			return v, true
		}
	}
	return "", false
}
