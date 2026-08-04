package supabase

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/xavidop/mamori"
)

// clearEnv unsets every variable this provider reads, so a developer who
// happens to have SUPABASE_* set in their shell cannot change what a test
// exercises. t.Setenv restores the previous value when the test ends.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{envURL, envServiceKey, envSchema, envView} {
		t.Setenv(k, "")
	}
}

// mustRef parses a ref or fails the test.
func mustRef(t *testing.T, raw string) mamori.Ref {
	t.Helper()
	ref, err := mamori.ParseRef(raw)
	if err != nil {
		t.Fatalf("ParseRef(%q): %v", raw, err)
	}
	return ref
}

// TestResolveReturnsTheDecryptedSecret is the base case: the value under the
// decrypted_secret column reaches Value.Bytes.
func TestResolveReturnsTheDecryptedSecret(t *testing.T) {
	clearEnv(t)
	f := newFake()
	f.set("DB_PASSWORD", "hunter2")

	v, err := f.provider().Resolve(context.Background(), mustRef(t, "supabase://DB_PASSWORD"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "hunter2" {
		t.Fatalf("Bytes = %q, want %q", v.Bytes, "hunter2")
	}
	if !v.Sensitive {
		t.Error("Sensitive = false; every value from a secret manager must be marked sensitive")
	}
	if v.Version == "" {
		t.Error("Version is empty")
	}
}

// TestResolveSendsTheDocumentedPostgRESTQuery pins the wire shape this provider
// was built from: the relation as the path, PostgREST's "?name=eq.<value>"
// horizontal filter, and an explicit select= naming only the three columns it
// reads.
//
// The select matters beyond tidiness: vault.decrypted_secrets also carries the
// raw ciphertext, the key id and the nonce, and none of them belongs in a
// response this process holds in memory.
func TestResolveSendsTheDocumentedPostgRESTQuery(t *testing.T) {
	clearEnv(t)
	f := newFake()
	f.set("DB_PASSWORD", "hunter2")

	if _, err := f.provider().Resolve(context.Background(), mustRef(t, "supabase://DB_PASSWORD")); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if got, want := f.lastPath, "/rest/v1/decrypted_secrets"; got != want {
		t.Errorf("path = %q, want %q", got, want)
	}
	if got, want := f.lastQuery.Get("name"), "eq.DB_PASSWORD"; got != want {
		t.Errorf("name filter = %q, want %q", got, want)
	}
	if got, want := f.lastQuery.Get("select"), "name,decrypted_secret,updated_at"; got != want {
		t.Errorf("select = %q, want %q", got, want)
	}
	if _, ok := f.lastQuery["limit"]; ok {
		t.Error("a limit= was sent; a duplicated name must stay detectable rather than be silently truncated")
	}
}

// TestResolveSendsBothCredentialHeaders pins that the service-role key travels
// in BOTH headers Supabase needs.
//
// Sending only apikey is the failure that looks like a permissions problem:
// PostgREST reads the Authorization bearer to choose the database role, so
// without it a correct service-role key still runs as the anonymous role and is
// refused the relation.
func TestResolveSendsBothCredentialHeaders(t *testing.T) {
	clearEnv(t)
	f := newFake()
	f.set("TOKEN", "value")

	if _, err := f.provider().Resolve(context.Background(), mustRef(t, "supabase://TOKEN")); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := f.lastAPIKey; got != testServiceKey {
		t.Errorf("apikey header = %q, want the service-role key", got)
	}
	if got, want := f.lastAuth, "Bearer "+testServiceKey; got != want {
		t.Errorf("Authorization header = %q, want %q", got, want)
	}
}

// TestResolveAlwaysSendsAcceptProfile pins that the schema is named on every
// request, including for the default public schema.
//
// PostgREST selects the FIRST entry of db-schemas as its default, so a project
// exposing "api,public" would read a different schema than one exposing
// "public,api" if the header were omitted. Sending it always is what makes a
// ref mean the same thing on both.
func TestResolveAlwaysSendsAcceptProfile(t *testing.T) {
	clearEnv(t)
	f := newFake()
	f.set("TOKEN", "value")

	if _, err := f.provider().Resolve(context.Background(), mustRef(t, "supabase://TOKEN")); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got, want := f.lastProfile, defaultSchema; got != want {
		t.Fatalf("Accept-Profile = %q, want %q even for the default schema", got, want)
	}
}

// TestSettingsPrecedence pins the precedence chain: a ref option beats a
// provider option, which beats an environment variable, which beats the
// built-in default. That is exactly providers/cloudflare-kv's ?namespace= rule,
// so an operator who knows one knows the other.
func TestSettingsPrecedence(t *testing.T) {
	tests := []struct {
		name       string
		env        map[string]string
		opts       []Option
		ref        string
		wantSchema string
		wantView   string
	}{
		{
			name:       "defaults when nothing is set",
			ref:        "supabase://TOKEN",
			wantSchema: "public",
			wantView:   "decrypted_secrets",
		},
		{
			name:       "environment beats the default",
			env:        map[string]string{envSchema: "env_schema", envView: "env_view"},
			ref:        "supabase://TOKEN",
			wantSchema: "env_schema",
			wantView:   "env_view",
		},
		{
			name:       "provider option beats the environment",
			env:        map[string]string{envSchema: "env_schema", envView: "env_view"},
			opts:       []Option{WithSchema("opt_schema"), WithView("opt_view")},
			ref:        "supabase://TOKEN",
			wantSchema: "opt_schema",
			wantView:   "opt_view",
		},
		{
			name:       "ref option beats the provider option",
			env:        map[string]string{envSchema: "env_schema", envView: "env_view"},
			opts:       []Option{WithSchema("opt_schema"), WithView("opt_view")},
			ref:        "supabase://TOKEN?schema=ref_schema&view=ref_view",
			wantSchema: "ref_schema",
			wantView:   "ref_view",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearEnv(t)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			p := New(tt.opts...)
			got, err := p.settingsFor(mustRef(t, tt.ref))
			if err != nil {
				t.Fatalf("settingsFor: %v", err)
			}
			if got.schema != tt.wantSchema {
				t.Errorf("schema = %q, want %q", got.schema, tt.wantSchema)
			}
			if got.view != tt.wantView {
				t.Errorf("view = %q, want %q", got.view, tt.wantView)
			}
		})
	}
}

// TestRefOptionsReachTheWire is the end-to-end half of the precedence test:
// that ?schema= and ?view= actually change the request rather than only the
// settings struct.
func TestRefOptionsReachTheWire(t *testing.T) {
	clearEnv(t)
	f := newFake()
	f.set("TOKEN", "value")

	// The fake refuses a schema it does not serve with PGRST106, exactly as a
	// project does, so this also pins that the header is what selects it.
	_, err := f.provider().Resolve(context.Background(), mustRef(t, "supabase://TOKEN?schema=other_schema"))
	if err == nil {
		t.Fatal("a schema the project does not expose resolved successfully")
	}
	if got, want := f.lastProfile, "other_schema"; got != want {
		t.Errorf("Accept-Profile = %q, want %q", got, want)
	}

	_, err = f.provider().Resolve(context.Background(), mustRef(t, "supabase://TOKEN?view=other_view"))
	if err == nil {
		t.Fatal("a relation the project does not have resolved successfully")
	}
	if got, want := f.lastPath, "/rest/v1/other_view"; got != want {
		t.Errorf("path = %q, want %q", got, want)
	}
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Errorf("an unknown relation err = %v, want ErrNotFound (PostgREST answers 404)", err)
	}
}

// TestCredentialsReadFromEnvironment pins that the project URL and service key
// are read lazily from the environment at resolve time, which is what makes
// registering this provider from a blank import safe when no credentials exist
// at process start.
func TestCredentialsReadFromEnvironment(t *testing.T) {
	clearEnv(t)
	f := newFake()
	t.Setenv(envURL, testProjectURL)
	t.Setenv(envServiceKey, testServiceKey)
	f.set("TOKEN", "value")

	p := New(WithHTTPClient(&http.Client{Transport: f.transport()}))
	v, err := p.Resolve(context.Background(), mustRef(t, "supabase://TOKEN"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "value" {
		t.Fatalf("Bytes = %q, want %q", v.Bytes, "value")
	}
}

// TestExplicitCredentialsBeatTheEnvironment pins the other half: an explicit
// option wins over its environment variable.
func TestExplicitCredentialsBeatTheEnvironment(t *testing.T) {
	clearEnv(t)
	f := newFake()
	t.Setenv(envURL, "https://wrong.supabase.test")
	t.Setenv(envServiceKey, "wrong-key")
	f.set("TOKEN", "value")

	if _, err := f.provider().Resolve(context.Background(), mustRef(t, "supabase://TOKEN")); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if f.lastAPIKey != testServiceKey {
		t.Fatalf("apikey = %q, want the explicit option's key", f.lastAPIKey)
	}
}

// TestResolveUsesUpdatedAtAsVersion pins the Version decision: the row's
// updated_at, so mamori gets change detection from the backend's own write
// timestamp rather than from a content hash.
func TestResolveUsesUpdatedAtAsVersion(t *testing.T) {
	clearEnv(t)
	f := newFake()
	f.set("TOKEN", "first")

	first, err := f.provider().Resolve(context.Background(), mustRef(t, "supabase://TOKEN"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := f.secrets["TOKEN"].updatedAt; first.Version != got {
		t.Fatalf("Version = %q, want the row's updated_at %q", first.Version, got)
	}

	f.set("TOKEN", "second")
	second, err := f.provider().Resolve(context.Background(), mustRef(t, "supabase://TOKEN"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if first.Version == second.Version {
		t.Fatal("Version did not change across a rotation; mamori would never see the new value")
	}
}

// TestResolveVersionChangesWhenOnlyTheTimestampDoes is the case an id-based
// Version would fail and the reason updated_at was chosen over it.
//
// Vault's update_secret rewrites a row in place, so the id survives a rotation
// unchanged. Here the same bytes are written again: the value is identical, so
// a content hash would also be unchanged, and only updated_at reports that a
// write happened at all.
func TestResolveVersionChangesWhenOnlyTheTimestampDoes(t *testing.T) {
	clearEnv(t)
	f := newFake()
	f.set("TOKEN", "same-bytes")

	first, err := f.provider().Resolve(context.Background(), mustRef(t, "supabase://TOKEN"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	f.set("TOKEN", "same-bytes")
	second, err := f.provider().Resolve(context.Background(), mustRef(t, "supabase://TOKEN"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(first.Bytes) != string(second.Bytes) {
		t.Fatal("the fake changed the value; this test is about an identical rewrite")
	}
	if first.Version == second.Version {
		t.Fatal("Version is unchanged after a rewrite of identical bytes; that is the content-hash behaviour updated_at exists to beat")
	}
}

// TestResolveHashesWhenTheRelationOmitsUpdatedAt pins the fallback. An
// operator's relation may not select updated_at, and rendering an absent
// timestamp as "" would pin Version to a constant for every ref at once, which
// is the one failure mode a poller cannot report.
func TestResolveHashesWhenTheRelationOmitsUpdatedAt(t *testing.T) {
	clearEnv(t)
	f := newFake()
	f.omitUpdatedAt = true
	f.set("TOKEN", "value")

	v, err := f.provider().Resolve(context.Background(), mustRef(t, "supabase://TOKEN"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if v.Version == "" {
		t.Fatal("Version is empty; the content-hash fallback did not apply")
	}
	if want := mamori.VersionHash([]byte("value")); v.Version != want {
		t.Fatalf("Version = %q, want the content hash %q", v.Version, want)
	}
}

// TestResolveVersionIgnoresKeySelection pins that the version describes the
// WHOLE secret rather than the selected fragment, so two refs selecting
// different #fields of one secret agree on when it changed.
func TestResolveVersionIgnoresKeySelection(t *testing.T) {
	clearEnv(t)
	f := newFake()
	f.set("CREDS", `{"user":"admin","password":"hunter2"}`)

	whole, err := f.provider().Resolve(context.Background(), mustRef(t, "supabase://CREDS"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	part, err := f.provider().Resolve(context.Background(), mustRef(t, "supabase://CREDS#password"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(part.Bytes) != "hunter2" {
		t.Fatalf("Bytes = %q, want %q", part.Bytes, "hunter2")
	}
	if whole.Version != part.Version {
		t.Fatalf("Version differs between a whole read (%q) and a #field read (%q)", whole.Version, part.Version)
	}
}

// TestResolveSelectsWithPointer pins RFC 6901 JSON Pointer selection, the
// nested counterpart of the literal-key case above.
func TestResolveSelectsWithPointer(t *testing.T) {
	clearEnv(t)
	f := newFake()
	f.set("CREDS", `{"db":{"password":"hunter2"}}`)

	v, err := f.provider().Resolve(context.Background(), mustRef(t, "supabase://CREDS#/db/password"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "hunter2" {
		t.Fatalf("Bytes = %q, want %q", v.Bytes, "hunter2")
	}
}

// TestResolveEmptySecretIsNotNotFound pins the distinction the pointer in
// secretRow exists for: a secret whose value is legitimately the empty string
// resolves, rather than being mistaken for an absent one. An absent row is an
// empty ARRAY; an empty STRING is a row that exists.
func TestResolveEmptySecretIsNotNotFound(t *testing.T) {
	clearEnv(t)
	f := newFake()
	f.set("EMPTY", "")

	v, err := f.provider().Resolve(context.Background(), mustRef(t, "supabase://EMPTY"))
	if err != nil {
		t.Fatalf("Resolve: %v; an empty secret is a value, not an absent row", err)
	}
	if len(v.Bytes) != 0 {
		t.Fatalf("Bytes = %q, want empty", v.Bytes)
	}
	if v.Version == "" {
		t.Error("Version is empty; an empty value still has a write timestamp")
	}
}

// TestSecretNameIsTheWholePath pins that the entire ref path is the name,
// including slashes and dot segments.
//
// This provider is unusual in being able to promise that: the name is a
// PostgREST filter VALUE in the query string, never a path segment, so
// httpcore's dot-segment rejection never applies to it. A name like "../etc" is
// an ordinary name that either matches a row or matches nothing.
func TestSecretNameIsTheWholePath(t *testing.T) {
	clearEnv(t)
	for _, name := range []string{
		"config/prod/db",
		"../etc/passwd",
		"a.b.c",
		"has spaces",
		`quote"and\backslash`,
		"comma,and(parens)",
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake()
			f.set(name, "value-for-"+name)

			ref := mustRef(t, "supabase://"+strings.ReplaceAll(name, "#", "%23"))
			v, err := f.provider().Resolve(context.Background(), ref)
			if err != nil {
				t.Fatalf("Resolve(%q): %v", name, err)
			}
			if got, want := string(v.Bytes), "value-for-"+name; got != want {
				t.Fatalf("Bytes = %q, want %q", got, want)
			}
		})
	}
}

// TestFilterValueQuotesOnlyWhenNeeded pins the PostgREST filter escaping rule.
//
// A plain name must reach the wire unquoted, byte-identical to every vendor
// example, because this provider's wire format is documented rather than
// live-verified. A name holding a reserved character must be double quoted,
// which is the escape PostgREST documents.
func TestFilterValueQuotesOnlyWhenNeeded(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"DB_PASSWORD", "DB_PASSWORD"},
		{"config/prod/db", "config/prod/db"},
		{"a.b", `"a.b"`},
		{"a,b", `"a,b"`},
		{"a:b", `"a:b"`},
		{"a(b)", `"a(b)"`},
		{"has space", `"has space"`},
		{`say"hi`, `"say\"hi"`},
		{`back\slash`, `"back\\slash"`},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := filterValue(tt.in); got != tt.want {
				t.Fatalf("filterValue(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestResolveRejectsANonHTTPProjectURL pins the closed scheme set. httpcore.New
// requires only a scheme and a host, so an ftp:// typo would construct cleanly
// and then fail on every resolve with net/http's "unsupported protocol scheme".
func TestResolveRejectsANonHTTPProjectURL(t *testing.T) {
	clearEnv(t)
	p := New(WithProjectURL("ftp://project.supabase.test"), WithServiceKey(testServiceKey))
	_, err := p.Resolve(context.Background(), mustRef(t, "supabase://TOKEN"))
	if !errors.Is(err, mamori.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}

// TestResolveRejectsAnInsecureProjectURL pins that http:// needs an explicit
// opt-in, and TestAllowInsecurePermitsHTTPAndNothingElse pins that the opt-in
// rescues cleartext http and nothing else.
func TestResolveRejectsAnInsecureProjectURL(t *testing.T) {
	clearEnv(t)
	f := newFake()
	f.set("TOKEN", "value")

	p := f.provider(WithProjectURL("http://127.0.0.1:54321"))
	_, err := p.Resolve(context.Background(), mustRef(t, "supabase://TOKEN"))
	if !errors.Is(err, mamori.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
	if strings.Contains(err.Error(), testServiceKey) {
		t.Fatalf("the service-role key leaked into %q", err)
	}
}

// TestAllowInsecurePermitsHTTPAndNothingElse pins that WithAllowInsecure is not
// a general "skip the scheme check" switch: it permits cleartext http, and an
// ftp:// URL is still refused with it set.
func TestAllowInsecurePermitsHTTPAndNothingElse(t *testing.T) {
	clearEnv(t)
	f := newFake()
	f.set("TOKEN", "value")

	p := f.provider(WithProjectURL("http://127.0.0.1:54321"), WithAllowInsecure())
	if _, err := p.Resolve(context.Background(), mustRef(t, "supabase://TOKEN")); err != nil {
		t.Fatalf("http:// with WithAllowInsecure: %v", err)
	}

	q := New(WithProjectURL("ftp://127.0.0.1"), WithServiceKey(testServiceKey), WithAllowInsecure())
	if _, err := q.Resolve(context.Background(), mustRef(t, "supabase://TOKEN")); !errors.Is(err, mamori.ErrInvalid) {
		t.Fatalf("ftp:// with WithAllowInsecure err = %v, want ErrInvalid", err)
	}
}

// TestProjectURLIsJoinedWithTheRESTPrefix pins that an operator pastes the
// dashboard's project URL and this package appends /rest/v1, rather than the
// operator having to know the mount point.
func TestProjectURLIsJoinedWithTheRESTPrefix(t *testing.T) {
	clearEnv(t)
	f := newFake()
	f.set("TOKEN", "value")

	// A trailing slash must not produce a double slash.
	p := f.provider(WithProjectURL(testProjectURL + "/"))
	if _, err := p.Resolve(context.Background(), mustRef(t, "supabase://TOKEN")); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got, want := f.lastPath, "/rest/v1/decrypted_secrets"; got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
}

// TestProviderNeverPrintsTheServiceKey pins the closure decision in Provider.
//
// fmt's %+v and %#v walk unexported fields by reflection, and reflection cannot
// call a String or GoString method on a value it reaches that way, so a
// redaction method would not have protected a plain string field. A Provider is
// exactly the value an application passes to mamori.WithProvider, so it is a
// plausible thing to log or to appear in a panic trace.
func TestProviderNeverPrintsTheServiceKey(t *testing.T) {
	clearEnv(t)
	f := newFake()
	f.set("TOKEN", "value")
	p := f.provider()

	// Before and after a resolve: the client is built lazily, so the second
	// check covers the state the first cannot reach.
	//
	// Only the POINTER is formatted. fmt expands a pointer-to-struct's fields
	// for every verb here ("&{...}" for %v and %+v, the full literal for %#v),
	// so this reaches the unexported fields exactly as a dereferenced copy
	// would; formatting *p as well would copy the Provider's sync.Mutex, which
	// go vet rejects.
	for _, phase := range []string{"before", "after"} {
		for _, verb := range []string{"%v", "%+v", "%#v"} {
			if got := fmt.Sprintf(verb, p); strings.Contains(got, testServiceKey) {
				t.Fatalf("%s resolve, %s of the Provider contains the service-role key", phase, verb)
			}
		}
		if _, err := p.Resolve(context.Background(), mustRef(t, "supabase://TOKEN")); err != nil {
			t.Fatalf("Resolve: %v", err)
		}
	}
}

// TestResolveIsNotCached pins that every Resolve is a live read. mamori.Refresh
// and mamori.Doctor both call Resolve directly, and PostgREST offers no ETag to
// gate a held snapshot on, so a cache here would serve a rotated secret's old
// value.
func TestResolveIsNotCached(t *testing.T) {
	clearEnv(t)
	f := newFake()
	f.set("TOKEN", "first")
	p := f.provider()

	if _, err := p.Resolve(context.Background(), mustRef(t, "supabase://TOKEN")); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	f.set("TOKEN", "second")
	v, err := p.Resolve(context.Background(), mustRef(t, "supabase://TOKEN"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "second" {
		t.Fatalf("Bytes = %q, want the rotated value; the read was cached", v.Bytes)
	}
	if f.reads != 2 {
		t.Fatalf("reads = %d, want 2", f.reads)
	}
}

// TestDeletedSecretBecomesNotFound pins the transition, which is the shape a
// poller actually sees: a secret that resolved a moment ago and has since been
// deleted must become not_found rather than keep resolving.
func TestDeletedSecretBecomesNotFound(t *testing.T) {
	clearEnv(t)
	f := newFake()
	f.set("TOKEN", "value")
	p := f.provider()

	if _, err := p.Resolve(context.Background(), mustRef(t, "supabase://TOKEN")); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	f.del("TOKEN")
	_, err := p.Resolve(context.Background(), mustRef(t, "supabase://TOKEN"))
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// TestProviderIsNotWatchable pins the deliberate absence of a Watch method.
// PostgREST exposes no streaming or blocking read, so mamori must wrap this
// provider in its polling adapter; a Watch method that polled internally would
// duplicate that machinery and hide the interval from mamori's configuration.
func TestProviderIsNotWatchable(t *testing.T) {
	var p any = New()
	if _, ok := p.(mamori.WatchableProvider); ok {
		t.Fatal("Provider implements WatchableProvider; PostgREST has no streaming read to back it")
	}
}

// TestSchemeIsRegistered pins that a blank import routes supabase:// refs here.
func TestSchemeIsRegistered(t *testing.T) {
	if got := New().Scheme(); got != "supabase" {
		t.Fatalf("Scheme() = %q, want %q", got, "supabase")
	}
}
