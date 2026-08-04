package cloudflarekv

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/xavidop/mamori"
)

// TestResolveClassifiesStatus pins the status-to-kind mapping this provider
// inherits from httpcore.ClassifyStatus, exercised end to end through Resolve
// against a real response. There is no longer a hand-rolled classifyStatus to
// table-test directly: the mapping lives in one place for every httpcore-backed
// provider.
//
// Three rows differ from the mapping this provider carried before it moved onto
// httpcore, and each is deliberate:
//
//   - 422 was previously unclassified (KindUnknown), so mamori treated it as an
//     unknown failure and kept retrying a request that was well formed and
//     semantically wrong. It is now KindInvalid.
//   - 408 was previously unclassified. It is now KindRateLimited, alongside 429.
//   - 418, and every other status neither this provider nor httpcore names, was
//     previously KindUnknown and is now KindUnavailable: httpcore has one
//     default for an unrecognized status rather than per-provider silence.
//
// 404 is absent on purpose: get and bulkGet each take it back for a message of
// their own, which TestResolveAbsentKeyIsNotFound covers.
func TestResolveClassifiesStatus(t *testing.T) {
	cases := []struct {
		code int
		want mamori.Kind
	}{
		{http.StatusUnauthorized, mamori.KindUnauthenticated},
		{http.StatusForbidden, mamori.KindPermissionDenied},
		{http.StatusTooManyRequests, mamori.KindRateLimited},
		{http.StatusRequestTimeout, mamori.KindRateLimited},
		{http.StatusBadRequest, mamori.KindInvalid},
		{http.StatusUnprocessableEntity, mamori.KindInvalid},
		{http.StatusInternalServerError, mamori.KindUnavailable},
		{http.StatusBadGateway, mamori.KindUnavailable},
		{http.StatusTeapot, mamori.KindUnavailable},
	}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.code), func(t *testing.T) {
			f := newFake()
			f.set(testNamespace, "k", []byte("v"))
			f.failStatus(testNamespace, tc.code)
			p := f.provider()

			_, err := p.Resolve(context.Background(), ref(t, "cloudflare-kv://k"))
			if err == nil {
				t.Fatalf("status %d: Resolve returned a nil error", tc.code)
			}
			if got := mamori.ErrorKind(err); got != tc.want {
				t.Fatalf("status %d: ErrorKind = %q, want %q (err: %v)", tc.code, got, tc.want, err)
			}
		})
	}
}

// TestResolveBatchClassifiesUnmappedStatus is TestResolveClassifiesStatus's
// counterpart for the bulk path, which reaches the same classification through
// its own call site.
func TestResolveBatchClassifiesUnmappedStatus(t *testing.T) {
	f := newFake()
	f.set(testNamespace, "k", []byte("v"))
	f.failStatus(testNamespace, http.StatusTeapot)
	p := f.provider()

	_, err := p.ResolveBatch(context.Background(), []mamori.Ref{ref(t, "cloudflare-kv://k")})
	if err == nil {
		t.Fatal("ResolveBatch returned a nil error for a 418 response")
	}
	if got := mamori.ErrorKind(err); got != mamori.KindUnavailable {
		t.Fatalf("ErrorKind = %q, want %q", got, mamori.KindUnavailable)
	}
}

// TestResolveErrorMessagePreservesContext guards what the migration onto
// httpcore had to keep: the key, the HTTP status, and the verbatim (bounded)
// upstream body all still reach the message, and the classification is still
// reachable with errors.Is rather than flattened into text.
func TestResolveErrorMessagePreservesContext(t *testing.T) {
	f := newFake()
	f.set(testNamespace, "log-level", []byte("v"))
	f.failStatus(testNamespace, http.StatusForbidden)
	p := f.provider()

	_, err := p.Resolve(context.Background(), ref(t, "cloudflare-kv://log-level"))
	if err == nil {
		t.Fatal("Resolve returned a nil error for a 403 response")
	}
	if !errors.Is(err, mamori.ErrPermissionDenied) {
		t.Fatalf("classification lost: %v", err)
	}
	for _, want := range []string{"log-level", "403", "injected failure"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error message lost %q: %v", want, err)
		}
	}
}

// TestRedactPathSubstitutesBothIDs pins the hook this package hands
// httpcore.Config.RedactPath: both ids are gone from the path httpcore will
// render, both placeholders are there in their place, and the rest of the path
// is untouched so an error still says which endpoint was called.
//
// It replaces the two tests that covered redactIDs, the local workaround this
// hook retired. Half of what those tests guarded is now unfalsifiable rather
// than untested: redactIDs rewrote a finished error message and so could lose
// the cause, while a path substituted before the message exists cannot, since
// no error is rewritten at all. The two transport-failure leak tests in
// resolve_test.go drive the same guarantee end to end, through Resolve and
// through ResolveBatch.
func TestRedactPathSubstitutesBothIDs(t *testing.T) {
	s := settings{token: secretToken, account: secretAccount, namespace: secretNamespace}
	path := namespacePath(s) + "/values/log-level"

	got := redactPath(s)(path)

	if strings.Contains(got, secretAccount) || strings.Contains(got, secretNamespace) {
		t.Fatalf("redactPath left an id in %q", got)
	}
	if !strings.Contains(got, accountPlaceholder) || !strings.Contains(got, namespacePlaceholder) {
		t.Fatalf("redactPath did not substitute both placeholders: %q", got)
	}
	if !strings.HasSuffix(got, "/values/log-level") {
		t.Fatalf("redactPath rewrote more than the ids: %q", got)
	}
}

// TestRedactPathLeavesAPathCarryingNoIDAlone keeps the hook from mangling a
// path that names nothing identifying, which is every path httpcore renders for
// a client built with settings these ids did not come from.
func TestRedactPathLeavesAPathCarryingNoIDAlone(t *testing.T) {
	s := settings{token: secretToken, account: secretAccount, namespace: secretNamespace}
	const path = "/accounts/other/storage/kv/namespaces/other/values/k"

	if got := redactPath(s)(path); got != path {
		t.Fatalf("redactPath rewrote a path carrying no id: %q", got)
	}
}
