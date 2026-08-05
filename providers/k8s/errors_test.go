package k8s

import (
	"errors"
	"testing"

	"github.com/xavidop/mamori"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
)

var testGR = schema.GroupResource{Group: "", Resource: "secrets"}

func TestClassifyK8s(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want mamori.Kind
	}{
		{"NotFound", apierrors.NewNotFound(testGR, "db"), mamori.KindNotFound},
		{"Forbidden", apierrors.NewForbidden(testGR, "db", errors.New("rbac")), mamori.KindPermissionDenied},
		{"Unauthorized", apierrors.NewUnauthorized("bad token"), mamori.KindUnauthenticated},
		{"TooManyRequests", apierrors.NewTooManyRequests("slow down", 1), mamori.KindRateLimited},
		{"ServiceUnavailable", apierrors.NewServiceUnavailable("no endpoints"), mamori.KindUnavailable},
		{"Timeout", apierrors.NewTimeoutError("timed out", 1), mamori.KindUnavailable},
		{"BadRequest", apierrors.NewBadRequest("malformed"), mamori.KindInvalid},
		{"Conflict", apierrors.NewConflict(testGR, "db", errors.New("conflict")), mamori.KindUnknown},
		{"PlainError", errors.New("connection refused"), mamori.KindUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mamori.ErrorKind(classifyK8s(tc.err)); got != tc.want {
				t.Fatalf("ErrorKind = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestClassifyK8sPreservesStatusError(t *testing.T) {
	orig := apierrors.NewForbidden(testGR, "db", errors.New("rbac denied"))
	wrapped := classifyK8s(orig)

	if !errors.Is(wrapped, mamori.ErrPermissionDenied) {
		t.Fatalf("classification lost: %v", wrapped)
	}
	if !apierrors.IsForbidden(wrapped) {
		t.Fatalf("apierrors.IsForbidden no longer recognizes the wrapped error; "+
			"the %%w: %%w pattern must keep the *StatusError reachable: %v", wrapped)
	}
	var se *apierrors.StatusError
	if !errors.As(wrapped, &se) {
		t.Fatalf("errors.As can no longer reach *apierrors.StatusError: %v", wrapped)
	}
	if se.ErrStatus.Reason != metav1.StatusReasonForbidden {
		t.Fatalf("recovered Reason = %q, want Forbidden", se.ErrStatus.Reason)
	}
}

// TestMapGetErrorNotFoundPreservesStatusError guards mapGetError's IsNotFound
// pre-check branch specifically (not classifyK8s directly): it must
// double-wrap (sentinel AND the underlying *apierrors.StatusError), not just
// the sentinel, so errors.As can still reach the SDK error.
func TestMapGetErrorNotFoundPreservesStatusError(t *testing.T) {
	orig := apierrors.NewNotFound(testGR, "db")
	got := mapGetError(SchemeSecret, "secret", "default", "db", orig)

	if !errors.Is(got, mamori.ErrNotFound) {
		t.Fatalf("mapGetError lost ErrNotFound: %v", got)
	}
	var se *apierrors.StatusError
	if !errors.As(got, &se) {
		t.Fatalf("errors.As can no longer reach *apierrors.StatusError: %v", got)
	}
	if !apierrors.IsNotFound(got) {
		t.Fatalf("apierrors.IsNotFound no longer recognizes the wrapped error: %v", got)
	}
}

// TestWithClientDoesNotClaimOwnership is the ownership half of rule 5,
// asserted directly against the unexported field: a clientset handed in with
// WithClient must never be recorded as owned, or Close (k8s.go) will call
// closeIdleConnections on it and pull a shared clientset's keep-alive pool out
// from under every other consumer sharing it.
//
// This lives in this file (package k8s), not k8s_test.go (package
// k8s_test), specifically so it can read p.ownClient directly. The
// functional check in k8s_test.go's TestCloseDoesNotTouchInjectedClientset -
// that the fake clientset keeps serving requests after Close - has no power
// against this particular regression: closeIdleConnections is a documented
// no-op on the fake clientset's typed-nil *rest.RESTClient regardless of
// p.ownClient's value, so that test alone cannot fail if ownership tracking
// breaks. This test is what actually pins the flag.
func TestWithClientDoesNotClaimOwnership(t *testing.T) {
	p := New(WithClient(fake.NewSimpleClientset()))
	if p.ownClient {
		t.Fatal("WithClient claimed ownership of the injected clientset")
	}
}

// TestSplitPath asserts that a malformed ref path (not <namespace>/<name>)
// reports mamori.KindInvalid, matching the documented kind table (and the
// gcp/azure providers, which already classify malformed refs this way).
func TestSplitPath(t *testing.T) {
	cases := []struct {
		in      string
		ns      string
		name    string
		wantErr bool
	}{
		{in: "default/db", ns: "default", name: "db"},
		{in: "db", wantErr: true},
		{in: "", wantErr: true},
		{in: "default/", wantErr: true},
		{in: "/db", wantErr: true},
		{in: "default/db/extra", wantErr: true},
	}
	for _, tc := range cases {
		ns, name, err := splitPath(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("splitPath(%q) = (%q,%q,nil), want error", tc.in, ns, name)
				continue
			}
			if got := mamori.ErrorKind(err); got != mamori.KindInvalid {
				t.Errorf("splitPath(%q): ErrorKind = %q, want %q (a malformed ref)", tc.in, got, mamori.KindInvalid)
			}
			continue
		}
		if err != nil {
			t.Errorf("splitPath(%q) unexpected error: %v", tc.in, err)
			continue
		}
		if ns != tc.ns || name != tc.name {
			t.Errorf("splitPath(%q) = (%q,%q), want (%q,%q)", tc.in, ns, name, tc.ns, tc.name)
		}
	}
}
