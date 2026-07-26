package mamoriprov

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/xavidop/mamori"
)

// batchRequest is POST /v1/values's body: the names to resolve, in the order
// the caller wants them answered. It mirrors server/wire.go's batchRequest
// field-for-field.
type batchRequest struct {
	Names []string `json:"names"`
}

// batchResponse is POST /v1/values's body: one valueBody per requested name,
// each independently either a resolved value or an Error entry. It mirrors
// server/wire.go's batchResponse field-for-field.
type batchResponse struct {
	Values []valueBody `json:"values"`
}

// ResolveBatch implements mamori.BatchProvider by issuing a single POST
// /v1/values for every ref in refs, keyed by ref.Path (the binding name; see
// Resolve's doc comment for why a mamori:// ref's path is always a binding
// name, never an upstream ref).
//
// A non-200 response is treated as a whole-request failure (malformed body,
// auth failure before any name was even looked at) and reported as a single
// classified error for the entire call - never a per-name outcome.
//
// On 200, each response entry is matched back to its input ref by name
// (valueBody.Name), not by position, to stay robust against a server that
// does not preserve order. An entry with no error becomes a map entry keyed
// by the input ref's String() (its Raw tag). An entry with error.kind
// "not_found" is OMITTED from the map (mamori applies its own default), as
// is any requested name absent from the response entirely. Any OTHER error
// kind (permission_denied, unavailable, rate_limited, invalid,
// unauthenticated, unknown) is a hard per-name failure with no
// last-known-good value, and fails the WHOLE ResolveBatch call: silently
// dropping it would let a consumer apply a default in place of a secret it
// is not allowed to read, or one that is genuinely unavailable, which is not
// what a per-ref Resolve of that same name would do.
func (p *Provider) ResolveBatch(ctx context.Context, refs []mamori.Ref) (map[string]mamori.Value, error) {
	names := make([]string, len(refs))
	// keyByName maps a requested binding name back to its input ref's
	// String() so a response entry, matched by name, can be keyed correctly
	// in the result map even if the server does not preserve request order.
	keyByName := make(map[string]string, len(refs))
	for i, ref := range refs {
		if ref.Path == "" {
			return nil, fmt.Errorf("%w: mamori:// ref %q has no binding name", mamori.ErrInvalid, ref.Raw)
		}
		names[i] = ref.Path
		keyByName[ref.Path] = ref.String()
	}

	reqBody, err := json.Marshal(batchRequest{Names: names})
	if err != nil {
		return nil, fmt.Errorf("%w: encoding mamori batch request: %s", mamori.ErrInvalid, err)
	}

	resp, err := p.do(ctx, http.MethodPost, "/v1/values", bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		limited := io.LimitReader(resp.Body, errBodyLimit)
		var env errorEnvelope
		if err := json.NewDecoder(limited).Decode(&env); err != nil {
			return nil, fmt.Errorf("mamori: batch request returned status %d with an undecodable body: %s", resp.StatusCode, err)
		}
		if sentinel := sentinelForKind(env.Error.Kind); sentinel != nil {
			return nil, fmt.Errorf("%w: %s", sentinel, env.Error.Message)
		}
		return nil, fmt.Errorf("mamori: batch request returned status %d kind %q: %s", resp.StatusCode, env.Error.Kind, env.Error.Message)
	}

	var br batchResponse
	if err := json.NewDecoder(resp.Body).Decode(&br); err != nil {
		return nil, fmt.Errorf("%w: decoding mamori batch response: %s", mamori.ErrInvalid, err)
	}

	result := make(map[string]mamori.Value, len(refs))
	for _, entry := range br.Values {
		key, known := keyByName[entry.Name]
		if !known {
			// Not one of the names we asked about; ignore.
			continue
		}

		if entry.Error == nil {
			var notAfter time.Time
			if entry.NotAfter != nil {
				notAfter = *entry.NotAfter
			}
			result[key] = mamori.Value{
				Bytes:     entry.Bytes,
				Version:   entry.Version,
				Sensitive: entry.Sensitive,
				NotAfter:  notAfter,
				Metadata:  entry.Metadata,
			}
			continue
		}

		if mamori.Kind(entry.Error.Kind) == mamori.KindNotFound {
			// Omitted: mamori applies the default for a missing entry.
			continue
		}

		if sentinel := sentinelForKind(entry.Error.Kind); sentinel != nil {
			return nil, fmt.Errorf("%w: %s (name %q)", sentinel, entry.Error.Message, entry.Name)
		}
		return nil, fmt.Errorf("mamori: batch entry %q failed with kind %q: %s", entry.Name, entry.Error.Kind, entry.Error.Message)
	}

	return result, nil
}
