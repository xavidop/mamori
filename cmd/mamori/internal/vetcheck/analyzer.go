// Package vetcheck defines the go/analysis Analyzer behind `mamori vet`. It
// flags struct fields that pull a secret-bearing source (via their
// `source:"..."` tag) but store it in a plain, unprotected Go type (string or
// []byte) instead of the redacting secret.String / secret.Bytes wrapper types
// from github.com/xavidop/mamori/secret.
//
// It lives inside the mamori CLI module: cmd/mamori exposes it both as the
// `mamori vet` subcommand and, via the go vet tool protocol, as
// `go vet -vettool=$(which mamori)`. See ../../vet.go and ../../main.go.
package vetcheck

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

	"github.com/xavidop/mamori/cmd/mamori/internal/sourcetag"
)

// Analyzer reports two things: any struct field that binds a secret-bearing
// source (via its `source:"..."` tag) to a plain string or []byte, and any
// WithDerive hook that reveals such a secret and launders the plaintext into
// a plain string or []byte write path. Both suggest using secret.String /
// secret.Bytes instead. Its name is what appears when the CLI runs as a go
// vet tool.
var Analyzer = &analysis.Analyzer{
	Name: "mamorivet",
	// The scheme list is rendered from the set itself so help text cannot
	// drift from what the analyzer actually flags.
	Doc: "reports struct fields that pull a secret-bearing source (" +
		strings.Join(sourcetag.DefaultSecretSchemes().Sorted(), ", ") +
		") into a plain string or []byte instead of the redacting " +
		"secret.String / secret.Bytes types, and WithDerive hooks that " +
		"reveal such a secret and write the plaintext into a plain string " +
		"or []byte field instead",
	Requires: []*analysis.Analyzer{inspect.Analyzer},
	Run:      run,
}

// extraSchemes holds the -secret-schemes flag: a comma-separated list of
// additional secret-bearing schemes, added to the built-in set. It exists
// because the built-in set cannot cover a scheme mamori does not ship (see
// sourcetag.defaultSecretSchemes for why the set is static), which would
// otherwise let a custom provider's secrets sit in a plain string unflagged.
//
// Registering it on Analyzer.Flags is what makes it work in both of the CLI's
// modes from one declaration: unitchecker exposes it as
// -mamorivet.secret-schemes under `go vet -vettool`, and the `mamori vet`
// subcommand sets it through the same FlagSet (see ../../vet.go).
var extraSchemes string

func init() {
	Analyzer.Flags.StringVar(&extraSchemes, "secret-schemes", "",
		"comma-separated extra source schemes to treat as secret-bearing, added to the built-in set")
}

// secretSchemes builds the scheme set for one run: the built-in set plus
// anything the -secret-schemes flag added. It is rebuilt per pass rather than
// cached so that the flag can be set between runs (analysistest does exactly
// that), which costs a handful of map inserts per package.
func secretSchemes() (sourcetag.SchemeSet, error) {
	set := sourcetag.DefaultSecretSchemes()
	extra, err := sourcetag.ParseSchemeList(extraSchemes)
	if err != nil {
		return nil, fmt.Errorf("-secret-schemes: %w", err)
	}
	set.Add(extra...)
	return set, nil
}

func run(pass *analysis.Pass) (any, error) {
	insp := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)

	schemes, err := secretSchemes()
	if err != nil {
		return nil, err
	}

	nodeFilter := []ast.Node{(*ast.StructType)(nil), (*ast.CallExpr)(nil)}
	insp.Preorder(nodeFilter, func(n ast.Node) {
		switch node := n.(type) {
		case *ast.StructType:
			if node.Fields == nil {
				return
			}
			for _, field := range node.Fields.List {
				checkField(pass, field, schemes)
			}
		case *ast.CallExpr:
			checkDerives(pass, node)
		}
	})

	return nil, nil
}

// checkField reports a diagnostic if field has a source tag naming a scheme in
// schemes but a plain string / []byte type.
func checkField(pass *analysis.Pass, field *ast.Field, schemes sourcetag.SchemeSet) {
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
	// chain position is not silently missed - see FirstSensitiveScheme.
	scheme, hasSensitive := schemes.FirstSensitiveScheme(source)
	if !hasSensitive {
		return
	}

	fieldType := pass.TypesInfo.TypeOf(field.Type)
	if fieldType == nil {
		return
	}

	kind, isPlain := plainSecretKind(fieldType)
	if !isPlain {
		// Already a secret.String / secret.Bytes (or some other named type) -
		// nothing to flag.
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
