# Ref Grammar and Value Pipeline Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add three resolve-time capabilities to mamori's `source` tag grammar (RFC 6901 nested key selection, `?decode=` value transforms, and `${VAR}` interpolation) without changing the meaning of any existing ref or the `Provider` SPI.

**Architecture:** All three are additive changes inside the core module. Pointer selection extends the existing shared `SelectKey` helper, so all 26 provider files that call it inherit it for free. Decoding is a core-owned step applied at the three points where a `Value` enters the engine. Interpolation runs once at spec-walk time, before refs are parsed.

**Tech Stack:** Go 1.26, standard library only for the new code (`encoding/json`, `encoding/base64`, `encoding/hex`, `compress/gzip`, `strings`, `strconv`, `os`). Tests use the standard `testing` package, table-driven, with `go.uber.org/goleak` already wired into the suite.

**Spec:** `docs/superpowers/specs/2026-07-27-ref-grammar-design.md`

## Global Constraints

- **No new dependencies in the core module.** Core's `go.mod` requires exactly `fsnotify`, `validator/v10`, `mapstructure/v2`, `goleak`, `golang.org/x/sys`, and `yaml.v3`. Every coding in this plan is stdlib. Do not add one that is not.
- **No existing ref may change meaning.** A `#` fragment not beginning with `/` stays a literal top-level key, forever. The test that guards this is Task 1 Step 1.
- **No change to `Provider`, `WatchableProvider`, or `BatchProvider`.** No provider module is edited by this plan.
- **Error sentinels are wrapped with `%w`, never formatted with `%v`.** Use the two-verb form `fmt.Errorf("...: %w: %w", ErrInvalid, err)` when wrapping both a sentinel and an underlying error, so `errors.Is` reaches the sentinel and `errors.As` reaches the cause.
- **Only `ErrNotFound` may trigger `default:` / `optional`.** Anything that is a structural mismatch rather than an absence must wrap `ErrInvalid`.
- **Conventional Commits.** `feat:` for the three features, `docs:` for documentation-only commits, `test:` for test-only commits. The changelog is generated from these and `docs:`/`test:`/`chore:` do not trigger a release.
- **Every feature ships with its documentation in the same PR.** Not a follow-up.
- **Branches and PRs are managed exclusively through the `gh stack` CLI.** Never
  `git checkout -b` a stack layer and never `gh pr create`. See "Delivery" below.
- **Run `go test -race ./...` from the repo root before every commit.** The repo uses `goleak`; a leaked goroutine fails the suite.

## Delivery: the gh stack

Use the `gh stack` extension (`github/gh-stack`, already installed). **Do not
create branches with `git checkout -b` or open PRs with `gh pr create --base`.**
The extension tracks the stack's topology itself and manages every PR's base
branch; hand-rolled branches and PRs are not tracked and will be rebased wrong.

Three PRs stacked on `xavier/new-features-2`, which already holds the spec
commit (`24fbb14`) as PR 1:

| PR | Branch | Tasks |
|---|---|---|
| 1 | `xavier/new-features-2` | the spec, already committed |
| 2 | `xavier/ref-grammar-pointer` | 1, 2, 3 |
| 3 | `xavier/ref-grammar-decode` | 4, 5, 6, 7, 8 |
| 4 | `xavier/ref-grammar-interpolation` | 9, 10, 11 |

**Before Task 1**, adopt the existing bottom branch into a new stack:

```bash
git checkout xavier/new-features-2
gh stack init xavier/new-features-2
gh stack view
```

`gh stack init` adopts an existing branch rather than erroring, and bases it on
the default branch (`main`).

Then, per layer:

- `gh stack add <branch>` creates the next branch **on top of the current stack
  tip** and checks it out. Called with no `-m`, `-A`, or `-u` flag it only
  creates the branch; it does not commit.
- Commit inside that layer with ordinary `git commit`.
- `gh stack submit` pushes every branch and creates or updates every PR,
  setting each one's base to the branch below it. It is idempotent, so running
  it after each layer publishes that layer and leaves the earlier ones updated
  rather than duplicated.

`gh stack submit` opens an interactive editor by default. In a non-interactive
session pass `--auto`, which uses generated titles and creates PRs as drafts
unless `--open` is also passed.

Useful during execution: `gh stack view` (or `--short`) to see the topology and
PR status, `gh stack checkout <branch>` to move between layers, and
`gh stack sync` after anything lands upstream.

## File Structure

| File | Status | Responsibility |
|---|---|---|
| `jsonpointer.go` | **Create** | RFC 6901 pointer parsing and resolution against a JSON document. Pure, no mamori types beyond the error sentinels. |
| `jsonpointer_test.go` | **Create** | Pointer unit tests, including the RFC 6901 example document. |
| `helpers.go` | Modify | `SelectKey` gains the one-line dispatch to the pointer path. |
| `helpers_test.go` | Modify | Backward-compatibility assertions for literal dotted keys. |
| `decodeopt.go` | **Create** | The `?decode=` coding registry, pipeline parsing, and `applyDecode`. |
| `decodeopt_test.go` | **Create** | Per-coding round-trip and failure tests. |
| `refvars.go` | **Create** | `${VAR}` expansion, `WithRefVars`, `EnvVars`. |
| `refvars_test.go` | **Create** | Expansion, escaping, and error-text tests. |
| `resolve.go` | Modify | Call `applyDecode` in `resolveRef` and `resolveBatchScheme`. |
| `reconciler.go` | Modify | Call `applyDecode` in `recordSourceUpdate`. |
| `decode.go` | Modify | Thread `vars` through `fieldSpecs`/`walkSpecs`; expand and validate `?decode=` at walk time. |
| `reconcile.go` | Modify | Add `refVars` to `options`; pass it to `fieldSpecs`. |
| `doctor.go` | Modify | Pass `o.refVars` to `fieldSpecs`. |
| `fuzz_test.go` | **Create** | `FuzzRefGrammar` over `ParseRefs` + `SelectKey`. |
| `providertest/providertest.go` | Modify | Two new conformance cases and the `NoStructuredPayload` opt-out. |
| `site/src/pages/docs/concepts/ref-grammar.md` | **Create** | The consolidated grammar reference. |

Three new small files rather than growing `helpers.go`: each has one responsibility, and `helpers.go` is currently 60 lines of unrelated utilities.

---

## Task 1: JSON Pointer selection in SelectKey

**Files:**
- Create: `jsonpointer.go`
- Create: `jsonpointer_test.go`
- Modify: `helpers.go:27-49` (`SelectKey`)
- Modify: `helpers_test.go`

**Interfaces:**
- Consumes: `ErrNotFound` and `ErrInvalid` from `errors.go`.
- Produces: `selectPointer(data []byte, ptr string) ([]byte, error)`, called only by `SelectKey`. `SelectKey`'s exported signature is unchanged.

**Implementation note that improves on the spec:** walk the document as `json.RawMessage` one level at a time rather than unmarshalling the whole thing into `any`. Unmarshalling into `any` and re-marshalling would normalize key order and whitespace, which would make a pointer-selected object's bytes differ from a literal-selected one's and would perturb `Value.changed`'s byte comparison. The raw walk preserves bytes exactly, matching what the existing literal path already does with `map[string]json.RawMessage`.

**Behavior change to be aware of:** today `SelectKey` returns a bare error for a non-object payload (`helpers.go:33-36`), which wraps no sentinel and therefore classifies as `KindUnknown`. This task makes it wrap `ErrInvalid`, so it classifies as `KindInvalid`. That is an intentional improvement, and Step 1 asserts it.

- [ ] **Step 1: Write the failing backward-compatibility and pointer tests**

Add to `helpers_test.go`:

```go
func TestSelectKeyLiteralDottedKeysUnchanged(t *testing.T) {
	// Guards spec decision D1. tls.crt / tls.key / ca.crt are the canonical
	// Kubernetes TLS secret keys and appear in shipped docs; a dotted key must
	// never be reinterpreted as a path.
	payload := []byte(`{"tls.crt":"CERT","tls.key":"KEY","ca.crt":"CA","application.properties":"a=1"}`)
	for _, key := range []string{"tls.crt", "tls.key", "ca.crt", "application.properties"} {
		got, err := SelectKey(payload, key)
		if err != nil {
			t.Fatalf("SelectKey(%q) unexpected error: %v", key, err)
		}
		if len(got) == 0 {
			t.Errorf("SelectKey(%q) returned empty", key)
		}
	}
}

func TestSelectKeyNonObjectPayloadIsInvalid(t *testing.T) {
	if _, err := SelectKey([]byte(`not json`), "k"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}
```

Create `jsonpointer_test.go`:

```go
package mamori

import (
	"errors"
	"testing"
)

// rfc6901Doc is the example document from RFC 6901 section 5.
var rfc6901Doc = []byte(`{
   "foo": ["bar", "baz"],
   "": 0,
   "a/b": 1,
   "c%d": 2,
   "e^f": 3,
   "g|h": 4,
   "i\\j": 5,
   "k\"l": 6,
   " ": 7,
   "m~n": 8
}`)

func TestSelectPointerRFC6901(t *testing.T) {
	tests := []struct{ ptr, want string }{
		{"/foo", `["bar", "baz"]`},
		{"/foo/0", "bar"},
		{"/foo/1", "baz"},
		{"/", "0"},
		{"/a~1b", "1"},
		{"/c%d", "2"},
		{"/e^f", "3"},
		{"/g|h", "4"},
		{"/i\\j", "5"},
		{"/k\"l", "6"},
		{"/ ", "7"},
		{"/m~0n", "8"},
	}
	for _, tt := range tests {
		got, err := SelectKey(rfc6901Doc, tt.ptr)
		if err != nil {
			t.Fatalf("SelectKey(%q) error: %v", tt.ptr, err)
		}
		if string(got) != tt.want {
			t.Errorf("SelectKey(%q) = %q, want %q", tt.ptr, got, tt.want)
		}
	}
}

// replicaDoc is the array-of-objects shape from spec section 5.2.
var replicaDoc = []byte(`{"replicas":[` +
	`{"host":"r0.db","creds":{"user":"app","password":"p0"}},` +
	`{"host":"r1.db","creds":{"user":"app","password":"p1"}},` +
	`{"host":"r2.db","creds":{"user":"app","password":"p2"}},` +
	`{"host":"r3.db","creds":{"user":"app","password":"p3"}},` +
	`{"host":"r4.db","creds":{"user":"app","password":"p4"}},` +
	`{"host":"r5.db","creds":{"user":"app","password":"p5"}}]}`)

func TestSelectPointerArrayOfObjects(t *testing.T) {
	got, err := SelectKey(replicaDoc, "/replicas/5/creds/password")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if string(got) != "p5" {
		t.Errorf("= %q, want p5", got)
	}
	got, err = SelectKey(replicaDoc, "/replicas/0/host")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if string(got) != "r0.db" {
		t.Errorf("= %q, want r0.db", got)
	}
}

func TestSelectPointerErrors(t *testing.T) {
	tests := []struct {
		name string
		ptr  string
		want error
	}{
		{"absent key", "/replicas/0/missing", ErrNotFound},
		{"index out of range", "/replicas/99", ErrNotFound},
		{"descend into scalar", "/replicas/0/host/nope", ErrInvalid},
		{"non-numeric array token", "/replicas/five", ErrInvalid},
		{"leading zero index", "/replicas/05", ErrInvalid},
		{"dash token", "/replicas/-", ErrInvalid},
		{"bad escape", "/replicas/0/a~2b", ErrInvalid},
		{"trailing tilde", "/replicas/0/a~", ErrInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := SelectKey(replicaDoc, tt.ptr)
			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestSelectPointerPreservesBytes(t *testing.T) {
	// A pointer-selected object must come back byte-identical, not
	// key-reordered by a marshal round trip.
	doc := []byte(`{"outer":{"z":1,"a":2,"m":3}}`)
	got, err := SelectKey(doc, "/outer")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"z":1,"a":2,"m":3}` {
		t.Errorf("= %q, want byte-preserved object", got)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -run 'TestSelectPointer|TestSelectKeyLiteral|TestSelectKeyNonObject' ./... -v`
Expected: FAIL. The `jsonpointer_test.go` cases fail because `selectPointer` does not exist and `SelectKey` treats `/foo` as a literal key that is absent, so they report `ErrNotFound` instead of a value. `TestSelectKeyNonObjectPayloadIsInvalid` fails because the current error wraps no sentinel.

- [ ] **Step 3: Write `jsonpointer.go`**

```go
package mamori

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// selectPointer resolves an RFC 6901 JSON Pointer against a JSON document and
// returns the addressed value, using the same encoding rule SelectKey applies
// to a literal key: a JSON string yields its unquoted contents, anything else
// yields its JSON encoding.
//
// The walk descends one level at a time over json.RawMessage rather than
// unmarshalling the whole document into an any tree. That preserves the exact
// bytes of a selected object or array: a marshal round trip would sort object
// keys and drop original whitespace, which would make a pointer-selected value
// differ byte-for-byte from a literally-selected one and perturb
// Value.changed's byte comparison for providers that supply no Version.
//
// ptr is guaranteed by the caller to begin with '/'.
func selectPointer(data []byte, ptr string) ([]byte, error) {
	cur := json.RawMessage(data)
	// A pointer's leading '/' produces an empty first token that is not a
	// reference token, so it is dropped.
	for _, raw := range strings.Split(ptr, "/")[1:] {
		tok, err := unescapeToken(raw)
		if err != nil {
			return nil, fmt.Errorf("mamori: pointer %q: %w", ptr, err)
		}
		next, err := descend(cur, tok, ptr)
		if err != nil {
			return nil, err
		}
		cur = next
	}
	return unquoteJSON(cur), nil
}

// descend resolves one reference token against one node.
func descend(node json.RawMessage, tok, ptr string) (json.RawMessage, error) {
	switch containerKind(node) {
	case '{':
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(node, &obj); err != nil {
			return nil, fmt.Errorf("mamori: pointer %q: malformed object at token %q: %w: %w", ptr, tok, ErrInvalid, err)
		}
		v, ok := obj[tok]
		if !ok {
			return nil, fmt.Errorf("mamori: pointer %q: key %q not present: %w", ptr, tok, ErrNotFound)
		}
		return v, nil
	case '[':
		var arr []json.RawMessage
		if err := json.Unmarshal(node, &arr); err != nil {
			return nil, fmt.Errorf("mamori: pointer %q: malformed array at token %q: %w: %w", ptr, tok, ErrInvalid, err)
		}
		i, err := arrayIndex(tok)
		if err != nil {
			return nil, fmt.Errorf("mamori: pointer %q: %w", ptr, err)
		}
		if i >= len(arr) {
			return nil, fmt.Errorf("mamori: pointer %q: index %d out of range (array has %d elements): %w", ptr, i, len(arr), ErrNotFound)
		}
		return arr[i], nil
	default:
		// A scalar, or a payload that is not JSON at all. Either way the
		// pointer asks for something this document cannot contain: a
		// structural mismatch, not an absence, so default:/optional must not
		// mask it.
		return nil, fmt.Errorf("mamori: pointer %q: cannot descend into a non-container value at token %q: %w", ptr, tok, ErrInvalid)
	}
}

// containerKind reports the first significant byte of raw, which is '{' for an
// object, '[' for an array, and anything else (or 0 when empty) for a scalar or
// non-JSON payload.
func containerKind(raw json.RawMessage) byte {
	for _, c := range raw {
		switch c {
		case ' ', '\t', '\n', '\r':
			continue
		default:
			return c
		}
	}
	return 0
}

// unescapeToken applies RFC 6901's escaping: "~1" is '/', "~0" is '~'. The
// order matters and is why this is a single left-to-right pass rather than two
// strings.ReplaceAll calls: replacing "~0" first would turn the literal "~01"
// into "~1" and then into "/", which is wrong.
func unescapeToken(tok string) (string, error) {
	if !strings.ContainsRune(tok, '~') {
		return tok, nil
	}
	var b strings.Builder
	b.Grow(len(tok))
	for i := 0; i < len(tok); i++ {
		if tok[i] != '~' {
			b.WriteByte(tok[i])
			continue
		}
		if i+1 >= len(tok) {
			return "", fmt.Errorf("token %q ends with an unescaped %q: %w", tok, "~", ErrInvalid)
		}
		switch tok[i+1] {
		case '0':
			b.WriteByte('~')
		case '1':
			b.WriteByte('/')
		default:
			return "", fmt.Errorf("token %q contains invalid escape %q (want ~0 or ~1): %w", tok, tok[i:i+2], ErrInvalid)
		}
		i++
	}
	return b.String(), nil
}

// arrayIndex parses an RFC 6901 array index: either "0", or a non-zero digit
// followed by digits. A leading zero is rejected rather than silently accepted
// so that "/05" fails loudly instead of aliasing "/5". The "-" token, which
// RFC 6901 defines as one-past-the-end for JSON Patch's add operation, can
// never address an existing value and is rejected for the same reason.
func arrayIndex(tok string) (int, error) {
	switch {
	case tok == "-":
		return 0, fmt.Errorf("token %q addresses one past the end of the array and can never select a value: %w", tok, ErrInvalid)
	case tok == "":
		return 0, fmt.Errorf("empty array index token: %w", ErrInvalid)
	case len(tok) > 1 && tok[0] == '0':
		return 0, fmt.Errorf("array index %q has a leading zero: %w", tok, ErrInvalid)
	}
	i, err := strconv.Atoi(tok)
	if err != nil || i < 0 {
		return 0, fmt.Errorf("token %q is not an array index: %w", tok, ErrInvalid)
	}
	return i, nil
}

// unquoteJSON returns a JSON string's unquoted contents, or raw unchanged for
// any other JSON value. It is the shared final step of both SelectKey's literal
// path and selectPointer, so both encode a selected value identically.
func unquoteJSON(raw json.RawMessage) []byte {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []byte(s)
	}
	return raw
}
```

- [ ] **Step 4: Wire it into `SelectKey`**

Replace the body of `SelectKey` in `helpers.go` with:

```go
func SelectKey(data []byte, key string) ([]byte, error) {
	if key == "" {
		return data, nil
	}
	if strings.HasPrefix(key, "/") {
		return selectPointer(data, key)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, fmt.Errorf("mamori: cannot select key %q: payload is not a JSON object: %w: %w", key, ErrInvalid, err)
	}
	raw, ok := obj[key]
	if !ok {
		return nil, fmt.Errorf("mamori: key %q not present in payload: %w", key, ErrNotFound)
	}
	return unquoteJSON(raw), nil
}
```

Add `"strings"` to `helpers.go`'s imports. Update the doc comment above `SelectKey` to document the pointer rule:

```go
// SelectKey extracts a single value from a structured payload, for refs of the
// form scheme://path#key.
//
// The fragment is interpreted one of two ways, chosen by its first character:
//
//   - A fragment beginning with '/' is an RFC 6901 JSON Pointer, addressing a
//     value at any depth, through objects and array elements alike:
//     "#/credentials/password", "#/replicas/5/host". Escapes are RFC 6901's:
//     "~1" for a literal '/', "~0" for a literal '~'.
//   - Any other fragment is a literal top-level key, exactly as it has always
//     been. This is what keeps "#ca.crt" addressing the key named "ca.crt"
//     rather than a path, which matters because "tls.crt"/"tls.key"/"ca.crt"
//     are the canonical Kubernetes TLS secret keys and dotted keys are the norm
//     in ConfigMaps and Java properties files.
//
// If key is empty, data is returned unchanged. String values are returned
// unquoted; objects, arrays, numbers, and booleans are returned as their JSON
// encoding, byte-for-byte as they appeared in the payload.
//
// An absent key or an out-of-range index wraps ErrNotFound, so the field's
// default: or optional handling applies. A structural mismatch (a pointer
// descending into a scalar, a non-numeric token against an array, a malformed
// escape, or a payload that is not JSON) wraps ErrInvalid instead, because it
// is a malformed request against this payload rather than an absence and must
// not be silently masked by a default.
//
// Provider authors should call SelectKey with ref.Key after fetching the raw
// payload, so that fragment selection behaves identically across all providers.
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test -race -run 'TestSelectPointer|TestSelectKey' . -v`
Expected: PASS, all cases.

- [ ] **Step 6: Run the full suite**

Run: `go test -race ./...`
Expected: PASS. Watch specifically for `TestSelectKey` in `helpers_test.go`, which asserts the existing literal behavior and must still pass unchanged.

- [ ] **Step 7: Add the stack layer and commit**

```bash
# Adopts the spec branch as the stack bottom, once, then adds this layer on top.
git checkout xavier/new-features-2
gh stack init xavier/new-features-2
gh stack add xavier/ref-grammar-pointer

git add jsonpointer.go jsonpointer_test.go helpers.go helpers_test.go
git commit -m "feat: RFC 6901 JSON Pointer selection in SelectKey

A source-tag fragment beginning with '/' is now an RFC 6901 JSON Pointer,
addressing a value at any depth through objects and array elements. Any other
fragment stays a literal top-level key, so shipped refs like #ca.crt and
#tls.key keep addressing the keys they always did.

Absence (a missing key, an out-of-range index) wraps ErrNotFound so default:
and optional still apply. A structural mismatch (descending into a scalar, a
non-numeric array token, a leading-zero index, a bad escape, or a non-JSON
payload) wraps ErrInvalid so it cannot be silently masked by a default. That
last case also fixes a non-object payload previously classifying as
KindUnknown.

The walk descends over json.RawMessage one level at a time rather than
unmarshalling into an any tree, so a selected object comes back byte-identical
instead of key-reordered by a marshal round trip.

All 26 provider files that call mamori.SelectKey inherit this unchanged.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

## Task 2: Conformance case for pointer selection

**Files:**
- Modify: `providertest/providertest.go:32-98` (`Config`), `:124-139` (`Run`)

**Interfaces:**
- Consumes: `mamori.SelectKey` behavior from Task 1.
- Produces: `providertest.Config.NoStructuredPayload bool`, and a `JSONPointerSelection` subtest registered in `Run`.

- [ ] **Step 1: Add the opt-out field to `Config`**

Insert after the `NoResolveErrors` field in `providertest/providertest.go`:

```go
	// NoStructuredPayload declares that this provider's values are never JSON
	// documents, so fragment selection cannot be exercised against it. A
	// feature-flag provider returning a bare bool, or a backend whose values are
	// opaque binary, is the usual case.
	//
	// Like NoResolveErrors, this is a deliberate, greppable declaration rather
	// than a silent gap: a provider that stores JSON and does not call
	// mamori.SelectKey has a real bug, and the JSONPointerSelection case exists
	// to catch exactly that.
	NoStructuredPayload bool
```

- [ ] **Step 2: Write the conformance case**

Add to `providertest/providertest.go`:

```go
// testJSONPointerSelection verifies the provider routes its #fragment through
// mamori.SelectKey, so nested selection works identically everywhere. A
// provider that hand-rolls a top-level-only key lookup passes every other case
// in this kit and fails this one.
func testJSONPointerSelection(t *testing.T, c Config) {
	t.Helper()
	if c.NoStructuredPayload {
		t.Skip("providertest: provider declares NoStructuredPayload; values are never JSON documents")
		return
	}
	ctx := context.Background()
	p := c.New()
	key := testKey(c, "jsonpointer")

	const payload = `{"outer":{"inner":"deep"},"list":[{"n":"zero"},{"n":"one"}],"dotted.key":"literal"}`
	if err := c.Seed(ctx, key, payload); err != nil {
		t.Fatalf("Seed: %v", err)
	}

	cases := []struct{ frag, want string }{
		{"#/outer/inner", "deep"},
		{"#/list/1/n", "one"},
		{"#dotted.key", "literal"}, // literal fragment, not a path
	}
	for _, tc := range cases {
		ref, err := mamori.ParseRef(c.Ref(key) + tc.frag)
		if err != nil {
			t.Fatalf("ParseRef(%q): %v", tc.frag, err)
		}
		v, err := p.Resolve(ctx, ref)
		if err != nil {
			t.Fatalf("Resolve(%q): %v", tc.frag, err)
		}
		if string(v.Bytes) != tc.want {
			t.Errorf("Resolve(%q) = %q, want %q", tc.frag, v.Bytes, tc.want)
		}
	}

	ref, err := mamori.ParseRef(c.Ref(key) + "#/outer/absent")
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	if _, err := p.Resolve(ctx, ref); !errors.Is(err, mamori.ErrNotFound) {
		t.Errorf("absent pointer err = %v, want ErrNotFound", err)
	}

	ref, err = mamori.ParseRef(c.Ref(key) + "#/outer/inner/deeper")
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	if _, err := p.Resolve(ctx, ref); !errors.Is(err, mamori.ErrInvalid) {
		t.Errorf("descend-into-scalar err = %v, want ErrInvalid", err)
	}
}
```

If a `testKey(c, name)` helper does not already exist in the file, use the same expression the other cases use to derive a key from `c.Key`; read `testResolveSeeded` at `providertest/providertest.go:152` and copy its key derivation verbatim rather than inventing a second convention.

- [ ] **Step 3: Register the case in `Run`**

In `Run`, after the `NotFoundTyped` line:

```go
	t.Run("JSONPointerSelection", func(t *testing.T) { testJSONPointerSelection(t, c) })
```

- [ ] **Step 4: Run the conformance kit across every provider module**

Run:
```bash
go test -race ./providertest/...
for d in providers/*/; do (cd "$d" && go test -race ./... 2>&1 | tail -3); done
```
Expected: PASS everywhere, or a clear `NoStructuredPayload` skip. Any provider that fails here stores JSON but does not call `mamori.SelectKey`. Fix by setting `NoStructuredPayload` only if its values genuinely are never JSON; otherwise the provider has a real bug and this case just found it. **Report any such provider rather than silently setting the flag.**

- [ ] **Step 5: Commit**

```bash
git add providertest/providertest.go providers/
git commit -m "test: conformance case for JSON Pointer fragment selection

JSONPointerSelection verifies every provider routes its #fragment through
mamori.SelectKey, so nested selection, literal dotted keys, and the
ErrNotFound/ErrInvalid split behave identically across the ecosystem. A
provider that hand-rolls a top-level-only lookup passes every other case in
the kit and fails this one.

Config.NoStructuredPayload opts out providers whose values are never JSON
documents, following the NoResolveErrors precedent of an explicit, greppable
declaration rather than a silent skip.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

## Task 3: Documentation for pointer selection

**Files:**
- Create: `site/src/pages/docs/concepts/ref-grammar.md`
- Modify: `site/src/pages/docs/concepts/index.md`, `site/src/pages/docs/usage/index.md`, `site/src/pages/docs/writing-a-provider/resolve.md`, `site/src/pages/docs/writing-a-provider/conformance.md`, `site/src/pages/docs/providers/kubernetes.md`, `README.md`, `providers/k8s/README.md`, `skills/mamori/SKILL.md`, `CONTRIBUTING.md`, `doc.go`

- [ ] **Step 1: Read the surrounding pages before writing**

Run: `ls site/src/pages/docs/concepts/ && head -30 site/src/pages/docs/concepts/source-chains.md`
Copy that page's frontmatter shape, heading levels, and code-fence style exactly. Do not invent a new page format.

- [ ] **Step 2: Create `site/src/pages/docs/concepts/ref-grammar.md`**

Content must cover, with runnable `go` and text fences:
- The full grammar, both hierarchical and opaque forms, with the `#key` before `?opts` ordering and why it differs from a standard URL.
- The two fragment forms, with the leading-`/` discriminator stated first.
- The RFC 6901 escape table (`~0`, `~1`) and the zero-based array-index rule including the leading-zero and `-` rejections.
- The full error table from spec section 5.3, with a column for whether `default:` applies.
- The string-containing-JSON case and its `flatten:"json"` remedy.
- Placeholder sections for `?decode=` and `${VAR}`, added by Tasks 8 and 11. Mark them with a real heading and one sentence, not a TODO.

- [ ] **Step 3: Add the literal-key note to both Kubernetes pages**

In `providers/k8s/README.md` near line 23 and `site/src/pages/docs/providers/kubernetes.md` near line 46, after the `#ca.crt` example:

> `ca.crt`, `tls.crt`, and `tls.key` are literal key names, not paths. A mamori fragment is only a JSON Pointer when it begins with `/`, so a dotted key addresses exactly the key it names. A Kubernetes Secret's `data` is a flat map with no nesting to point into, so this provider only ever does a literal lookup.

- [ ] **Step 4: Update the remaining files**

- `README.md`: add a nested-selection line to the "What makes it different" list, and one nested field to the quick-start struct.
- `site/src/pages/docs/usage/index.md`: nested selection in the field-tag walkthrough.
- `site/src/pages/docs/writing-a-provider/resolve.md`: `SelectKey`'s new contract, and that calling it is what earns pointer support for free.
- `site/src/pages/docs/writing-a-provider/conformance.md`: the `JSONPointerSelection` case and `NoStructuredPayload`.
- `CONTRIBUTING.md`: add `JSONPointerSelection` to the provider checklist near the existing conformance step.
- `skills/mamori/SKILL.md`: the fragment rule, so agents stop emitting only top-level keys.
- `doc.go`: one grammar line in the package doc.
- `site/src/pages/docs/concepts/index.md`: link the new page.

- [ ] **Step 5: Verify the site builds**

Run: `cd site && npm run build`
Expected: build succeeds, no broken-link warnings for the new page.

- [ ] **Step 6: Commit and submit the stack through PR 2**

```bash
git add -A
git commit -m "docs: JSON Pointer fragment selection

Adds a consolidated concepts/ref-grammar.md page, which the docs site has
lacked; the grammar is now large enough to need one. Notes on both Kubernetes
pages that ca.crt and tls.key stay literal keys and why.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"

gh stack submit
gh stack view
```

`gh stack submit` pushes both branches and creates PR 1 (the spec) and PR 2,
with PR 2 based on PR 1's branch automatically. In a non-interactive session use
`gh stack submit --auto --open`. Confirm with `gh stack view` that PR 2's base
is `xavier/new-features-2` and not `main`.

---

## Task 4: The decode coding registry and pipeline

**Files:**
- Create: `decodeopt.go`
- Create: `decodeopt_test.go`

**Interfaces:**
- Consumes: `Ref`, `Value`, `ErrInvalid`, and `redactRef` (`status.go:53`).
- Produces:
  - `parseDecodePipeline(spec string) ([]decodeStep, error)`
  - `applyDecode(ref Ref, v Value) (Value, error)`
  - `type decodeStep struct { name string; fn func([]byte) ([]byte, error) }`

Task 6 calls `applyDecode`. Task 5 calls `parseDecodePipeline` for validation.

- [ ] **Step 1: Write the failing tests**

Create `decodeopt_test.go`:

```go
package mamori

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"testing"
)

func gz(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestApplyDecodeRoundTrip(t *testing.T) {
	plain := []byte("s3cr3t")
	tests := []struct {
		spec string
		in   []byte
	}{
		{"base64", []byte(base64.StdEncoding.EncodeToString(plain))},
		{"base64url", []byte(base64.URLEncoding.EncodeToString(plain))},
		{"hex", []byte(hex.EncodeToString(plain))},
		{"gzip", gz(t, plain)},
		{"trim", []byte("  s3cr3t\n")},
	}
	for _, tt := range tests {
		t.Run(tt.spec, func(t *testing.T) {
			ref, err := ParseRef("env:X?decode=" + tt.spec)
			if err != nil {
				t.Fatal(err)
			}
			got, err := applyDecode(ref, Value{Bytes: tt.in})
			if err != nil {
				t.Fatalf("applyDecode: %v", err)
			}
			if string(got.Bytes) != string(plain) {
				t.Errorf("= %q, want %q", got.Bytes, plain)
			}
		})
	}
}

func TestApplyDecodeChainAppliesLeftToRight(t *testing.T) {
	plain := []byte("s3cr3t")
	// decode=base64,gzip means: base64-decode first, then gunzip. So the stored
	// value is base64(gzip(plain)).
	stored := []byte(base64.StdEncoding.EncodeToString(gz(t, plain)))
	ref, err := ParseRef("env:X?decode=base64,gzip")
	if err != nil {
		t.Fatal(err)
	}
	got, err := applyDecode(ref, Value{Bytes: stored})
	if err != nil {
		t.Fatalf("applyDecode: %v", err)
	}
	if string(got.Bytes) != string(plain) {
		t.Errorf("= %q, want %q", got.Bytes, plain)
	}
}

func TestApplyDecodeFailureIsInvalid(t *testing.T) {
	for _, spec := range []string{"base64", "base64url", "hex", "gzip"} {
		t.Run(spec, func(t *testing.T) {
			ref, err := ParseRef("env:X?decode=" + spec)
			if err != nil {
				t.Fatal(err)
			}
			_, err = applyDecode(ref, Value{Bytes: []byte("!!! not valid !!!")})
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestApplyDecodePreservesMetadata(t *testing.T) {
	ref, err := ParseRef("env:X?decode=base64")
	if err != nil {
		t.Fatal(err)
	}
	in := Value{
		Bytes:     []byte(base64.StdEncoding.EncodeToString([]byte("v"))),
		Version:   "abc123",
		Sensitive: true,
		Metadata:  map[string]string{"k": "v"},
	}
	got, err := applyDecode(ref, in)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != "abc123" {
		t.Errorf("Version = %q, want abc123 (the provider revision describes the source, not the decoded form)", got.Version)
	}
	if !got.Sensitive {
		t.Error("Sensitive was lost across the decode pipeline")
	}
	if got.Metadata["k"] != "v" {
		t.Error("Metadata was lost across the decode pipeline")
	}
}

func TestApplyDecodeNoOptIsPassthrough(t *testing.T) {
	ref, err := ParseRef("env:X")
	if err != nil {
		t.Fatal(err)
	}
	got, err := applyDecode(ref, Value{Bytes: []byte("raw")})
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Bytes) != "raw" {
		t.Errorf("= %q, want raw", got.Bytes)
	}
}

func TestParseDecodePipelineUnknownCoding(t *testing.T) {
	_, err := parseDecodePipeline("base64,rot13")
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test -run 'TestApplyDecode|TestParseDecodePipeline' . -v`
Expected: FAIL, compilation error: `undefined: applyDecode`, `undefined: parseDecodePipeline`.

- [ ] **Step 3: Write `decodeopt.go`**

```go
package mamori

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
)

// decodeStep is one coding in a ?decode= pipeline, carrying its name so a
// failure can say which stage failed rather than only that decoding failed.
type decodeStep struct {
	name string
	fn   func([]byte) ([]byte, error)
}

// decodeCodings is the closed set of codings ?decode= understands. It is
// deliberately closed and deliberately stdlib-only: core's dependency set is a
// stated property of the project layout, and an extension point here would
// duplicate WithDecodeHook, which already exists one layer down for arbitrary
// per-type conversion.
var decodeCodings = map[string]func([]byte) ([]byte, error){
	"base64":    func(b []byte) ([]byte, error) { return base64.StdEncoding.DecodeString(string(bytes.TrimSpace(b))) },
	"base64url": func(b []byte) ([]byte, error) { return base64.URLEncoding.DecodeString(string(bytes.TrimSpace(b))) },
	"hex":       func(b []byte) ([]byte, error) { return hex.DecodeString(string(bytes.TrimSpace(b))) },
	"gzip":      gunzip,
	"trim":      func(b []byte) ([]byte, error) { return bytes.TrimSpace(b), nil },
}

func gunzip(b []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer func() { _ = zr.Close() }()
	return io.ReadAll(zr)
}

// parseDecodePipeline turns a ?decode= option value into an ordered pipeline.
// Codings are applied left to right, outermost wrapper first: "base64,gzip"
// means the stored value is base64 of gzip of the payload, so it is
// base64-decoded and then gunzipped.
//
// The option is named decode rather than encoding precisely so that order is
// unambiguous. HTTP's Content-Encoding lists codings in the order they were
// applied and is therefore decoded in reverse; naming this one after the action
// removes any question of which direction the list reads.
func parseDecodePipeline(spec string) ([]decodeStep, error) {
	if spec == "" {
		return nil, nil
	}
	parts := strings.Split(spec, ",")
	steps := make([]decodeStep, 0, len(parts))
	for _, p := range parts {
		name := strings.TrimSpace(p)
		fn, ok := decodeCodings[name]
		if !ok {
			return nil, fmt.Errorf("mamori: unknown decode coding %q (want base64, base64url, hex, gzip, or trim): %w", name, ErrInvalid)
		}
		steps = append(steps, decodeStep{name: name, fn: fn})
	}
	return steps, nil
}

// applyDecode runs ref's ?decode= pipeline over v's bytes, returning v
// unchanged when the ref carries no decode option.
//
// Version, Sensitive, NotAfter, and Metadata are carried through untouched. In
// particular Version still describes the provider's revision of the source, not
// the decoded form, so Value.changed keeps detecting change exactly as it does
// for an undecoded field.
//
// The pipeline is re-derived per call rather than cached on the Ref: it is a
// string split and a map lookup per coding, which is free next to the network
// round trip that produced v, and caching it would mean giving Ref mutable
// state it does not otherwise have.
func applyDecode(ref Ref, v Value) (Value, error) {
	steps, err := parseDecodePipeline(ref.Opt("decode"))
	if err != nil {
		return Value{}, fmt.Errorf("mamori: ref %q: %w", redactRef(ref), err)
	}
	if len(steps) == 0 {
		return v, nil
	}
	b := v.Bytes
	for _, s := range steps {
		b, err = s.fn(b)
		if err != nil {
			return Value{}, fmt.Errorf("mamori: ref %q: %s decode failed: %w: %w", redactRef(ref), s.name, ErrInvalid, err)
		}
	}
	v.Bytes = b
	return v, nil
}
```

- [ ] **Step 4: Run to verify pass**

Run: `go test -race -run 'TestApplyDecode|TestParseDecodePipeline' . -v`
Expected: PASS, all subtests.

- [ ] **Step 5: Add the stack layer and commit**

```bash
gh stack add xavier/ref-grammar-decode
git add decodeopt.go decodeopt_test.go
git commit -m "feat: ?decode= coding registry and pipeline

Adds the closed, stdlib-only set of codings a source ref can declare (base64,
base64url, hex, gzip, trim) and the pipeline that applies them left to right,
outermost wrapper first: decode=base64,gzip base64-decodes and then gunzips.

A failed decode wraps ErrInvalid, naming the stage that failed. Version,
Sensitive, NotAfter, and Metadata pass through untouched, so change detection
keeps working on the provider's own revision rather than the decoded bytes.

Not yet wired into any resolve path; that is the next commit.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

## Task 5: Reject unknown codings at spec-walk time

**Files:**
- Modify: `decode.go:68-140` (`walkSpecs`)
- Modify: `decode_test.go`

**Interfaces:**
- Consumes: `parseDecodePipeline` from Task 4.
- Produces: no new symbols. `fieldSpecs` gains a failure mode.

A typo in `?decode=` must fail at `Watch()` alongside every other tag error, not an hour later on a poll tick.

- [ ] **Step 1: Write the failing test**

Add to `decode_test.go`:

```go
func TestFieldSpecsRejectsUnknownDecodeCoding(t *testing.T) {
	type cfg struct {
		A string `source:"env:A?decode=rot13"`
	}
	_, err := fieldSpecs(reflect.TypeOf(cfg{}))
	if err == nil {
		t.Fatal("want an error for an unknown decode coding, got nil")
	}
	if !strings.Contains(err.Error(), "rot13") {
		t.Errorf("error %q should name the offending coding", err)
	}
	if !strings.Contains(err.Error(), "A") {
		t.Errorf("error %q should name the offending field", err)
	}
}

func TestFieldSpecsAcceptsKnownDecodeCodings(t *testing.T) {
	type cfg struct {
		A string `source:"env:A?decode=base64,gzip"`
	}
	if _, err := fieldSpecs(reflect.TypeOf(cfg{})); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
```

Add `"strings"` to `decode_test.go`'s imports if absent.

- [ ] **Step 2: Run to verify failure**

Run: `go test -run TestFieldSpecsRejects . -v`
Expected: FAIL, "want an error for an unknown decode coding, got nil".

- [ ] **Step 3: Validate in `walkSpecs`**

In `decode.go`, immediately after the `refs, err := ParseRefs(source)` block and its error check, add:

```go
		// Every ref in the chain is validated, not just the first: a typo in a
		// lower-precedence position would otherwise stay silent until that
		// position actually won.
		for _, r := range refs {
			if _, err := parseDecodePipeline(r.Opt("decode")); err != nil {
				return nil, fmt.Errorf("mamori: field %s: %w", path, err)
			}
		}
```

- [ ] **Step 4: Run to verify pass**

Run: `go test -race -run TestFieldSpecs . -v`
Expected: PASS.

- [ ] **Step 5: Run the full suite and commit**

```bash
go test -race ./...
git add decode.go decode_test.go
git commit -m "feat: reject unknown ?decode= codings at spec-walk time

A typo now fails at Load/Watch/Doctor alongside every other source-tag error
rather than an hour later on a poll tick. Every ref in a precedence chain is
checked, not just the first, so a typo in a lower-precedence position cannot
stay silent until that position wins.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

## Task 6: Wire applyDecode into all three value entry points

**Files:**
- Modify: `resolve.go:140-159` (`resolveRef`), `resolve.go:192-215` (`resolveBatchScheme`)
- Modify: `reconciler.go:665-685` (`recordSourceUpdate`)
- Create: `decodepath_test.go`

**Interfaces:**
- Consumes: `applyDecode` from Task 4.
- Produces: no new symbols.

**This is the correctness-critical task in the plan.** A `Value` enters the engine at three places. Wiring only the first would mean a watched field decodes on load and then silently stops decoding on its first update, which is the worst possible failure mode: correct in every quick test, wrong in production after the first rotation.

- [ ] **Step 1: Write the failing tests**

Create `decodepath_test.go`:

```go
package mamori

import (
	"context"
	"encoding/base64"
	"testing"
	"time"

	"github.com/xavidop/mamori/mamoritest"
)

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func TestDecodeAppliesOnLoad(t *testing.T) {
	type cfg struct {
		A string `source:"test://a?decode=base64"`
	}
	p := mamoritest.New("test")
	p.Set("a", b64("plain"))

	got, err := Load[cfg](context.Background(), WithProvider(p))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.A != "plain" {
		t.Errorf("A = %q, want plain", got.A)
	}
}

// TestDecodeAppliesOnWatchUpdate is the regression test for the failure mode
// this task exists to prevent: decoding correctly on the initial load and then
// silently passing raw bytes through on every subsequent update.
func TestDecodeAppliesOnWatchUpdate(t *testing.T) {
	type cfg struct {
		A string `source:"test://a?decode=base64"`
	}
	p := mamoritest.New("test")
	p.Set("a", b64("first"))

	changed := make(chan cfg, 4)
	w, err := Watch[cfg](context.Background(),
		WithProvider(p),
		WithPollInterval(10*time.Millisecond),
		WithDebounce(time.Millisecond),
		OnChange(func(ev Change[cfg]) { changed <- ev.New }),
	)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	if got := w.Get().A; got != "first" {
		t.Fatalf("initial A = %q, want first", got)
	}

	p.Set("a", b64("second"))

	select {
	case got := <-changed:
		if got.A != "second" {
			t.Errorf("after update A = %q, want second (decode was not applied on the watch path)", got.A)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the update")
	}
}
```

Before writing this, read `mamoritest/mamoritest.go` and `watch_test.go` and use whatever constructor and seeding calls they actually use. `mamoritest.New("test")` and `p.Set(...)` above are the shape documented in the README; if the real API differs, follow the real API and keep the test's assertions identical.

Add a `BatchProvider` case in the same file exercising `resolveBatchScheme`, modeled on whatever existing test covers the batch path (search: `grep -rn "BatchProvider" *_test.go`).

- [ ] **Step 2: Run to verify failure**

Run: `go test -race -run TestDecodeApplies . -v`
Expected: FAIL. `TestDecodeAppliesOnLoad` fails with `A = "cGxhaW4="`, and `TestDecodeAppliesOnWatchUpdate` fails the same way.

- [ ] **Step 3: Wire `resolveRef`**

In `resolve.go`, replace the tail of `resolveRef`:

```go
	if err != nil {
		return Value{}, &ProviderError{Scheme: ref.Scheme, Ref: redactRef(ref), Err: err}
	}
	val, err = applyDecode(ref, val)
	if err != nil {
		return Value{}, &ProviderError{Scheme: ref.Scheme, Ref: redactRef(ref), Err: err}
	}
	return val, nil
```

- [ ] **Step 4: Wire `resolveBatchScheme`**

In `resolve.go`, inside the result loop, replace `setResolved(r, val)` with:

```go
		dec, derr := applyDecode(r.spec.Refs[0], val)
		if derr != nil {
			return &ProviderError{Scheme: scheme, Ref: redactRef(r.spec.Refs[0]), Err: derr}
		}
		setResolved(r, dec)
```

- [ ] **Step 5: Wire `recordSourceUpdate`**

In `reconciler.go`, replace the success tail of `recordSourceUpdate`:

```go
	val := up.Value
	// Decode before the value becomes engine state, so every consumer of
	// srcState (recomputeWinner, the report, the snapshot) sees decoded bytes.
	// A decode failure is recorded exactly like a transient resolve failure:
	// the position carries the error, recomputeWinner stops the chain walk
	// there, and the field keeps its last good value.
	dec, err := applyDecode(e.specs[specIdx].Refs[pos], val)
	if err != nil {
		st.err = err
		st.value = Value{}
		return
	}
	val = dec
	if sensitive {
		val.Sensitive = true
	}
	st.value = val
	st.err = nil
```

- [ ] **Step 6: Run to verify pass**

Run: `go test -race -run TestDecodeApplies . -v`
Expected: PASS, both tests.

- [ ] **Step 7: Run the full suite and commit**

Run: `go test -race ./...`
Expected: PASS.

```bash
git add resolve.go reconciler.go decodepath_test.go
git commit -m "feat: apply ?decode= at every point a Value enters the engine

A Value arrives at three places: resolveRef (Load, Doctor, and the reconciler's
re-resolves), resolveBatchScheme (any BatchProvider), and recordSourceUpdate
(every native watch and poll update). Wiring only the first would decode
correctly on load and then silently pass raw bytes through on the first
update, which is a failure that passes every quick test and breaks in
production after the first rotation. TestDecodeAppliesOnWatchUpdate is the
regression test for exactly that.

On the watch path a decode failure is recorded like a transient resolve
failure: the chain position carries the error and the field keeps its last
good value.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

## Task 7: Conformance case for ?decode=

**Files:**
- Modify: `providertest/providertest.go`

**Interfaces:**
- Consumes: `applyDecode` behavior via a real `Load`, and `Config.NoStructuredPayload` from Task 2.
- Produces: a `DecodeOption` subtest registered in `Run`.

Decoding is core-owned, so every provider gets it for free. The case exists to prove exactly that, and to catch a provider that mangles or strips unknown query options before core sees them.

- [ ] **Step 1: Write and register the case**

```go
// testDecodeOption verifies a provider passes an unrecognized query option
// through untouched, so core's ?decode= pipeline sees it. Decoding itself is
// core's job; what this case catches is a provider that strips, rewrites, or
// chokes on a query option it does not recognize.
func testDecodeOption(t *testing.T, c Config) {
	t.Helper()
	ctx := context.Background()
	p := c.New()
	key := testKey(c, "decodeopt")

	if err := c.Seed(ctx, key, base64.StdEncoding.EncodeToString([]byte("plain"))); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	ref, err := mamori.ParseRef(c.Ref(key) + "?decode=base64")
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	// The provider must resolve normally and ignore the option; core decodes.
	v, err := p.Resolve(ctx, ref)
	if err != nil {
		t.Fatalf("Resolve with an unrecognized query option: %v", err)
	}
	if string(v.Bytes) != base64.StdEncoding.EncodeToString([]byte("plain")) {
		t.Errorf("provider returned %q; it must return the stored value untouched and leave ?decode= to core", v.Bytes)
	}
}
```

Register in `Run` after `JSONPointerSelection`:

```go
	t.Run("DecodeOption", func(t *testing.T) { testDecodeOption(t, c) })
```

Add `"encoding/base64"` to the imports.

- [ ] **Step 2: Run across every provider module**

Run:
```bash
go test -race ./providertest/...
for d in providers/*/; do (cd "$d" && go test -race ./... 2>&1 | tail -3); done
```
Expected: PASS everywhere. A failure means that provider rejects or rewrites unknown query options, which is a real bug worth reporting rather than skipping.

- [ ] **Step 3: Commit**

```bash
git add providertest/providertest.go
git commit -m "test: conformance case for ?decode= option passthrough

Decoding is core-owned, so every provider gets it for free. DecodeOption proves
that by asserting a provider passes an unrecognized query option through
untouched, catching one that strips, rewrites, or chokes on it.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

## Task 8: Documentation for ?decode=

**Files:**
- Modify: `site/src/pages/docs/concepts/ref-grammar.md`, `site/src/pages/docs/usage/index.md`, `site/src/pages/docs/writing-a-provider/conformance.md`, `README.md`, `skills/mamori/SKILL.md`, `CONTRIBUTING.md`

- [ ] **Step 1: Fill in the `?decode=` section of the grammar page**

Replace the placeholder section from Task 3 Step 2 with the coding table, the left-to-right ordering rule and its Content-Encoding rationale, the `ErrInvalid` failure behavior, the note that `Version` is unaffected so change detection is unchanged, and the ordering consequence that decoding runs after `#key` selection with the `flatten:"json"` remedy for the decode-then-select case.

- [ ] **Step 2: Update the remaining files**

- `README.md`: a `?decode=` bullet, and one field in the quick-start struct using it.
- `site/src/pages/docs/usage/index.md`: `?decode=` in the field-tag walkthrough.
- `site/src/pages/docs/writing-a-provider/conformance.md`: the `DecodeOption` case, and the rule that a provider must pass unrecognized query options through untouched.
- `CONTRIBUTING.md`: add `DecodeOption` to the provider checklist.
- `skills/mamori/SKILL.md`: the coding list and ordering rule.

- [ ] **Step 3: Verify the site builds, commit, submit through PR 3**

```bash
cd site && npm run build && cd ..
git add -A
git commit -m "docs: ?decode= value transforms

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"

gh stack submit
gh stack view
```

`gh stack submit` updates PRs 1 and 2 in place and creates PR 3 based on
`xavier/ref-grammar-pointer`. It does not duplicate the existing PRs.

---

## Task 9: ${VAR} interpolation via WithRefVars

**Files:**
- Create: `refvars.go`
- Create: `refvars_test.go`
- Modify: `decode.go:61-68` (`fieldSpecs`, `walkSpecs`), `reconcile.go:21-48` (`options`), `:168-186` (`loadValue`), `doctor.go:29`
- Modify: `decode_test.go` (6 `fieldSpecs` call sites)

**Interfaces:**
- Consumes: `ErrInvalid`.
- Produces:
  - `func WithRefVars(vars map[string]string) Option`
  - `func EnvVars(names ...string) map[string]string`
  - `func expandRefVars(tag string, vars map[string]string) (string, error)`
  - `fieldSpecs(t reflect.Type, vars map[string]string) ([]fieldSpec, error)` (signature change)
  - `walkSpecs(t reflect.Type, prefix string, index []int, vars map[string]string) ([]fieldSpec, error)` (signature change)
  - `options.refVars map[string]string`

- [ ] **Step 1: Write the failing tests**

Create `refvars_test.go`:

```go
package mamori

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestExpandRefVars(t *testing.T) {
	vars := map[string]string{"ENV": "prod", "SVC": "checkout", "EMPTY": ""}
	tests := []struct{ in, want string }{
		{"aws-sm://${ENV}/db#password", "aws-sm://prod/db#password"},
		{"${ENV}-sm://x", "prod-sm://x"},
		{"aws-sm://a?tag=${SVC}", "aws-sm://a?tag=checkout"},
		{"aws-sm://a#${SVC}", "aws-sm://a#checkout"},
		{"env:PORT,aws-ps://${SVC}/port", "env:PORT,aws-ps://checkout/port"},
		{"env:NO_VARS_HERE", "env:NO_VARS_HERE"},
		{"exec:echo $HOME", "exec:echo $HOME"},   // bare $ is left alone
		{"exec:echo $$HOME", "exec:echo $HOME"},  // $$ is a literal $
		{"aws-sm://${EMPTY}x", "aws-sm://x"},     // defined-but-empty is legal
	}
	for _, tt := range tests {
		got, err := expandRefVars(tt.in, vars)
		if err != nil {
			t.Fatalf("expandRefVars(%q): %v", tt.in, err)
		}
		if got != tt.want {
			t.Errorf("expandRefVars(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestExpandRefVarsUndefined(t *testing.T) {
	_, err := expandRefVars("aws-sm://${NOPE}/db", map[string]string{})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
	if !strings.Contains(err.Error(), "NOPE") {
		t.Errorf("error %q should name the variable", err)
	}
	if !strings.Contains(err.Error(), "WithRefVars") {
		t.Errorf("error %q should say how to fix it", err)
	}
}

func TestExpandRefVarsUnterminated(t *testing.T) {
	if _, err := expandRefVars("aws-sm://${NOPE/db", map[string]string{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}

func TestFieldSpecsExpandsAndReportsField(t *testing.T) {
	type cfg struct {
		Pass string `source:"aws-sm://${ENV}/db#password"`
	}
	specs, err := fieldSpecs(reflect.TypeOf(cfg{}), map[string]string{"ENV": "prod"})
	if err != nil {
		t.Fatalf("fieldSpecs: %v", err)
	}
	if got := specs[0].Refs[0].Path; got != "prod/db" {
		t.Errorf("Path = %q, want prod/db", got)
	}
	if got := specs[0].Refs[0].Raw; !strings.Contains(got, "prod") {
		t.Errorf("Raw = %q, want the expanded form", got)
	}

	_, err = fieldSpecs(reflect.TypeOf(cfg{}), nil)
	if err == nil {
		t.Fatal("want an error for an undefined variable, got nil")
	}
	if !strings.Contains(err.Error(), "Pass") {
		t.Errorf("error %q should name the field", err)
	}
}

func TestWithRefVarsMerges(t *testing.T) {
	o := defaultOptions()
	WithRefVars(map[string]string{"A": "1", "B": "2"})(o)
	WithRefVars(map[string]string{"B": "override", "C": "3"})(o)
	want := map[string]string{"A": "1", "B": "override", "C": "3"}
	for k, v := range want {
		if o.refVars[k] != v {
			t.Errorf("refVars[%q] = %q, want %q", k, o.refVars[k], v)
		}
	}
}

func TestEnvVarsOmitsUnset(t *testing.T) {
	t.Setenv("MAMORI_TEST_SET", "yes")
	got := EnvVars("MAMORI_TEST_SET", "MAMORI_TEST_DEFINITELY_UNSET")
	if got["MAMORI_TEST_SET"] != "yes" {
		t.Errorf("set var = %q, want yes", got["MAMORI_TEST_SET"])
	}
	// An unset variable must be absent rather than empty, so expansion reports
	// the undefined-variable error rather than silently expanding to "".
	if _, present := got["MAMORI_TEST_DEFINITELY_UNSET"]; present {
		t.Error("unset var must be omitted, not mapped to the empty string")
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test -run 'TestExpandRefVars|TestWithRefVars|TestEnvVars|TestFieldSpecsExpands' . -v`
Expected: FAIL, compilation errors for the undefined symbols and the two-argument `fieldSpecs`.

- [ ] **Step 3: Write `refvars.go`**

```go
package mamori

import (
	"fmt"
	"os"
	"strings"
)

// WithRefVars supplies the variables available to ${VAR} expansion in `source`
// struct tags. Expansion happens once, when Load, Watch, or Doctor walks the
// config struct, before any ref is parsed, so a variable may supply a scheme, a
// path segment, a fragment, or a query value.
//
// Nothing is expanded unless it appears here. mamori never reads the ambient
// environment for this, and that is a deliberate security property rather than
// an omission: a ref decides which secret a process reads, so expanding one
// from ambient state would let anything able to set an environment variable
// redirect that read. This is the same reasoning that makes the exec: provider
// opt-in via WithExecProvider. Use EnvVars to opt in to named environment
// variables explicitly.
//
// Applying WithRefVars more than once merges, with later calls winning per key.
// (WithAuth rejects a second application instead, because "which authenticator
// wins" has two plausible answers while merging maps has one.)
//
// Values must not be secrets. After expansion a ref's Raw holds the expanded
// string, which appears in Status, Report, and mamori doctor output. Variables
// are for environment names, regions, service names, and tenant identifiers.
func WithRefVars(vars map[string]string) Option {
	return func(o *options) {
		if o.refVars == nil {
			o.refVars = make(map[string]string, len(vars))
		}
		for k, v := range vars {
			o.refVars[k] = v
		}
	}
}

// EnvVars reads the named environment variables into a map suitable for
// WithRefVars:
//
//	mamori.WithRefVars(mamori.EnvVars("ENVIRONMENT", "REGION"))
//
// Naming each variable is the point: it keeps the set of things that can
// influence which secret a process reads enumerable and greppable at the call
// site, rather than "any environment variable at all".
//
// A name that is not set in the environment is omitted from the result rather
// than mapped to the empty string, so expansion reports the undefined-variable
// error instead of silently producing a ref with a hole in it.
func EnvVars(names ...string) map[string]string {
	out := make(map[string]string, len(names))
	for _, n := range names {
		if v, ok := os.LookupEnv(n); ok {
			out[n] = v
		}
	}
	return out
}

// expandRefVars substitutes ${VAR} references in a raw `source` tag.
//
// Only the braced form is recognized. A bare $VAR is left untouched, so
// passwords, exec: commands, and paths containing '$' pass through unchanged.
// "$$" is a literal '$'. An unterminated "${" and an undefined variable are
// both errors: expanding either to nothing would yield a ref like
// "aws-sm:///db", which resolves not-found and then quietly takes the field's
// default:, turning a deployment misconfiguration into a silently wrong value.
func expandRefVars(tag string, vars map[string]string) (string, error) {
	if !strings.Contains(tag, "$") {
		return tag, nil
	}
	var b strings.Builder
	b.Grow(len(tag))
	for i := 0; i < len(tag); i++ {
		if tag[i] != '$' {
			b.WriteByte(tag[i])
			continue
		}
		if i+1 < len(tag) && tag[i+1] == '$' {
			b.WriteByte('$')
			i++
			continue
		}
		if i+1 >= len(tag) || tag[i+1] != '{' {
			b.WriteByte('$')
			continue
		}
		end := strings.IndexByte(tag[i+2:], '}')
		if end < 0 {
			return "", fmt.Errorf("mamori: source %q: unterminated ${: %w", tag, ErrInvalid)
		}
		name := tag[i+2 : i+2+end]
		v, ok := vars[name]
		if !ok {
			return "", fmt.Errorf("mamori: source %q: undefined ref variable %q (pass it with WithRefVars): %w", tag, name, ErrInvalid)
		}
		b.WriteString(v)
		i += 2 + end
	}
	return b.String(), nil
}
```

- [ ] **Step 4: Add `refVars` to `options`**

In `reconcile.go`, add to the `options` struct after `historyN`:

```go
	refVars      map[string]string // ${VAR} expansion for source tags; nil means none
```

Leave `defaultOptions` alone: a nil map reads correctly and `WithRefVars` allocates on first use.

- [ ] **Step 5: Thread `vars` through the spec walk**

In `decode.go`:

```go
func fieldSpecs(t reflect.Type, vars map[string]string) ([]fieldSpec, error) {
	if t.Kind() != reflect.Struct {
		return nil, fmt.Errorf("mamori: config type %s is not a struct", t)
	}
	return walkSpecs(t, "", nil, vars)
}

func walkSpecs(t reflect.Type, prefix string, index []int, vars map[string]string) ([]fieldSpec, error) {
```

Update the recursive call to `walkSpecs(f.Type, path, idx, vars)`.

Immediately before `refs, err := ParseRefs(source)`, expand:

```go
		source, err := expandRefVars(source, vars)
		if err != nil {
			return nil, fmt.Errorf("mamori: field %s: %w", path, err)
		}
```

Note the shadowing: `source` is currently declared by `source, hasSource := f.Tag.Lookup("source")`, so use `=` with a separate `err` declaration or restructure to avoid a redeclaration error. Compile before moving on.

- [ ] **Step 6: Update the three call sites**

- `reconcile.go:171`: `specs, err := fieldSpecs(t, o.refVars)`
- `doctor.go:29`: `specs, err := fieldSpecs(reflect.TypeOf(cfg), o.refVars)`
- `decode_test.go`: all 6 call sites take a trailing `, nil`.

- [ ] **Step 7: Run to verify pass**

Run: `go test -race -run 'TestExpandRefVars|TestWithRefVars|TestEnvVars|TestFieldSpecs' . -v`
Expected: PASS.

- [ ] **Step 8: Run the full suite, add the stack layer, and commit**

```bash
go test -race ./...
gh stack add xavier/ref-grammar-interpolation
git add refvars.go refvars_test.go decode.go decode_test.go reconcile.go doctor.go
git commit -m "feat: \${VAR} ref interpolation via WithRefVars

Source tags may now carry \${VAR} references, expanded once at spec-walk time
before any ref is parsed, so a variable can supply a scheme, a path segment, a
fragment, or a query value.

Variables come only from WithRefVars, never the ambient environment. A ref
decides which secret a process reads, so expanding one from ambient state would
let anything able to set an environment variable redirect that read; this is
the same reasoning that makes exec: opt-in. EnvVars(names...) opts in to named
environment variables explicitly, omitting any that are unset so an undefined
variable errors rather than silently expanding to nothing.

An undefined variable and an unterminated \${ both fail at Load/Watch/Doctor.
Expanding either to nothing would yield aws-sm:///db, which resolves not-found
and then quietly takes the field's default:.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

## Task 10: Fuzz the ref grammar

**Files:**
- Create: `fuzz_test.go`

**Interfaces:**
- Consumes: `ParseRefs`, `SelectKey`, `expandRefVars`, `parseDecodePipeline`.
- Produces: `FuzzRefGrammar`.

`ParseRef` is hand-rolled parsing, and this plan makes the grammar meaningfully more complex. The repo has no fuzzing today.

- [ ] **Step 1: Write the fuzz target**

```go
package mamori

import (
	"strings"
	"testing"
)

// FuzzRefGrammar exercises the hand-rolled parsing across the whole grammar.
// The properties asserted are the ones a parser must never violate regardless
// of input: it must not panic, and it must not return a value together with a
// non-nil error.
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
	}
	for _, s := range seeds {
		f.Add(s, []byte(`{"a":{"b":[1,2,"three"]}}`))
	}

	f.Fuzz(func(t *testing.T, tag string, payload []byte) {
		expanded, err := expandRefVars(tag, map[string]string{"ENV": "prod"})
		if err != nil {
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
			if _, err := parseDecodePipeline(r.Opt("decode")); err != nil {
				continue
			}
			got, err := SelectKey(payload, r.Key)
			if err != nil && got != nil {
				t.Fatalf("SelectKey(%q) returned both bytes and an error %v", r.Key, err)
			}
			// A ref that parsed must render back to something parseable.
			if _, err := ParseRef(r.String()); err != nil && !strings.HasPrefix(r.String(), "") {
				t.Fatalf("Ref.String() %q does not re-parse: %v", r.String(), err)
			}
		}
	})
}
```

- [ ] **Step 2: Run the fuzzer briefly**

Run: `go test -run FuzzRefGrammar -fuzz FuzzRefGrammar -fuzztime 60s .`
Expected: no crashes in 60 seconds. If it finds one, that is a real bug: fix it, and commit the generated `testdata/fuzz/` corpus entry alongside the fix.

- [ ] **Step 3: Run as a normal test and commit**

Run: `go test -race -run FuzzRefGrammar .`
Expected: PASS (seeds only).

```bash
git add fuzz_test.go testdata/
git commit -m "test: fuzz the ref grammar

ParseRef is hand-rolled parsing and this stack made the grammar meaningfully
larger (pointer fragments, a decode pipeline, variable expansion). Asserts the
two properties a parser must never violate on any input: no panic, and never a
value together with a non-nil error.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

## Task 11: Documentation for interpolation, and grammar page completion

**Files:**
- Modify: `site/src/pages/docs/concepts/ref-grammar.md`, `site/src/pages/docs/usage/index.md`, `site/src/pages/docs/observability/admin.md`, `README.md`, `skills/mamori/SKILL.md`, `doc.go`

- [ ] **Step 1: Fill in the interpolation section of the grammar page**

Cover: the `${VAR}` form and that bare `$VAR` is untouched; `$$`; that expansion runs once at spec-walk time before parsing, so a variable can supply any part of a ref including a scheme; the `WithRefVars`-only rule with its security rationale stated plainly; `EnvVars` as the explicit opt-in; the undefined-variable and unterminated-`${` errors with their real error text; and the chain-splitting note from spec section 7.5.

- [ ] **Step 2: Add the secrets warning where it will actually be read**

In the grammar page and in `site/src/pages/docs/observability/admin.md`, as a callout using whatever admonition syntax the site already uses:

> `WithRefVars` values must not be secrets. After expansion a ref's `Raw` holds the expanded string, and refs appear in `Status()`, in the admin endpoint's `Report`, and in `mamori doctor` output. Variables are for environment names, regions, service names, and tenant identifiers.

Check `grep -rn ':::' site/src/pages/docs/ | head` for the existing callout syntax before writing it.

- [ ] **Step 3: Update the remaining files**

- `README.md`: an interpolation bullet, and a `WithRefVars` line in the `Watch` example.
- `site/src/pages/docs/usage/index.md`: interpolation in the field-tag walkthrough.
- `skills/mamori/SKILL.md`: the `${VAR}` form and the `WithRefVars`-only rule, so agents do not suggest env-based expansion.
- `doc.go`: complete the grammar summary with all three additions.

- [ ] **Step 4: Final verification**

```bash
go test -race ./...
go vet ./...
golangci-lint run
cd site && npm run build && cd ..
for d in providers/*/ server/ cmd/mamori/ x/*/; do (cd "$d" && go test -race ./... 2>&1 | tail -2); done
```
Expected: all PASS.

- [ ] **Step 5: Commit and submit the complete stack**

```bash
git add -A
git commit -m "docs: \${VAR} ref interpolation

Completes concepts/ref-grammar.md with all three additions from the spec, and
adds the 'WithRefVars values must not be secrets' callout to both the grammar
page and the admin endpoint docs, since expanded refs appear in Status and
doctor output.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"

gh stack submit
gh stack view
```

- [ ] **Step 6: Verify the stack topology on GitHub**

Run: `gh stack view`
Expected, bottom to top:

```
main
 └─ xavier/new-features-2              PR 1  ○ open   docs: spec
     └─ xavier/ref-grammar-pointer     PR 2  ○ open   feat: JSON Pointer selection
         └─ xavier/ref-grammar-decode  PR 3  ○ open   feat: ?decode=
             └─ xavier/ref-grammar-interpolation  PR 4  ○ open   feat: ${VAR}
```

Every PR's base must be the branch below it, never `main` except for PR 1. If
any row shows `⚠ Needs rebase`, run `gh stack rebase` before asking for review.

---

## Self-Review

**Spec coverage:**

| Spec section | Task |
|---|---|
| 5.1 pointer grammar | 1 |
| 5.2 array indices, zero-based, leading zeros, `-` | 1 (`arrayIndex`), tested in Task 1 Step 1 |
| 5.3 error table | 1, `TestSelectPointerErrors` covers every row |
| 5.4 return semantics | 1 (`unquoteJSON`, shared by both paths) |
| 5.5 blast radius | 2 (conformance across all modules) |
| 6.1 grammar and coding table | 4 |
| 6.2 semantics, Version untouched | 4 (`TestApplyDecodePreservesMetadata`) |
| 6.3 three call sites | 6 |
| 6.4 redaction (no denylist change) | no code needed; documented in Task 8 |
| 7.1 grammar, `$$`, bare `$` | 9 |
| 7.2 API | 9 |
| 7.3 failure | 9 (`TestExpandRefVarsUndefined`) |
| 7.4 visibility and the secrets warning | 9 (Raw), 11 (warning) |
| 7.5 chains | 9 (chain case in `TestExpandRefVars`), documented in 11 |
| 8 testing, incl. fuzz | 1, 4, 6, 9, 10 |
| 9 documentation, 11 files | 3, 8, 11 |
| 10 delivery, the stack | branch and PR steps in 1, 3, 4, 8, 9, 11 |

Every spec section maps to a task. Spec section 6.4 needs no code, which is stated rather than left as a gap.

**Placeholder scan:** No TBD or TODO. The one deliberate placeholder, the empty `?decode=` and `${VAR}` sections created in Task 3 Step 2, is filled by Tasks 8 and 11 and is explicitly required to be a real heading with a sentence rather than a TODO marker. Task 6 Step 1 and Task 2 Step 2 instruct reading the real `mamoritest` and `providertest` APIs before writing against them rather than trusting the shapes shown, which is honest about what was not verified rather than a placeholder.

**Type consistency:** `applyDecode(Ref, Value) (Value, error)` is defined in Task 4 and called with that signature in Task 6 (three sites) and Task 10. `parseDecodePipeline(string) ([]decodeStep, error)` is defined in Task 4 and called in Tasks 5 and 10. `selectPointer(data []byte, ptr string)` is defined and called only in Task 1. `expandRefVars(string, map[string]string) (string, error)` is defined in Task 9 and called in Tasks 9 and 10. `fieldSpecs` gains its second parameter in Task 9 and every one of its 9 call sites (3 production, 6 test) is updated in the same task. `unquoteJSON` is introduced in Task 1 and used by both `SelectKey` and `selectPointer` in that same task.
