package mamori

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAuthFuncAdaptsAFunction(t *testing.T) {
	called := false
	var a Authenticator = AuthFunc(func(r *http.Request) (Identity, error) {
		called = true
		return Identity{Subject: "svc"}, nil
	})
	req := httptest.NewRequest("GET", "/", nil)
	id, err := a.Authenticate(req)
	if err != nil {
		t.Fatalf("Authenticate returned error: %v", err)
	}
	if !called {
		t.Fatal("AuthFunc did not invoke the wrapped function")
	}
	if id.Subject != "svc" {
		t.Fatalf("Identity.Subject = %q, want svc", id.Subject)
	}
}

func TestErrForbiddenIsDistinct(t *testing.T) {
	if errors.Is(ErrForbidden, ErrNotFound) {
		t.Fatal("ErrForbidden must be its own sentinel")
	}
}
