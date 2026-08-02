// This file implements the static-extraction engine shared by explain,
// schema, policy, and doctor --compare (decision D1: the CLI's static
// commands read source and never resolve anything). It loads Go packages
// with golang.org/x/tools/go/packages and reads the `source`/`default`/
// `optional`/`onfail`/`validate` struct tags the same way core's decode.go
// (and validator.go, for `validate`) do, so what the CLI reports matches
// what mamori.New would actually wire up.
package main

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"reflect"
	"sort"
	"strings"

	"golang.org/x/tools/go/packages"

	"github.com/xavidop/mamori/cmd/mamori/internal/sourcetag"
)

// FieldKind says why a field appears in a StructInfo, so each command can take
// the kinds it can act on. Extract has four consumers and they disagree: policy
// emits permissions from refs, doctor --compare diffs paths against a live
// report, schema describes everything mamori validates, and explain lists what
// mamori reads.
type FieldKind string

const (
	// KindSource carries a source: tag. Every consumer wants these.
	KindSource FieldKind = "source"
	// KindDerived is named by a WithDerive call's declared write paths. It has
	// no ref, so it grants no permissions and cannot appear in a live report.
	KindDerived FieldKind = "derived"
	// KindValidate has no source: tag but does carry validate: rules, which
	// mamori enforces on every load and update because the validator runs
	// against the whole struct. Only schema wants these.
	KindValidate FieldKind = "validate"
)

// Field describes one configurable leaf field discovered while walking a
// config struct: where its value comes from (its source tag) and how it is
// meant to be decoded. It mirrors core's unexported fieldSpec (decode.go)
// closely enough that the two never drift in what "a field" means, without
// either depending on the other (the CLI never resolves, decision D1).
type Field struct {
	Path   string    // dotted path from the struct root, e.g. "Redis.Password"
	Kind   FieldKind // why this field is here; see FieldKind
	GoType string    // the field's Go type, e.g. "string", "secret.String"
	Source string    // the raw source tag, e.g. "env:X,aws-sm://s"

	Refs []string // Source split into its precedence chain (sourcetag.SplitChain)

	Default    string // the default: tag value, meaningful only if HasDefault
	HasDefault bool   // whether a default: tag was present at all

	Optional  bool // optional:"true"
	Sensitive bool // secret.String/secret.Bytes field, or a secret-bearing scheme in Refs

	OnFail string // the onfail: tag value (keeplast/default/fail), or "" if absent

	Validate string // the raw validate: tag value (e.g. "gte=1,lte=256"), or "" if absent
}

// StructInfo is every source-tagged field discovered in one struct type.
type StructInfo struct {
	Package  string // the struct's package path
	TypeName string // the struct's type name
	Fields   []Field

	// DerivesIncomplete is true when at least one WithDerive call site
	// targeting this struct (see findDerives, derives.go) declared a write
	// path that was not a string literal (a variable, a spread slice, ...),
	// which cannot be read without running the program. When true, the
	// KindDerived fields already listed in Fields may not be every field this
	// struct's hooks actually write. explain prints a note when this is true
	// rather than silently listing an incomplete set as if it were complete.
	DerivesIncomplete bool
}

// Extract loads the Go packages matching patterns and returns a StructInfo
// for every struct type that has at least one field carrying a `source`
// struct tag, in deterministic order: packages sorted by path, structs in
// source declaration order within each package, fields in struct
// declaration order (with nested, source-less struct fields recursed into
// and their fields contributing dotted paths). A struct that qualifies also
// gets one KindDerived field appended per WithDerive-declared write path
// findDerives (derives.go) found for it; a struct with no source-tagged
// field at all is never returned, even if some WithDerive call names it -
// see findDerives's own doc comment.
//
// When typeName is non-empty, only the struct named typeName is returned
// (still recursing into any nested, source-less struct fields it has).
//
// schemes is the set of source schemes to treat as secret-bearing when
// deciding a field's Sensitive flag. Pass nil for mamori's built-in set;
// commands that accept --secret-schemes pass an extended one.
func Extract(patterns []string, typeName string, schemes sourcetag.SchemeSet) ([]StructInfo, error) {
	// A nil set means the built-ins, resolved to the shared package-level set
	// rather than a freshly built map per call.
	firstSensitive := sourcetag.FirstSensitiveScheme
	if schemes != nil {
		firstSensitive = schemes.FirstSensitiveScheme
	}

	sorted, err := loadPackages(patterns)
	if err != nil {
		return nil, err
	}

	// declared/incomplete come from findDerives (derives.go): every
	// WithDerive-declared write path found anywhere in the loaded packages,
	// keyed by "pkgpath.TypeName" of the config type that owns it, plus
	// which of those keys had at least one path that could not be read
	// statically.
	declared, incomplete := findDerives(sorted)

	var out []StructInfo
	for _, pkg := range sorted {
		for _, s := range taggedStructs(pkg) {
			if typeName != "" && s.name != typeName {
				continue
			}
			key := pkg.PkgPath + "." + s.name
			fields := walkFields(pkg.Types, s.typ, "", firstSensitive)
			fields = append(fields, derivedFields(pkg.Types, s.typ, declared[key])...)
			out = append(out, StructInfo{
				Package:           pkg.PkgPath,
				TypeName:          s.name,
				Fields:            fields,
				DerivesIncomplete: incomplete[key],
			})
		}
	}
	return out, nil
}

// loadPackages loads patterns with golang.org/x/tools/go/packages, in the
// mode both Extract's own field walk and findDerives need (types, syntax,
// and cross-package type info, so a WithDerive call site in one package can
// be matched against a T declared in another one - see
// TestFindDerivesCrossPackage), and returns them sorted by package path for
// deterministic output.
//
// It never sets packages.Config.Tests. explain/schema/policy/doctor
// --compare all describe the shipping config surface, not the test surface,
// so a source: tag or a WithDerive call living only in a _test.go file is
// invisible to every command built on Extract.
func loadPackages(patterns []string) ([]*packages.Package, error) {
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedTypes | packages.NeedSyntax |
			packages.NeedTypesInfo | packages.NeedDeps | packages.NeedImports,
	}
	pkgs, err := packages.Load(cfg, patterns...)
	if err != nil {
		return nil, fmt.Errorf("mamori: loading packages %v: %w", patterns, err)
	}
	if n := packages.PrintErrors(pkgs); n > 0 {
		return nil, fmt.Errorf("mamori: %d error(s) loading packages %v", n, patterns)
	}

	sorted := make([]*packages.Package, len(pkgs))
	copy(sorted, pkgs)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].PkgPath < sorted[j].PkgPath })
	return sorted, nil
}

// namedStruct is a package-level struct type found while scanning a
// package's syntax trees, along with its source position so callers can
// recover source declaration order (types.Package.Scope().Names() is
// alphabetical, not declaration order, hence walking the AST instead).
type namedStruct struct {
	name string
	typ  *types.Struct
	file string // the declaring file, for a stable cross-file order
	pos  token.Pos
}

// taggedStructs returns every package-level struct type in pkg that has at
// least one field carrying a `source` struct tag, in source declaration
// order.
func taggedStructs(pkg *packages.Package) []namedStruct {
	var found []namedStruct
	seen := make(map[string]bool)
	for _, file := range pkg.Syntax {
		ast.Inspect(file, func(n ast.Node) bool {
			ts, ok := n.(*ast.TypeSpec)
			if !ok {
				return true
			}
			if _, ok := ts.Type.(*ast.StructType); !ok {
				return true
			}
			obj, ok := pkg.TypesInfo.Defs[ts.Name]
			if !ok || obj == nil {
				return true
			}
			named, ok := obj.Type().(*types.Named)
			if !ok {
				return true
			}
			st, ok := named.Underlying().(*types.Struct)
			if !ok {
				return true
			}
			name := named.Obj().Name()
			if seen[name] || !hasSourceTaggedField(st) {
				return true
			}
			seen[name] = true
			found = append(found, namedStruct{
				name: name,
				typ:  st,
				file: pkg.Fset.Position(ts.Pos()).Filename,
				pos:  ts.Pos(),
			})
			return true
		})
	}
	// Sort by (file, pos), not by pos alone. A token.Pos is only meaningful
	// within one file: across the files of a package its value depends on the
	// order they were added to the shared FileSet, which packages.Load does
	// concurrently. Sorting on pos alone therefore let a package whose structs
	// span several files report them in a different order from run to run,
	// breaking the deterministic order Extract's own doc comment promises and
	// making "mamori diff" see changes between two runs of an unchanged tree.
	// Filename first keeps declaration order inside each file, which is what a
	// reader expects, and is identical to the old behavior for the
	// single-file packages that are the common case.
	sort.Slice(found, func(i, j int) bool {
		if found[i].file != found[j].file {
			return found[i].file < found[j].file
		}
		return found[i].pos < found[j].pos
	})
	return found
}

// hasSourceTaggedField reports whether any exported field of st carries a
// `source` struct tag (the same presence test decode.go uses: Lookup, so an
// empty-valued source:"" tag still counts as present).
func hasSourceTaggedField(st *types.Struct) bool {
	for i := 0; i < st.NumFields(); i++ {
		f := st.Field(i)
		if !f.Exported() {
			continue
		}
		if _, ok := reflect.StructTag(st.Tag(i)).Lookup("source"); ok {
			return true
		}
	}
	return false
}

// walkFields walks the exported fields of st and returns the leaf Fields,
// mirroring core's decode.go walkSpecs: a field with a `source` tag is
// always a leaf (never recursed into, matching decode.go's isLeafStruct &&
// !hasSource guard); a source-less field whose type is a struct (other than
// secret.String/secret.Bytes) is a container and is recursed into,
// contributing dotted paths under prefix; any other source-less field is
// skipped.
//
// firstSensitive reports the first secret-bearing scheme in a source tag; it
// is threaded down from Extract so a caller-extended scheme set (see
// --secret-schemes) reaches nested fields too.
func walkFields(pkg *types.Package, st *types.Struct, prefix string, firstSensitive func(string) (string, bool)) []Field {
	var fields []Field
	for i := 0; i < st.NumFields(); i++ {
		v := st.Field(i)
		if !v.Exported() {
			continue
		}
		path := v.Name()
		if prefix != "" {
			path = prefix + "." + v.Name()
		}

		tag := reflect.StructTag(st.Tag(i))
		source, hasSource := tag.Lookup("source")
		def, hasDefault := tag.Lookup("default")
		optional := tag.Get("optional") == "true"
		onFail := tag.Get("onfail")
		validate := tag.Get("validate")

		sensitiveType := isSecretType(v.Type())

		switch {
		case hasSource:
			refs := sourcetag.SplitChain(source)
			_, schemeSensitive := firstSensitive(source)
			fields = append(fields, Field{
				Path:       path,
				Kind:       KindSource,
				GoType:     types.TypeString(v.Type(), shortQualifier(pkg)),
				Source:     source,
				Refs:       refs,
				Default:    def,
				HasDefault: hasDefault,
				Optional:   optional,
				Sensitive:  sensitiveType || schemeSensitive,
				OnFail:     onFail,
				Validate:   validate,
			})
		default:
			// Recurse BEFORE considering a validate-only leaf. A nested struct
			// commonly carries its own `validate:"required"`, and treating that
			// as a leaf here would drop every source-tagged field inside it.
			if nested, ok := v.Type().Underlying().(*types.Struct); ok && !sensitiveType {
				fields = append(fields, walkFields(pkg, nested, path, firstSensitive)...)
				continue
			}
			if validate != "" {
				// mamori validates the whole struct, so this rule is enforced at
				// runtime even with no source tag. schema would otherwise
				// describe less than mamori enforces.
				fields = append(fields, Field{
					Path:       path,
					Kind:       KindValidate,
					GoType:     types.TypeString(v.Type(), shortQualifier(pkg)),
					Default:    def,
					HasDefault: hasDefault,
					Optional:   optional,
					Sensitive:  sensitiveType,
					Validate:   validate,
				})
			}
			// Neither sourced, nor a container, nor validated: skip, exactly as
			// before (decode.go leaves it to its zero value).
		}
	}
	return fields
}

// shortQualifier returns a types.Qualifier that renders a field's own
// package unqualified (as decode.go's reflect.Type.String() would for a
// same-package type) and every other package by its short name (e.g.
// "secret.String"), not the fully qualified import path that
// types.RelativeTo itself would use: an import path is precise but far
// noisier than useful in a table meant for a human to scan.
func shortQualifier(home *types.Package) types.Qualifier {
	return func(other *types.Package) string {
		if other == home {
			return ""
		}
		return other.Name()
	}
}

// isSecretType reports whether t is secret.String or secret.Bytes: a
// *types.Named whose package path ends in "/secret" and whose name is
// String or Bytes. This is the static-analysis equivalent of decode.go's
// f.Type == secretStringType || f.Type == secretBytesType comparison, which
// compares reflect.Type values the CLI cannot obtain (it never imports or
// runs the config struct's package, only reads its syntax and types).
func isSecretType(t types.Type) bool {
	named, ok := t.(*types.Named)
	if !ok {
		return false
	}
	obj := named.Obj()
	pkg := obj.Pkg()
	if pkg == nil || !strings.HasSuffix(pkg.Path(), "/secret") {
		return false
	}
	switch obj.Name() {
	case "String", "Bytes":
		return true
	default:
		return false
	}
}
