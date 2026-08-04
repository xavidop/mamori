package supabase

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providers/httpcore"
)

// secretRow is one row of the operator's relation, which mirrors
// vault.decrypted_secrets.
//
// DecryptedSecret is a pointer so that a row carrying no decrypted_secret
// column at all is distinguishable from one whose secret is the empty string,
// which is a value a ref may genuinely resolve to. The first is a relation
// built without that column and must fail; the second must succeed.
type secretRow struct {
	Name            string  `json:"name"`
	DecryptedSecret *string `json:"decrypted_secret"`
	UpdatedAt       string  `json:"updated_at"`
}

// Resolve fetches ref's secret from the project's PostgREST Data API.
//
// One ref is one GET of the operator's relation, filtered to a single row:
//
//	GET /rest/v1/<view>?name=eq.<name>&select=name,decrypted_secret,updated_at
//	apikey: <service-role key>
//	Authorization: Bearer <service-role key>
//	Accept-Profile: <schema>
//
// PostgREST answers a filtered select with a JSON ARRAY, even for one row, and
// an absent row is an EMPTY array with status 200 rather than a 404. Mapping
// that empty array onto mamori.ErrNotFound is the most consequential line in
// this package: ErrNotFound is the one kind that makes a field's default: and
// optional handling apply, so classifying it as anything else turns an absent
// optional secret into a hard startup failure.
//
// There is deliberately no cache: mamori.Refresh and mamori.Doctor both call
// Resolve directly, and PostgREST exposes no ETag this provider could gate a
// held snapshot on, so every call is a live read of the current value.
//
// ref.Key is handed to mamori.SelectKey, so a fragment is either an RFC 6901
// JSON Pointer or a literal top-level key, identically to every other provider.
func (p *Provider) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
	name, err := secretNameOf(ref)
	if err != nil {
		return mamori.Value{}, err
	}
	s, err := p.settingsFor(ref)
	if err != nil {
		return mamori.Value{}, err
	}
	client, err := p.clientFor()
	if err != nil {
		return mamori.Value{}, err
	}

	// select= names the three columns this provider reads rather than
	// defaulting to "*". Vault's view also carries the raw ciphertext, the key
	// id and the nonce, and none of them belongs in a response this process
	// holds in memory when it needs one column of it.
	q := url.Values{
		nameColumn: {"eq." + filterValue(name)},
		"select":   {strings.Join([]string{nameColumn, secretColumn, versionColumn}, ",")},
	}

	resp, err := client.Do(ctx, httpcore.Request{
		Path:  "/" + s.view,
		Query: q,
		// Accept-Profile is how PostgREST is told which schema to read, and it
		// is sent ALWAYS rather than only for a non-default schema. PostgREST
		// picks the FIRST entry of db-schemas as its default, so a project that
		// exposes "api,public" would silently read a different schema than one
		// exposing "public,api". Naming the schema on every request makes a ref
		// mean the same thing on both.
		Header: map[string][]string{"Accept-Profile": {s.schema}},
	})
	if err != nil {
		// One %w: the cause already carries the sentinel httpcore classified it
		// with, and adding a second would replace that kind rather than
		// duplicate it.
		return mamori.Value{}, fmt.Errorf("mamori/supabase: reading secret %q: %w", name, err)
	}

	var rows []secretRow
	if err := json.Unmarshal(resp.Body, &rows); err != nil {
		// The decode error is dropped rather than wrapped. encoding/json quotes
		// the offending byte in a syntax error, and on the success path this
		// body contains the decrypted secret: no fragment of it may reach an
		// error string. The secret name is enough to act on.
		return mamori.Value{}, fmt.Errorf("mamori/supabase: secret %q response is not a JSON array: %w", name, mamori.ErrInvalid)
	}

	switch {
	case len(rows) == 0:
		// The load-bearing mapping. PostgREST reports "no such row" as 200 with
		// [], never as a 404, so without this line an absent secret would look
		// like a successful read of nothing.
		return mamori.Value{}, fmt.Errorf("mamori/supabase: secret %q not found in %s.%s: %w",
			name, s.schema, s.view, mamori.ErrNotFound)
	case len(rows) > 1:
		// vault.secrets keeps name unique, so more than one row means the
		// operator's relation is not the one-row-per-name view this provider
		// documents. That is a misconfiguration, so it is ErrInvalid and
		// explicitly NOT ErrNotFound: silently taking rows[0] would resolve a
		// field to whichever row the planner happened to return first.
		return mamori.Value{}, fmt.Errorf("mamori/supabase: secret %q matched %d rows in %s.%s; the relation must have one row per name: %w",
			name, len(rows), s.schema, s.view, mamori.ErrInvalid)
	}

	row := rows[0]
	if row.DecryptedSecret == nil {
		// A relation built without the decrypted_secret column, or one whose
		// column is NULL. ErrInvalid rather than ErrNotFound: the row exists,
		// so applying the field's default would hide a broken relation behind a
		// value that looks deliberate.
		return mamori.Value{}, fmt.Errorf("mamori/supabase: secret %q row in %s.%s carried no %q column; see the provider README's setup SQL: %w",
			name, s.schema, s.view, secretColumn, mamori.ErrInvalid)
	}

	value := []byte(*row.DecryptedSecret)
	body, err := mamori.SelectKey(value, ref.Key)
	if err != nil {
		return mamori.Value{}, fmt.Errorf("mamori/supabase: secret %q: %w", name, err)
	}

	return mamori.Value{
		Bytes: body,
		// The version describes the whole secret, so it is derived from the
		// whole secret value rather than from the selected fragment: two refs
		// that select different #fields of one secret must agree on when it
		// changed.
		Version: versionOf(row.UpdatedAt, value),
		// Always true. This is a secret manager; there is no per-ref or
		// per-provider switch, because a relation that re-exposes
		// vault.decrypted_secrets has nothing non-secret in it.
		Sensitive: true,
	}, nil
}

// versionOf renders the row's updated_at as mamori's Version, falling back to a
// content hash when the relation supplied none.
//
// vault.decrypted_secrets offers two candidates and only one of them works.
//
//   - id is a UUID identifying WHICH secret. Vault's update_secret rewrites a
//     row in place, so the id is unchanged by a rotation: using it would pin
//     Version to a constant for the life of the secret and make change
//     detection impossible, which is the one failure mode a poller cannot
//     report, because nothing ever looks changed.
//   - updated_at is a microsecond-resolution timestamp that advances on every
//     write. It identifies WHEN the secret last changed, which is exactly what
//     a Version is for.
//
// So updated_at it is. Nothing is gained by combining the two: the ref already
// pins the name, and a secret deleted and recreated under that name gets a
// later updated_at anyway, so the id would never break a tie updated_at had not
// already broken.
//
// The hash fallback matters because an operator's relation may omit updated_at,
// and rendering an absent timestamp as "" would pin Version to a constant for
// every ref at once.
func versionOf(updatedAt string, value []byte) string {
	if updatedAt != "" {
		return updatedAt
	}
	return mamori.VersionHash(value)
}

// filterValue renders one PostgREST filter value, quoting it only when it needs
// quoting.
//
// PostgREST reserves ",", ".", ":", "(" and ")" inside a filter value and
// documents double quotes as the way to make a value containing one literal.
// Quoting unconditionally would work too, but a plain name would then reach the
// wire in a shape no vendor example shows, and this provider's wire format is
// documented rather than live-verified: the common path is worth keeping
// byte-identical to the documentation it was pinned from.
//
// Inside the quotes a literal double quote or backslash is backslash-escaped,
// so a name can contain either without terminating the quoted run.
func filterValue(name string) string {
	if !strings.ContainsAny(name, `,.:()"\ `) {
		return name
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + r.Replace(name) + `"`
}
