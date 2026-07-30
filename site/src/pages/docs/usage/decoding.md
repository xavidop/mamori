---
layout: ../../../layouts/DocsLayout.astro
title: Value decoding
---

# Value decoding

Add `?decode=` when a backend stores a value encoded, and mamori decodes it
before your field ever sees it.

```go
type Config struct {
	// stored as base64 text, wanted as raw bytes
	TLSKey []byte `source:"aws-sm://prod/tls#key?decode=base64"`

	// stored as base64 of a gzip stream
	Bundle []byte `source:"aws-sm://prod/bundle?decode=base64,gzip"`
}
```

## Codings

| Coding | Does |
| --- | --- |
| `base64` | standard alphabet, padded |
| `base64url` | URL-safe alphabet |
| `hex` | hex decode |
| `gzip` | decompress, output capped at 16 MiB |
| `trim` | strip leading and trailing whitespace |

This is a closed set. For anything else, use
[`WithDecodeHook`](/docs/usage/), which converts per Go type one layer down.

`base64`, `base64url`, and `hex` trim surrounding whitespace before decoding,
because a trailing newline from `base64 < key.pem` is common and failing a
secret over an invisible byte is miserable to debug. **`gzip` does not trim**:
its payload is binary, and a valid stream can legitimately end in bytes whose
values happen to be ASCII whitespace. If a gzip payload really is padded, say
so explicitly with `?decode=trim,gzip`.

## Order reads left to right

`?decode=base64,gzip` means the stored value is base64 **of** gzip **of** the
payload, so it is base64-decoded first, then gunzipped. Outermost wrapper
first, exactly as written.

## Failures are loud

A malformed payload, a truncated gzip stream, or a gzip result over 16 MiB
fails with `ErrInvalid`. There is no fallback to the raw bytes: handing your
application a still-encoded or silently truncated secret would fail later,
somewhere else, looking like anything but a decode problem.

A typo in the coding name (`?decode=base64,gzp`) is caught at `Load`, `Watch`,
or `Doctor` time rather than on a later poll. Every ref in a
[precedence chain](/docs/concepts/source-chains/) is checked, not just the one
that wins, so a typo in a fallback ref cannot lie dormant.

## Decoding runs after `#key` selection

`#tls.crt?decode=base64` means "select `tls.crt`, then decode what was
selected".

So a fragment cannot look inside what decoding produces. If the JSON you want
to select from only exists after decoding, drop the fragment and let
`flatten:"json"` do the selection instead:

```go
type Config struct {
	// The whole entry is base64. It decodes to a JSON object, and
	// flatten:"json" does the selection a #key could not.
	Creds Creds `source:"aws-sm://prod/bundle?decode=base64" flatten:"json"`
}
```

## Things worth knowing

**`default:` is used as-is, undecoded.** A default is a literal you wrote in
the tag, not an encoded payload, so write it already decoded.

**`Version` is untouched.** Decoding only changes the bytes, so change
detection keeps comparing the provider's own revision.

**`?decode=` is not redacted** from `Status()` or `mamori doctor`. An operator
debugging a garbled value needs to see a decode step is in play, and the option
is not itself sensitive.

**It belongs on your ref, not on a server binding.** Decoding happens in the
process that loads the config, so put it in your own `source` tag, including on
a `mamori://name` ref. A [config server](/docs/server/) binding rejects the
option rather than serving still-encoded bytes.

## See also

- [Ref grammar](/docs/concepts/ref-grammar/) - where `?decode=` sits in a tag.
- [JSON Pointer selection](/docs/concepts/json-pointer/) - the `#key` step that runs first.
- [Error kinds](/docs/concepts/error-kinds/) - how `ErrInvalid` surfaces.
