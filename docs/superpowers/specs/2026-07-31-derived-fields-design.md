# Derived fields: `WithDerive`

**Status:** approved
**Date:** 2026-07-31

Adds `WithDerive`, a typed hook that computes fields from already-resolved
fields, running on every `Load` and every reconciled update.

Core change. It hooks the resolve pipeline, which nothing outside the core can
reach.

## The problem

Assembling a value from several resolved fields is routine, and a DSN is the
canonical case:

```go
dsn := fmt.Sprintf("postgres://%s:%s@%s/%s", cfg.User, cfg.Password.Reveal(), cfg.Host, cfg.DB)
```

Today that line lives in application code, after `Get()`. Which means it runs
**once**, at wiring time, and mamori's entire reason for existing does not
reach it: when the password rotates, `Get()` returns a new snapshot with a new
password and the DSN the application built an hour ago is silently wrong.

The caller can fix this by rebuilding the DSN inside every `OnChange`, but that
is exactly the hand-rolled reconciliation mamori exists to delete, and it is
the kind of thing that is remembered on the first field and forgotten on the
fourth.

## Shape

```go
func WithDerive[T any](fn func(*T) error) Option
```

```go
w, err := mamori.Watch[Config](ctx,
    mamori.WithDerive(func(c *Config) error {
        c.DSN = secret.NewString((&url.URL{
            Scheme: "postgres",
            User:   url.UserPassword(c.User, c.Password.Reveal()),
            Host:   c.Host,
            Path:   "/" + c.DB,
        }).String())
        return nil
    }),
)
```

### Why a function and not a `derive:` tag

A tag reads more naturally and would be visible to `explain` and `schema`, but
it drags in two mechanisms this design gets to skip entirely:

- **Escaping.** A password containing `@`, `/`, or `:` silently produces a
  broken DSN. A tag would need escape modifiers, and mamori would own a
  template language. Here, escaping is `net/url`'s problem, which has already
  solved it correctly.
- **Sensitivity.** The assembled DSN contains the password. A tag assigning it
  into a plain `string` would copy a secret into a field with no redaction, so
  it would surface in logs, `fmt`, and JSON. Preventing that means inferring
  sensitivity from inputs and rejecting at parse time. Here the caller writes
  `secret.String` and the type system carries it.

Neither mechanism is deferred. Both are structurally unnecessary.

### Why no `context.Context`

`PreApply` is `func(ctx context.Context, ev Change[T]) error` because its job
is I/O: proving a credential against a real dependency. A derive is a pure
transformation of an already-resolved struct. Omitting the context is how the
API says which of those two things it is, and the asymmetry is deliberate
rather than an oversight.

## Where it runs

```text
resolve refs -> apply defaults -> DERIVE -> validate -> PreApply -> swap in
```

Two positions in that line are load-bearing:

- **Before validation.** So `validate:"required,url"` on a derived field
  checks the assembled value rather than the zero value it had a moment
  earlier. A derived field that could not be validated would be the only
  unvalidated field in the struct.
- **Before `PreApply`.** So a rotation-safety hook can `Ping` using the
  *derived* DSN. This is the motivating case end to end: the password rotates,
  the DSN is rebuilt, and `PreApply` proves the rebuilt DSN works before
  `Get()` serves it. Running derive after `PreApply` would prove the previous
  one, which is worse than not proving anything, because it would look like it
  worked.

It runs on `Load` and on every reconciled update, on the same code path.

## Errors

A derive returning an error rejects the whole candidate snapshot, exactly as a
validation failure does: `Get()` keeps serving the last good config, and the
error reaches `OnError`.

Not "report and continue". A config whose derived fields were not built is not
a config anyone should serve, and half-applying one would produce a snapshot
where some fields reflect the new password and the DSN reflects the old.

## Composition

Multiple `WithDerive` calls run in **registration order**. Unrelated
derivations stay in separate functions rather than accreting into one closure,
and a field derived from another derived field works with no new concept: the
later function simply sees the earlier one's output.

## Type mismatch fails loudly

The generic hook is stored in the non-generic `options` struct as `any` and
asserted at use, the pattern `preApply` and `onChange` already use.

**Follow `preApply`, not `onChange`.** These two behave differently today, and
the difference is worth knowing before copying either:

```go
// preApply, reconciler.go: loud
fn, ok := o.preApply.(func(context.Context, Change[T]) error)
if !ok {
    return nil, fmt.Errorf("mamori: PreApply hook has type %T, want %T: %w", o.preApply, want, ErrInvalid)
}

// onChange, reconciler.go: silent
onChange, _ = o.onChange.(func(Change[T]))
```

A `WithOnChange[Foo]` passed to `Watch[Bar]` compiles and then silently never
runs. `WithDerive` must not inherit that: a mismatch is a programmer error, and
one that would otherwise present as "my DSN is empty and nothing told me why".
It returns an error naming both types and wrapping `ErrInvalid`.

The `onChange` behavior is pre-existing and out of scope here, but it should be
filed separately.

## What mamori cannot know

The derivation is opaque Go, so mamori does not know which fields it touches.
Three consequences, which belong where the feature is introduced rather than in
a footnote:

- **Invisible to `mamori explain`, `schema`, and `diff`.** Those read struct
  tags, and there is no tag. A derived field looks like an unsourced field to
  every CLI surface, whether or not its write is declared: all three are
  static analysis over Go source, and a `writes` declaration lives at the
  `Watch`/`Load` call site, not in the struct tags they read.
- **Absent from `Status()`'s per-field report, unless the write is declared.**
  There is no ref, so there is no staleness or error kind to report either way.
  This was a real gap in the first shipped version: an operator debugging a
  wrong DSN looked in `Status()` first and found nothing. See the change-detection
  section below for the `writes` declaration that closed it, and note what it
  still cannot close: an undeclared write is exactly as absent as before.
- **A `source` tag and a derive assignment on the same field cannot be flagged
  as a conflict.** The derive simply wins, because mamori cannot see the
  assignment. Documentation, not enforcement. `writes` does not change this:
  declaring a path only reports that path changed, it does not detect a
  conflicting `source` tag on it.
- **A derived field never appears in `ev.Changed()`, unless its write is
  declared.** Same root cause as the rest of this list: mamori cannot infer
  which fields an opaque function writes, so nothing keys a diff to it by
  default. See the change-detection section below for what shipped to fix
  this and why the first version of this spec rejected it.

These are the cost of choosing a function over a tag. They were accepted
knowingly; the docs should present them the same way.

## Change detection: the free-lunch claim, the real cost, and what shipped

An earlier revision of this spec claimed change detection came free. That was
wrong, and the error was found during implementation rather than by reading,
which is worth recording because it is the kind of claim that reads as
obviously true. This section originally recorded that correction; a second
round of implementation then revisited the decision it led to, which this
section now also records, so the history reads as what actually happened
rather than as though the spec were right the first time.

**As first shipped, `ev.Changed("DSN")` could never be true for a derived
field.** Both diff sites in `reconciler.go` (`buildCandidate`'s loop and
`diffApplied`) walked `e.specs`, which `fieldSpecs` populates only for fields
carrying a `source` tag, comparing the `Version` strings recorded per ref. A
derived field had no spec, no ref, and no version, so it could not enter
`Change.Fields` at all, regardless of where in the derive chain it ran.
Callers had to trigger on an input and read the derived field instead:

```go
mamori.OnChange(func(ev mamori.Change[Config]) {
    if ev.Changed("Pass") || ev.Changed("Host") || ev.Changed("User") {
        pool.Rotate(w.Get().DSN.Reveal())   // always correct, always rebuilt
    }
})
```

That was an honest cost, and it partly reopened the problem the feature exists
to solve: forget one input in that condition and the pool never rotates when
that field changes, the same bookkeeping `WithDerive` set out to delete.

Two alternatives were considered and rejected at the time. Having `WithDerive`
declare the field paths it writes would restore `ev.Changed("DSN")`, at the
cost of a list that can silently drift from the function it describes.
Diffing the finished structs reflectively would fix it with no API change, but
that changes core `Changed()` semantics for every field and all 38 providers,
and was judged to deserve its own spec rather than riding along here.

**The first alternative shipped, in a follow-up round rather than this one.**
`WithDerive[T any](fn func(*T) error, writes ...string) Option` now takes a
variadic `writes` parameter naming the dotted field paths the hook writes
(`WithDerive(buildDSN, "DSN")`). mamori carries that declaration through both
diff sites via `derivedFieldChanges`, the single implementation shared by the
candidate build, the coalesced `Unpin` diff, and the initial-load `Change`, and
into `Status()`'s per-field report as `FieldStatus{Path: "DSN", Derived: true}`.
A declared write is reported changed exactly when the rebuilt value differs
from the one it replaced, compared with `reflect.DeepEqual` rather than a
version string, since a derived field still has no ref and no version of its
own to compare.

The drift risk that got the declared-writes alternative rejected the first
time was real, and turned out to be worth accepting rather than designing
around: a wrong or missing declaration degrades to exactly the original
undeclared behavior (invisible to `ev.Changed` and `Status()`, never enforced
or rejected) rather than misbehaving, and `WithDerive(fn)` with zero paths
still compiles and behaves exactly as it always did, so no existing caller
broke. The reflective-diff alternative stayed rejected on its own merits: it
still changes every field's `Changed()` semantics, not only derived ones, and
still deserves its own spec if anyone revisits it.

`writes` does not close every gap this section opened with. A path is
validated for shape only (non-empty, non-whitespace) at `Load`/`Watch` time,
never checked against `T`, and a hook that writes a field it never declares is
exactly as invisible as it was before `writes` existed - mamori still cannot
inspect the hook's body, only the paths it is told about. See
[Derived fields](https://mamorigo.dev/docs/usage/derived-fields) for the
user-facing account.

## Testing

| Aspect | How |
| --- | --- |
| The motivating case | a password rotation rebuilds the DSN; with the write declared (`WithDerive(fn, "DSN")`), `ev.Changed("DSN")` is true and `Status().Fields` carries a `Derived: true` entry for it; with no write declared, both stay exactly as invisible as before `writes` existed |
| Runs before validation | a derived field with `validate:"required"` passes when the derive fills it, and fails when the derive leaves it empty |
| Runs before PreApply | `PreApply` observes the derived value, asserted by capturing what the hook saw |
| Error rejects the snapshot | a derive returning an error leaves `Get()` on the previous config and fires `OnError` |
| Composition order | two derives run in registration order, and the second sees the first's output |
| Type mismatch | `WithDerive[Foo]` with `Watch[Bar]` returns an error wrapping `ErrInvalid` naming both types, rather than silently not running |
| Load path too | the same derive runs under `Load`, not only under `Watch` |
| Secret hygiene | a derived `secret.String` redacts in `fmt`, JSON, and `slog`, and a derive that reveals a password into a plain `string` field is the caller's choice, documented but not prevented |

The type-mismatch test is the one most worth writing carefully. It is the
difference between this feature and `onChange`, and without it a future
refactor could quietly converge them.

## Documentation

A `usage/derived-fields.md` page leading with the rotation problem rather than
the API, the option in `usage/options.md`, and a note in `usage/rotation.md`
connecting derive to `PreApply`, since together they are what makes a rotated
credential both rebuilt and proven before it goes live.
