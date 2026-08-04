package heroku

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providers/httpcore"
)

// configVars is the shape GET /apps/{app_id_or_name}/config-vars answers with:
// a flat JSON object of names to values, with no envelope around it. Heroku's
// own published schema types a value as ["string","null"], not "string", so a
// value is a *string rather than a string. That is not defensive padding: null
// is how the PATCH form of this endpoint deletes a var, and decoding a null
// into a plain string field would resolve the ref to the empty string, which is
// a legal value a config var may genuinely hold. A pointer keeps "absent" and
// "empty" apart, and valueFor treats a null exactly like an absent name.
type configVars map[string]*string

// Resolve fetches one config var of one app.
//
// It costs one request that returns EVERY config var of the app, because that
// is the only read this endpoint offers. Resolving several refs of the same app
// through ResolveBatch instead collapses those requests into one; mamori does
// that automatically on the Load path.
//
// There is deliberately no cache: mamori.Refresh and mamori.Doctor both call
// Resolve directly, and a cache here would make those report a snapshot rather
// than the current value. httpcore.Revalidator is deliberately not used either
// - see the module README's "Why there is no conditional GET".
//
// ref.Key is handed to mamori.SelectKey, so a fragment is either an RFC 6901
// JSON Pointer or a literal top-level key, identically to every other provider.
//
// A ref path containing a "." or ".." segment is rejected with
// mamori.ErrInvalid. That check is not here: httpcore.Client.Do enforces it for
// every provider built on it, in both the literal and the percent-encoded form,
// so no provider can forget it. It matters more than usual for this grammar,
// where the FIRST path segment is interpolated straight into the request URL as
// the app identity.
func (p *Provider) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
	t, err := p.targetFor(ref)
	if err != nil {
		return mamori.Value{}, err
	}
	doc, err := p.fetch(ctx, t.app)
	if err != nil {
		return mamori.Value{}, err
	}
	return valueFor(doc, t, ref)
}

// ResolveBatch resolves every ref in one request per app.
//
// This is the whole reason this provider implements mamori.BatchProvider. The
// Platform API has no single-config-var endpoint: reading one var and reading
// all of them are the same request, so a config with twelve heroku:// fields
// against one app costs twelve identical full-document GETs through Resolve and
// exactly one through here. Heroku meters an account at 4500 requests per hour
// and answers 429 rate_limit past that, so the difference is not only latency.
//
// A ref whose config var is absent from its app's document is OMITTED from the
// result map rather than failing the batch, per the BatchProvider contract, so
// mamori applies that field's default. The same holds two other ways:
//
//   - A ref whose #field selection is absent from an otherwise-present JSON
//     value. mamori.SelectKey reports that as an error satisfying
//     mamori.ErrNotFound, and it is treated exactly like an absent name, which
//     is what Resolve does too. providers/vercel-gc shipped the opposite
//     behaviour and one missing optional field failed the whole batch, taking
//     every sibling ref down with it.
//   - A whole app that answers 404. On this endpoint a 404 can only mean the
//     app itself is absent or invisible to the token, never that one var is
//     missing, so its refs are omitted the same way rather than one
//     misconfigured app failing every sibling ref in the batch, in that app or
//     any other. providers/cloudflare-kv makes the same call one level down,
//     for a namespace that 404s.
//
// Every other failure still fails the batch. An ErrInvalid from selection (a
// #field of a value that is not a JSON object) is a malformed request against
// the payload rather than an absence, and a 401, 403, 429 or 5xx is a real
// failure that must not be silently rendered as "every field took its default".
//
// Apps are fetched in first-seen ref order, so the request sequence is
// deterministic and a test can assert it.
func (p *Provider) ResolveBatch(ctx context.Context, refs []mamori.Ref) (map[string]mamori.Value, error) {
	if len(refs) == 0 {
		return map[string]mamori.Value{}, nil
	}

	// One group per app. targets runs parallel to refs so the app and name each
	// ref resolved to are not recomputed after the fetch.
	type appGroup struct {
		refs    []mamori.Ref
		targets []target
	}
	groups := map[string]*appGroup{}
	order := make([]string, 0, len(refs))

	for _, r := range refs {
		t, err := p.targetFor(r)
		if err != nil {
			return nil, err
		}
		g, ok := groups[t.app]
		if !ok {
			g = &appGroup{}
			groups[t.app] = g
			order = append(order, t.app)
		}
		g.refs = append(g.refs, r)
		g.targets = append(g.targets, t)
	}

	out := make(map[string]mamori.Value, len(refs))
	for _, app := range order {
		g := groups[app]
		doc, err := p.fetch(ctx, app)
		if err != nil {
			if errors.Is(err, mamori.ErrNotFound) {
				// The app itself is absent or invisible to this token. Skip its
				// refs so each falls back to its own default, exactly as a
				// Resolve of the same ref would; failing here would lose every
				// sibling ref in the batch to one bad app name.
				continue
			}
			return nil, err
		}
		for i, r := range g.refs {
			v, err := valueFor(doc, g.targets[i], r)
			if err != nil {
				if errors.Is(err, mamori.ErrNotFound) {
					// An absent var, a null var, or an absent selected #field.
					// mamori applies the field's default.
					continue
				}
				return nil, err
			}
			out[r.Raw] = v
		}
	}
	return out, nil
}

// fetch issues the one documented read, GET /apps/{app_id_or_name}/config-vars,
// and decodes the whole document.
//
// The app identity is url.PathEscape'd. Heroku app names are lowercase
// alphanumeric with dashes, so escaping is normally a no-op, but the identity
// comes from a mamori ref and a ref path is not only what the struct tag says:
// ${VAR} interpolation substitutes values the application supplies at runtime.
// Escaping keeps whatever arrives as ONE path segment, and httpcore refuses a
// "." or ".." before anything is sent.
func (p *Provider) fetch(ctx context.Context, app string) (configVars, error) {
	client, err := p.clientFor()
	if err != nil {
		return nil, err
	}

	resp, err := client.Do(ctx, httpcore.Request{
		Path: appsPath + url.PathEscape(app) + configVarsSuffix,
		// The version header the API requires. It is set here, on the one
		// request this package makes, rather than left to a caller: there is no
		// path through this provider that can reach the wire without it.
		Header: http.Header{"Accept": {acceptVersion}},
	})
	if err != nil {
		// One %w: the cause already carries the sentinel httpcore classified it
		// with, and adding a second would replace that kind rather than
		// duplicate it.
		return nil, fmt.Errorf("mamori/heroku: reading config vars for app %q: %w", app, err)
	}

	var doc configVars
	if err := json.Unmarshal(resp.Body, &doc); err != nil {
		// The decode error is dropped rather than wrapped. encoding/json quotes
		// the offending byte in a syntax error, and on the success path this
		// body is the app's entire config var document: no fragment of it may
		// reach an error string. The app name and the status are enough to act
		// on.
		return nil, fmt.Errorf("mamori/heroku: config vars for app %q are not a JSON object: %w", app, mamori.ErrInvalid)
	}
	if doc == nil {
		// A body of literal "null" unmarshals into a nil map with no error, and
		// a nil map answers every lookup with "absent" - which would report
		// every ref of this app as not-found and have mamori silently apply
		// every default. A backend that answers 200 with "null" is
		// misbehaving, and that must not look like an empty app.
		return nil, fmt.Errorf("mamori/heroku: config vars for app %q decoded to null rather than an object: %w", app, mamori.ErrInvalid)
	}
	return doc, nil
}

// valueFor selects one config var out of an app's document and applies the
// ref's #key selection.
//
// A name that is absent, and a name whose value is JSON null, are the same
// answer: not found, so mamori applies the field's default. Heroku's schema
// types a config var value as ["string","null"] and null is how the PATCH form
// of this endpoint deletes one, so the two really are the same state seen at
// two moments.
//
// Value.Sensitive is unconditionally true. See the module README; briefly, this
// endpoint hands back every config var of an app in one document with no
// per-var classification, and add-ons write DATABASE_URL and REDIS_URL - live
// credentials, complete with password - into that same namespace without the
// operator typing them. There is no signal here that could distinguish
// LOG_LEVEL from DATABASE_URL, and the two mistakes are not symmetric: marking
// a log level sensitive costs a redacted debug line, while marking a database
// password non-sensitive puts it in a log.
func valueFor(doc configVars, t target, ref mamori.Ref) (mamori.Value, error) {
	raw, ok := doc[t.name]
	if !ok || raw == nil {
		return mamori.Value{}, fmt.Errorf("mamori/heroku: app %q has no config var %q: %w", t.app, t.name, mamori.ErrNotFound)
	}
	value := []byte(*raw)

	body, err := mamori.SelectKey(value, ref.Key)
	if err != nil {
		// One %w: SelectKey already wraps ErrNotFound or ErrInvalid, and
		// ResolveBatch depends on telling those two apart. Neither the value
		// nor the selected fragment is named; SelectKey's own message carries
		// only the key.
		return mamori.Value{}, fmt.Errorf("mamori/heroku: config var %q of app %q: %w", t.name, t.app, err)
	}

	return mamori.Value{
		Bytes: body,
		// The hash is of the WHOLE config var value, not of the selected
		// fragment, so two refs selecting different #fields of one var agree on
		// when it changed.
		//
		// It is deliberately not the response ETag, which this endpoint does
		// send. An ETag describes the whole document, so editing one var would
		// change the Version of every ref pointing at that app: a spurious
		// PreApply and a spurious OnChange for every field but the one that
		// actually moved, and for a rotating credential a spurious reconnect.
		// Value.Version is compared instead of bytes (see value.go), so it must
		// move exactly when THIS var moves.
		Version:   mamori.VersionHash(value),
		Sensitive: true,
		Metadata: map[string]string{
			// The app is annotation, not payload: it is the one piece of scope
			// a ref may have taken from configuration rather than stated, so an
			// operator reading a status page can see which app answered.
			"app": t.app,
		},
	}, nil
}
