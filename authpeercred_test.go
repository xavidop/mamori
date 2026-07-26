package mamori_test

import (
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"

	"github.com/xavidop/mamori"
)

// TestPeerCredDeniesWithNoPeerCredsInContext is the one PeerCred behavior
// that must hold on every platform mamori builds for, supported or not: a
// request whose context was never plumbed with ContextWithPeerCred (the
// common case for anything not arriving over a Unix socket whose listener
// wires up ConnContext - i.e. almost every request in a test, and every
// request on an unsupported platform) is denied outright, never treated as
// "no restriction configured". See authpeercred.go's peerCred.authenticate
// and authpeercred_other.go's unconditional deny for the two places this
// guarantee is actually enforced.
func TestPeerCredDeniesWithNoPeerCredsInContext(t *testing.T) {
	a := mamori.PeerCred(mamori.PeerCredOptions{})
	req := httptest.NewRequest(http.MethodGet, "/", nil)

	if _, err := a.Authenticate(req); err == nil {
		t.Fatal("PeerCred allowed a request with no plumbed peer credentials, want deny")
	}
}

// TestPeerCredAllowlistByPlatform exercises the seam end to end - stashing a
// synthetic Ucred into a request's context with ContextWithPeerCred, exactly
// as a real ConnContext callback would (see PeerCred's doc comment) - without
// needing an actual Unix socket. What the resulting Authenticate call
// decides is platform-dependent by design: Linux and Darwin apply the
// uid/gid allowlist to whatever credentials the context carries (real,
// kernel-verified ones in production; a synthetic value here, since the seam
// itself does not care where the Ucred came from), while every other
// platform's Authenticate denies unconditionally regardless of what is in
// the context (authpeercred_other.go) - it must never be fooled into
// allowing just because a value happens to be present. Asserting both
// outcomes from the same test is what proves the seam behaves identically
// everywhere and only the platform-specific Authenticate differs.
func TestPeerCredAllowlistByPlatform(t *testing.T) {
	supported := runtime.GOOS == "linux" || runtime.GOOS == "darwin"
	cred := mamori.Ucred{UID: 1000, GID: 2000, PID: 4242}

	newReq := func() *http.Request {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		return req.WithContext(mamori.ContextWithPeerCred(req.Context(), cred))
	}

	t.Run("uid in allowlist", func(t *testing.T) {
		a := mamori.PeerCred(mamori.PeerCredOptions{UIDs: []int{cred.UID}})
		id, err := a.Authenticate(newReq())
		if supported {
			if err != nil {
				t.Fatalf("Authenticate returned error on a supported platform: %v", err)
			}
			if want := "uid:1000"; id.Subject != want {
				t.Fatalf("Identity.Subject = %q, want %q", id.Subject, want)
			}
			if got := id.Attrs["uid"]; len(got) != 1 || got[0] != "1000" {
				t.Fatalf("Identity.Attrs[uid] = %v, want [1000]", got)
			}
			if got := id.Attrs["gid"]; len(got) != 1 || got[0] != "2000" {
				t.Fatalf("Identity.Attrs[gid] = %v, want [2000]", got)
			}
			if got := id.Attrs["pid"]; len(got) != 1 || got[0] != "4242" {
				t.Fatalf("Identity.Attrs[pid] = %v, want [4242]", got)
			}
		} else if err == nil {
			t.Fatal("Authenticate allowed a request on an unsupported platform, want unconditional deny")
		}
	})

	t.Run("gid in allowlist", func(t *testing.T) {
		a := mamori.PeerCred(mamori.PeerCredOptions{GIDs: []int{cred.GID}})
		_, err := a.Authenticate(newReq())
		if supported && err != nil {
			t.Fatalf("Authenticate returned error on a supported platform: %v", err)
		}
		if !supported && err == nil {
			t.Fatal("Authenticate allowed a request on an unsupported platform, want unconditional deny")
		}
	})

	t.Run("neither uid nor gid in allowlist", func(t *testing.T) {
		a := mamori.PeerCred(mamori.PeerCredOptions{UIDs: []int{9999}, GIDs: []int{9999}})
		_, err := a.Authenticate(newReq())
		if err == nil {
			t.Fatal("Authenticate allowed a peer not in either allowlist, want deny")
		}
	})

	t.Run("empty allowlists permit any read peer", func(t *testing.T) {
		a := mamori.PeerCred(mamori.PeerCredOptions{})
		_, err := a.Authenticate(newReq())
		if supported && err != nil {
			t.Fatalf("Authenticate with empty allowlists denied a peer whose credentials were read: %v", err)
		}
		if !supported && err == nil {
			t.Fatal("Authenticate allowed a request on an unsupported platform, want unconditional deny")
		}
	})
}

// TestPeerCredIsNotAChallenger documents that a failed PeerCred request gets
// a bare 401: unlike BasicAuth or BearerToken, there is no WWW-Authenticate
// scheme a client could respond to (the credential is not something a client
// presents at all), so PeerCred implements no Challenger, matching APIKey
// and MTLS.
func TestPeerCredIsNotAChallenger(t *testing.T) {
	a := mamori.PeerCred(mamori.PeerCredOptions{})
	if _, ok := a.(mamori.Challenger); ok {
		t.Fatal("PeerCred must not implement Challenger (bare 401 only)")
	}
}
