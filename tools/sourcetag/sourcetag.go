// Package sourcetag provides stdlib-only parsing helpers for mamori's
// `source:"..."` struct tags, shared between tools that cannot depend on the
// mamori core module (such as tools/reconcilevet, a go vet analyzer that
// stays core-free by design) and tools that can (such as the cmd/mamori
// CLI). It replicates the chain-split and scheme rules from mamori's ref.go
// (splitChain / ParseRef) closely enough that both consumers agree on what
// "the refs in this tag" means, without either depending on the other.
package sourcetag

import (
	"regexp"
	"strings"
)

// SecretBearingSchemes is the set of source schemes that resolve to secret
// material. A field wired to any of these should hold its value in a
// redacting secret type (secret.String / secret.Bytes) so the plaintext
// cannot leak through logs, fmt, or JSON.
//
// It mirrors the scheme tokens used by mamori's secret-manager providers.
var SecretBearingSchemes = map[string]struct{}{
	"aws-sm":     {}, // AWS Secrets Manager
	"gcp-sm":     {}, // Google Cloud Secret Manager
	"azure-kv":   {}, // Azure Key Vault
	"vault":      {}, // HashiCorp Vault
	"op":         {}, // 1Password
	"sops":       {}, // Mozilla SOPS
	"k8s-secret": {}, // Kubernetes Secret
}

// IsSecretBearingScheme reports whether scheme is one of SecretBearingSchemes.
func IsSecretBearingScheme(scheme string) bool {
	_, ok := SecretBearingSchemes[scheme]
	return ok
}

// chainSchemeStart matches a scheme-like token at the start of a string,
// e.g. "aws-sm:" or "env:". It mirrors mamori's unexported ref.go
// schemeStart regexp, kept in sync by hand: this package cannot import the
// mamori core module directly (that would defeat the purpose of keeping
// consumers like reconcilevet core-free), so it replicates the rule instead.
var chainSchemeStart = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.\-]*:`)

// SplitChain splits a source tag on commas that begin a new scheme-prefixed
// ref, leaving every other comma (inside a query string or an opaque path
// such as exec:echo a,b) untouched. It replicates mamori's ref.go splitChain
// rule so callers' notion of "the refs in this tag" matches what ParseRefs
// actually parses at runtime; see chainSchemeStart for why this is a
// duplicate rather than a call to mamori itself.
func SplitChain(tag string) []string {
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

// SchemeOf returns the scheme of ref: the text before the first ':', after
// trimming surrounding whitespace. ok is false when ref has no scheme
// (no colon, or an empty scheme before the colon). mamori parses the scheme
// the same way (see ref.go / ParseRef); this replicates that single Cut.
func SchemeOf(ref string) (scheme string, ok bool) {
	s, _, hasColon := strings.Cut(strings.TrimSpace(ref), ":")
	if !hasColon || s == "" {
		return "", false
	}
	return s, true
}

// FirstSensitiveScheme splits tag into its chain of refs (via SplitChain)
// and returns the scheme of the first ref that uses a secret-bearing
// scheme. ok is false when no ref in the chain is secret-bearing, including
// the common case of an unchained, non-sensitive tag.
func FirstSensitiveScheme(tag string) (scheme string, ok bool) {
	for _, part := range SplitChain(strings.TrimSpace(tag)) {
		s, hasScheme := SchemeOf(part)
		if !hasScheme {
			continue
		}
		if IsSecretBearingScheme(s) {
			return s, true
		}
	}
	return "", false
}
