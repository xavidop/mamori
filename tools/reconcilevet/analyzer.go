// Package reconcilevet defines a go/analysis Analyzer that flags struct fields
// which pull a secret-bearing source but store it in a plain, unprotected Go
// type (string or []byte) instead of the redacting secret.String / secret.Bytes
// wrapper types from github.com/xavidop/mamori/secret.
package reconcilevet

import (
	"fmt"
	"go/ast"
	"go/types"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/inspector"
)

// secretBearingSchemes is the set of source schemes that resolve to secret
// material. A field wired to any of these should hold its value in a redacting
// secret type so the plaintext cannot leak through logs, fmt, or JSON.
//
// It mirrors the scheme tokens used by mamori's secret-manager providers.
var secretBearingSchemes = map[string]struct{}{
	"aws-sm":     {}, // AWS Secrets Manager
	"gcp-sm":     {}, // Google Cloud Secret Manager
	"azure-kv":   {}, // Azure Key Vault
	"vault":      {}, // HashiCorp Vault
	"op":         {}, // 1Password
	"sops":       {}, // Mozilla SOPS
	"k8s-secret": {}, // Kubernetes Secret
}

// Analyzer is the reconcilevet analyzer. It reports any struct field that binds
// a secret-bearing source (via its `source:"..."` tag) to a plain string or
// []byte, and suggests using secret.String / secret.Bytes instead.
var Analyzer = &analysis.Analyzer{
	Name: "reconcilevet",
	Doc: "reports struct fields that pull a secret-bearing source (aws-sm, gcp-sm, " +
		"azure-kv, vault, op, sops, k8s-secret) into a plain string or []byte " +
		"instead of the redacting secret.String / secret.Bytes types",
	Requires: []*analysis.Analyzer{inspect.Analyzer},
	Run:      run,
}

func run(pass *analysis.Pass) (any, error) {
	insp := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)

	nodeFilter := []ast.Node{(*ast.StructType)(nil)}
	insp.Preorder(nodeFilter, func(n ast.Node) {
		st := n.(*ast.StructType)
		if st.Fields == nil {
			return
		}
		for _, field := range st.Fields.List {
			checkField(pass, field)
		}
	})

	return nil, nil
}

// checkField reports a diagnostic if field has a secret-bearing source tag but
// a plain string / []byte type.
func checkField(pass *analysis.Pass, field *ast.Field) {
	if field.Tag == nil {
		return
	}

	// field.Tag.Value is the raw literal, including its surrounding quotes or
	// backticks; unquote it to get the tag contents.
	tagContents, err := strconv.Unquote(field.Tag.Value)
	if err != nil {
		return
	}

	source, ok := reflect.StructTag(tagContents).Lookup("source")
	if !ok {
		return
	}

	// A source tag may hold a comma-separated precedence chain, e.g.
	// "env:X,aws-sm://secret" (mamori's ParseRefs / spec 10.2). Check every
	// ref in the chain, not just the first, so a sensitive ref in a later
	// chain position is not silently missed - see firstSensitiveScheme.
	scheme, hasSensitive := firstSensitiveScheme(source)
	if !hasSensitive {
		return
	}

	fieldType := pass.TypesInfo.TypeOf(field.Type)
	if fieldType == nil {
		return
	}

	kind, isPlain := plainSecretKind(fieldType)
	if !isPlain {
		// Already a secret.String / secret.Bytes (or some other named type) - 		// nothing to flag.
		return
	}

	pass.Reportf(field.Pos(),
		"field %s has a secret-bearing source scheme %q but stores it in a plain %s; "+
			"use secret.String or secret.Bytes to keep the value redacted",
		fieldName(field), scheme, kind)
}

// firstSensitiveScheme splits source into its chain of refs (the same
// scheme-comma split mamori's ParseRefs applies, via splitSourceChain) and
// returns the scheme of the first ref that uses a secret-bearing scheme. ok
// is false when no ref in the chain is secret-bearing, including the common
// case of an unchained, non-sensitive tag.
func firstSensitiveScheme(source string) (scheme string, ok bool) {
	for _, part := range splitSourceChain(strings.TrimSpace(source)) {
		// mamori parses the scheme as the text before the first ':' (see
		// ref.go / ParseRef). We only need the scheme, so replicate that
		// single Cut.
		s, _, hasColon := strings.Cut(strings.TrimSpace(part), ":")
		if !hasColon || s == "" {
			continue
		}
		if _, secret := secretBearingSchemes[s]; secret {
			return s, true
		}
	}
	return "", false
}

// chainSchemeStart matches a scheme-like token at the start of a string,
// e.g. "aws-sm:" or "env:". It is a duplicate of mamori's unexported
// ref.go schemeStart regexp, kept in sync by hand: reconcilevet is its own
// module and does not depend on the mamori core module (it matches the
// secret.String / secret.Bytes wrapper types structurally instead, see
// README "Development"), so it cannot call splitChain or ParseRefs
// directly. A later CLI plan is expected to extract shared tag-parsing into
// a package both modules can depend on, which should replace this
// duplication.
var chainSchemeStart = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.\-]*:`)

// splitSourceChain splits a source tag on commas that begin a new
// scheme-prefixed ref, leaving every other comma (inside a query string or
// an opaque path such as exec:echo a,b) untouched. It replicates mamori's
// ref.go splitChain rule so the analyzer's notion of "the refs in this tag"
// matches what ParseRefs actually parses at runtime; see chainSchemeStart
// for why this is a duplicate rather than a call to mamori itself.
func splitSourceChain(tag string) []string {
	var parts []string
	start := 0
	for i := 0; i < len(tag); i++ {
		if tag[i] != ',' {
			continue
		}
		if chainSchemeStart.MatchString(tag[i+1:]) {
			parts = append(parts, tag[start:i])
			start = i + 1
		}
	}
	parts = append(parts, tag[start:])
	return parts
}

// plainSecretKind reports whether t is an unprotected plain string or []byte.
// secret.String and secret.Bytes are named struct types, so they never match
// here - that is exactly how the good fields are distinguished from the bad.
func plainSecretKind(t types.Type) (kind string, ok bool) {
	switch u := t.(type) {
	case *types.Basic:
		if u.Kind() == types.String {
			return "string", true
		}
	case *types.Slice:
		if b, isBasic := u.Elem().(*types.Basic); isBasic && b.Kind() == types.Byte {
			return "[]byte", true
		}
	}
	return "", false
}

// fieldName returns a human-readable identifier for the field for diagnostics.
func fieldName(field *ast.Field) string {
	if len(field.Names) == 0 {
		// Embedded field: describe by its type.
		return fmt.Sprintf("(embedded %s)", types.ExprString(field.Type))
	}
	names := make([]string, 0, len(field.Names))
	for _, n := range field.Names {
		names = append(names, strconv.Quote(n.Name))
	}
	return strings.Join(names, ", ")
}
