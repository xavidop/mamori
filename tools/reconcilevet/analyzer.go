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

	// mamori parses the scheme as the text before the first ':' (see ref.go /
	// ParseRef). We only need the scheme, so replicate that single Cut.
	scheme, _, hasColon := strings.Cut(strings.TrimSpace(source), ":")
	if !hasColon || scheme == "" {
		return
	}
	if _, secret := secretBearingSchemes[scheme]; !secret {
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
