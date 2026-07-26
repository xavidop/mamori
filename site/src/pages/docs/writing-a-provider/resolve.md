---
layout: ../../../layouts/DocsLayout.astro
title: Resolve and errors
---

# Resolve and errors

`Resolve` returns the current `mamori.Value` for a `Ref` and classifies every failure. Get this right and your provider is interchangeable with every other one.

## Implement Resolve

A `Ref` gives you `Path` (the backend location), `Key` (the `#key` fragment, empty when absent), and `Opts` (query options):

```go
func (p *Provider) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
	raw, err := p.client.get(ctx, ref.Path)
	if isNotFound(err) {
		return mamori.Value{}, mamori.ErrNotFound
	}
	if err != nil {
		return mamori.Value{}, err
	}
	if ref.Key != "" { // #key selects from a JSON payload, identically everywhere
		raw, err = mamori.SelectKey(raw, ref.Key)
		if err != nil {
			return mamori.Value{}, err
		}
	}
	return mamori.Value{
		Bytes:     raw,
		Version:   mamori.VersionHash(raw), // or a native backend revision
		Sensitive: true,                    // true for secret managers
	}, nil
}
```

Rules that keep providers interchangeable:

- Missing values: return an error satisfying `errors.Is(err, mamori.ErrNotFound)`, never a nil error with empty bytes.
- Set `Value.Version` from a native revision or `mamori.VersionHash(bytes)`. It must change whenever the value changes; that is what mamori uses for change detection.
- Use `mamori.SelectKey(payload, ref.Key)` for `#key` selection.
- Set `Sensitive: true` on secret-bearing values; never log the payload.
- Honor `ctx` in every network call.

## Map backend errors to kinds

`ErrNotFound` is the only error that changes mamori's behavior (it triggers `default:` and `optional` handling). Classify every other failure too, so telemetry (`mamori.error.kind`) and callers using `mamori.ErrorKind` can tell an operator what went wrong.

Wrap the SDK error with the matching sentinel using **two `%w` verbs, sentinel first**:

```go
var ae smithy.APIError
if errors.As(err, &ae) {
	switch ae.ErrorCode() {
	case "ResourceNotFoundException":
		return mamori.Value{}, fmt.Errorf("%w: %w", mamori.ErrNotFound, err)
	case "AccessDeniedException":
		return mamori.Value{}, fmt.Errorf("%w: %w", mamori.ErrPermissionDenied, err)
	case "ThrottlingException":
		return mamori.Value{}, fmt.Errorf("%w: %w", mamori.ErrRateLimited, err)
	}
}
return mamori.Value{}, err // unmapped: reports as unknown, which is fine
```

Two `%w` verbs keep `errors.Is(err, mamori.ErrPermissionDenied)` matching the sentinel while `errors.As` can still reach the original SDK error type.

**Never use `%v` for the sentinel.** It flattens the sentinel into a string and breaks the chain:

```go
// WRONG: %v flattens the sentinel into a string and destroys the chain.
return mamori.Value{}, fmt.Errorf("secretsmanager: %v", mamori.ErrPermissionDenied)
```

This still compiles and reads fine, but `errors.Is` no longer matches and every failure reports as `unknown`. The `ErrorClassification` [conformance case](/docs/writing-a-provider/conformance/) exists to catch exactly this.

### Which kind to use

Map each failure to the sentinel that names its cause. Leaving an error unmapped (reported as `unknown`) is fine; guessing is not.

| Kind | Use for |
|---|---|
| `ErrNotFound` | Key, secret, path, or version genuinely absent |
| `ErrPermissionDenied` | Authenticated but not authorized: IAM deny, Vault policy, RBAC |
| `ErrUnauthenticated` | Missing, malformed, or expired credentials; failed token renewal |
| `ErrUnavailable` | Network failure, DNS, timeout, 5xx, circuit open |
| `ErrRateLimited` | Throttling, quota exhaustion, 429 |
| `ErrInvalid` | The ref is malformed for this provider, or the payload cannot be parsed |
| (unmapped) | Anything else. Reports as `unknown`, an honest answer. |
| *(automatic)* | `context.DeadlineExceeded` is classified as `unavailable` by `ErrorKind`; you need not map it. A plain `context.Canceled` reports `unknown`, since the caller withdrew the request. |

See [Error kinds](/docs/concepts/error-kinds/) for how consumers read `Kind`.
