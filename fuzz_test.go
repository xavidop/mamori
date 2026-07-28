package mamori

import (
	"testing"
)

// FuzzRefGrammar exercises the hand-rolled parsing across the whole ref
// grammar as one pipeline, in the same order Load applies it: ${VAR}
// interpolation, the chain-and-precedence split, per-ref parsing (query
// first, then fragment), the ?decode= coding pipeline, and SelectKey's
// literal-key/RFC 6901 JSON Pointer fragment selection.
//
// Every function here is a hand-written scanner rather than something built
// on net/url or a grammar library, precisely because the grammar puts #key
// before ?opts, the reverse of a standard URL. That means none of the usual
// guarantees a well-reviewed parsing library would provide - bounded
// recursion, no out-of-bounds slicing, no silent misclassification of a
// malformed value as valid - are available for free here. So this asserts
// only the two properties that MUST hold for any input regardless of what the
// grammar itself says is well-formed, because nothing else is safe to assume
// about hand-rolled string scanning that was written expecting well-formed
// tags, not fuzzer-generated ones:
//
//  1. It must not panic. A malformed ref must fail closed with an error, not
//     crash the process resolving a config field.
//  2. It must never return a value together with a non-nil error. Every
//     caller of these functions (resolve.go, the builtin providers, Load
//     itself) follows the Go convention of only trusting a result when err is
//     nil; a violation here would be a latent bug at every call site, not
//     just in this package.
func FuzzRefGrammar(f *testing.F) {
	seeds := []string{
		"env:LOG_LEVEL",
		"aws-sm://prod/db#password",
		"aws-sm://prod/db#/credentials/password",
		"aws-sm://prod/db#/replicas/5/creds/password",
		"aws-sm://prod/db#/a~1b",
		"aws-sm://prod/db#ca.crt?decode=base64",
		"s3://bucket/blob?decode=base64,gzip",
		"env:PORT,aws-ps://svc/port",
		"aws-sm://${ENV}/db#password",
		"exec:echo a, b",
		"",
		"://",
		"#",
		"?",
		"a:b#/~",
		// ${VAR} expansion edge cases: unterminated brace, empty name,
		// escaped '$', and an undefined variable - all documented error
		// paths in expandRefVars.
		"a:b${",
		"a:b${}",
		"a:b$$",
		"a:b${NOPE}",
		// decode pipeline edge cases: empty spec, unknown coding, trailing
		// comma, and whitespace around a coding name.
		"a:b?decode=",
		"a:b?decode=bogus",
		"a:b?decode=base64,",
		"a:b?decode= base64 ",
		// JSON Pointer edge cases: index 0, the RFC 6901 "-" token, a
		// leading zero, and a dangling escape.
		"a:b#/0",
		"a:b#/-",
		"a:b#/01",
		"a:b#/a~",
		// a malformed query, to exercise net/url's error path inside ParseRef.
		"a:b?%zz",
	}
	for _, s := range seeds {
		f.Add(s, []byte(`{"a":{"b":[1,2,"three"]}}`))
	}
	// The pairings above all cross a fixed shallow payload with every tag, so
	// a pointer fragment deep enough to need multiple descend() calls, or one
	// that walks into an array-rooted document, is left entirely to chance
	// mutation to discover. Seed those pairings directly instead.
	f.Add("aws-sm://prod/db#/a/b/c/d/e", []byte(`{"a":{"b":{"c":{"d":{"e":"deep"}}}}}`))
	f.Add("aws-sm://prod/db#/2/x", []byte(`[{"x":1},{"x":2},{"x":30}]`))

	f.Fuzz(func(t *testing.T, tag string, payload []byte) {
		expanded, err := expandRefVars(tag, map[string]string{"ENV": "prod"})
		if err != nil {
			if expanded != "" {
				t.Fatalf("expandRefVars(%q) returned both %q and an error %v", tag, expanded, err)
			}
			return
		}

		refs, err := ParseRefs(expanded)
		if err != nil {
			if refs != nil {
				t.Fatalf("ParseRefs(%q) returned both refs and an error %v", expanded, err)
			}
			return
		}

		for _, r := range refs {
			steps, err := parseDecodePipeline(r.Opt("decode"))
			if err != nil {
				if steps != nil {
					t.Fatalf("parseDecodePipeline(%q) returned both steps and an error %v", r.Opt("decode"), err)
				}
				continue
			}

			got, err := SelectKey(payload, r.Key)
			if err != nil {
				if got != nil {
					t.Fatalf("SelectKey(%q) returned both bytes and an error %v", r.Key, err)
				}
			}

			// A ref that parsed must render back to something parseable from
			// its parts alone: Scheme/Path/Key/Opts.Encode(), not from Raw.
			// Ref.String() short-circuits to Raw whenever it is set, and
			// ParseRef always sets Raw to its own input, so checking
			// ParseRef(r.String()) directly would just re-parse the exact
			// same string that already parsed - a check that can never fail.
			// Zeroing Raw on this local copy (production Ref is untouched)
			// forces String() down its reconstruction branch, so this
			// actually exercises Opts.Encode() round-tripping through
			// url.Values and back.
			r2 := r
			r2.Raw = ""
			if _, err := ParseRef(r2.String()); err != nil {
				t.Fatalf("Ref.String() %q does not re-parse: %v", r2.String(), err)
			}
		}
	})
}
