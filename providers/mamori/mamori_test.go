package mamoriprov

import (
	"context"
	"errors"
	"net/url"
	"testing"

	"github.com/xavidop/mamori"
)

func TestParseEndpointUnix(t *testing.T) {
	baseURL, transport, err := parseEndpoint("unix:///run/x.sock", false)
	if err != nil {
		t.Fatalf("parseEndpoint returned unexpected error: %v", err)
	}
	if transport == nil {
		t.Fatal("expected a non-nil transport for a unix endpoint")
	}
	if transport.DialContext == nil {
		t.Fatal("expected transport.DialContext to be set for a unix endpoint")
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		t.Fatalf("baseURL %q did not parse: %v", baseURL, err)
	}
	if u.Host == "" {
		t.Fatalf("expected baseURL %q to have a dummy host", baseURL)
	}
}

func TestParseEndpointHTTPSOK(t *testing.T) {
	baseURL, transport, err := parseEndpoint("https://h:8443", false)
	if err != nil {
		t.Fatalf("parseEndpoint returned unexpected error: %v", err)
	}
	if transport == nil {
		t.Fatal("expected a non-nil transport for an https endpoint")
	}
	if baseURL != "https://h:8443" {
		t.Fatalf("baseURL = %q, want %q", baseURL, "https://h:8443")
	}
}

func TestParseEndpointHTTPRefusedWithoutInsecure(t *testing.T) {
	_, _, err := parseEndpoint("http://h:80", false)
	if err == nil {
		t.Fatal("expected an error for a plaintext http endpoint without InsecureNoTLS")
	}
	if !errors.Is(err, mamori.ErrInvalid) {
		t.Fatalf("err = %v, want errors.Is(err, mamori.ErrInvalid)", err)
	}
}

func TestParseEndpointHTTPAllowedWithInsecure(t *testing.T) {
	_, _, err := parseEndpoint("http://h:80", true)
	if err != nil {
		t.Fatalf("parseEndpoint returned unexpected error with insecure=true: %v", err)
	}
}

func TestParseEndpointEmptyIsInvalid(t *testing.T) {
	_, _, err := parseEndpoint("", false)
	if !errors.Is(err, mamori.ErrInvalid) {
		t.Fatalf("err = %v, want errors.Is(err, mamori.ErrInvalid)", err)
	}
}

func TestSchemeIsMamori(t *testing.T) {
	p := New(Config{Endpoint: "unix:///x.sock"})
	if got, want := p.Scheme(), "mamori"; got != want {
		t.Fatalf("Scheme() = %q, want %q", got, want)
	}
}

func TestEndpointErrorSurfacesFromResolve(t *testing.T) {
	p := New(Config{Endpoint: "http://h"})
	_, err := p.Resolve(context.Background(), mamori.Ref{Path: "x", Raw: "mamori://x"})
	if !errors.Is(err, mamori.ErrInvalid) {
		t.Fatalf("Resolve err = %v, want errors.Is(err, mamori.ErrInvalid)", err)
	}
}
