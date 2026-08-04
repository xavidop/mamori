// Package sourcetag provides stdlib-only parsing helpers for mamori's
// `source:"..."` struct tags, shared by the cmd/mamori CLI commands and the
// go vet analyzer behind `mamori vet` (../vetcheck). Being stdlib-only keeps
// the analyzer free of the mamori core and every provider module, which a go
// vet tool must be. It replicates the chain-split and scheme rules from
// mamori's ref.go (splitChain / ParseRef) closely enough that both consumers
// agree on what "the refs in this tag" means, without depending on the core.
package sourcetag

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// defaultSecretSchemes lists the source schemes that resolve to secret
// material, mirroring the scheme tokens of mamori's secret-manager providers.
//
// This set is a static approximation, and it has to be. A provider declares
// its values secret at resolve time, by returning Value{Sensitive: true}
// (see providers/aws/sm.go, providers/vault/vault.go, and friends), which a
// static analyzer can never observe: go vet does not run the program, and the
// CLI deliberately does not import provider modules so that no cloud SDK ends
// up in its dependency graph. So the scheme-to-secret mapping is declared
// here rather than derived.
//
// The list is deliberately inclusive. Some schemes below carry secrets only
// sometimes (noted inline), and they are still listed: a false positive costs
// you a secret.String on a value that did not need one, while a miss costs a
// leaked credential. Erring toward the cheap mistake is the right trade for a
// security check. Anything genuinely non-secret can stay a plain string by
// using a config-style scheme (env, file, consul, k8s-cm, and the like).
//
// Two consequences worth knowing. Keep this list in step with any new
// secret-bearing provider that ships. And because a custom provider's scheme
// cannot appear here, callers may extend the set at run time: see
// SchemeSet.Add and the `--secret-schemes` flag on vet and explain.
var defaultSecretSchemes = []string{
	// Dedicated secret managers: every value they resolve is secret.
	"aws-sm",     // AWS Secrets Manager
	"gcp-sm",     // Google Cloud Secret Manager
	"azure-kv",   // Azure Key Vault
	"vault",      // HashiCorp Vault
	"op",         // 1Password
	"sops",       // Mozilla SOPS
	"doppler",    // Doppler
	"hcp-vs",     // HCP Vault Secrets (distinct from self-hosted vault above)
	"k8s-secret", // Kubernetes Secret (k8s-cm, a ConfigMap, is not secret)

	// Not a secret manager, but every value it resolves is marked sensitive
	// and this list is about what a field HOLDS, not what the backend calls
	// itself. Heroku hands back an app's whole config var namespace with no
	// per-var classification, and add-ons write DATABASE_URL and REDIS_URL,
	// live credentials, into it.
	"heroku", // Heroku config vars

	// Conditionally secret, listed for the reason given above.
	"aws-ps", // SSM Parameter Store: only SecureString params are secret
	"exec",   // core marks all exec output Sensitive (builtin_exec.go)
	"mamori", // the client relays whatever a mamori server marks sensitive
}

// SchemeSet is a set of source schemes treated as secret-bearing. A field
// wired to any of them should hold its value in a redacting secret type
// (secret.String / secret.Bytes) so the plaintext cannot leak through logs,
// fmt, or JSON.
type SchemeSet map[string]struct{}

// DefaultSecretSchemes returns a new set holding mamori's built-in
// secret-bearing schemes. Each call returns a fresh map, so a caller that
// extends it (see Add) cannot disturb another caller's set.
func DefaultSecretSchemes() SchemeSet {
	s := make(SchemeSet, len(defaultSecretSchemes))
	s.Add(defaultSecretSchemes...)
	return s
}

// Add inserts schemes into the set. Entries are matched exactly, the same way
// SchemeOf reads a scheme off a ref, so they must be bare scheme tokens
// ("mysecrets"), not full refs ("mysecrets://x"); use ParseSchemeList to
// validate untrusted input before calling Add.
func (s SchemeSet) Add(schemes ...string) {
	for _, scheme := range schemes {
		if scheme == "" {
			continue
		}
		s[scheme] = struct{}{}
	}
}

// Contains reports whether scheme is in the set.
func (s SchemeSet) Contains(scheme string) bool {
	_, ok := s[scheme]
	return ok
}

// Sorted returns the set's schemes in lexical order, for stable help text and
// diagnostics.
func (s SchemeSet) Sorted() []string {
	out := make([]string, 0, len(s))
	for scheme := range s {
		out = append(out, scheme)
	}
	sort.Strings(out)
	return out
}

// schemeToken matches a bare scheme token: the same character class as
// chainSchemeStart, without the trailing colon.
var schemeToken = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.\-]*$`)

// ParseSchemeList splits a comma-separated list of scheme tokens (the form
// taken by `mamori vet --secret-schemes`), trimming spaces and dropping empty
// entries. It rejects anything that is not a bare scheme token, so a mistyped
// value such as "mysecrets://prod" is reported rather than silently ignored:
// a security check that quietly covers less than the caller asked for is
// worse than one that fails loudly.
func ParseSchemeList(list string) ([]string, error) {
	var out []string
	for _, part := range strings.Split(list, ",") {
		scheme := strings.TrimSpace(part)
		if scheme == "" {
			continue
		}
		if !schemeToken.MatchString(scheme) {
			return nil, fmt.Errorf("invalid scheme %q: want a bare scheme token such as \"mysecrets\", not a full ref", scheme)
		}
		out = append(out, scheme)
	}
	return out, nil
}

// chainSchemeStart matches a scheme-like token at the start of a string,
// e.g. "aws-sm:" or "env:". It mirrors mamori's unexported ref.go
// schemeStart regexp, kept in sync by hand: this package cannot import the
// mamori core module directly (that would defeat the purpose of keeping the
// mamori vet analyzer core-free), so it replicates the rule instead.
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

// FirstSensitiveScheme splits tag into its chain of refs (via SplitChain) and
// returns the scheme of the first ref whose scheme is in the set. ok is false
// when no ref in the chain is secret-bearing, including the common case of an
// unchained, non-sensitive tag.
func (s SchemeSet) FirstSensitiveScheme(tag string) (scheme string, ok bool) {
	for _, part := range SplitChain(strings.TrimSpace(tag)) {
		candidate, hasScheme := SchemeOf(part)
		if !hasScheme {
			continue
		}
		if s.Contains(candidate) {
			return candidate, true
		}
	}
	return "", false
}

// defaultSet backs the package-level helpers below. It is built once so the
// per-field callers (explain, policy) do not rebuild it for every field, and
// is never mutated; callers that extend the set build their own with
// DefaultSecretSchemes.
var defaultSet = DefaultSecretSchemes()

// FirstSensitiveScheme reports the first secret-bearing scheme in tag using
// mamori's built-in scheme set. Callers that honour a user-extended set (the
// vet analyzer) build their own SchemeSet and call the method instead.
func FirstSensitiveScheme(tag string) (scheme string, ok bool) {
	return defaultSet.FirstSensitiveScheme(tag)
}

// IsSecretBearingScheme reports whether scheme is one of mamori's built-in
// secret-bearing schemes.
func IsSecretBearingScheme(scheme string) bool {
	return defaultSet.Contains(scheme)
}
