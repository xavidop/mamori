package secret

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

func TestStringRedaction(t *testing.T) {
	s := NewString("hunter2")

	if got := s.String(); got != Redacted {
		t.Errorf("String() = %q, want %q", got, Redacted)
	}
	if got := fmt.Sprintf("%v", s); got != Redacted {
		t.Errorf("%%v = %q, want %q", got, Redacted)
	}
	if got := fmt.Sprintf("pw=%s", s); got != "pw="+Redacted {
		t.Errorf("%%s = %q, want %q", got, "pw="+Redacted)
	}
	if got := fmt.Sprintf("%#v", s); strings.Contains(got, "hunter2") {
		t.Errorf("%%#v leaked secret: %q", got)
	}
	if s.Reveal() != "hunter2" {
		t.Errorf("Reveal() = %q, want hunter2", s.Reveal())
	}
}

func TestStringJSON(t *testing.T) {
	type wrap struct {
		Pw String `json:"pw"`
	}
	out, err := json.Marshal(wrap{Pw: NewString("hunter2")})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "hunter2") {
		t.Fatalf("JSON leaked secret: %s", out)
	}
	if !strings.Contains(string(out), Redacted) {
		t.Fatalf("JSON missing redaction: %s", out)
	}
}

func TestStringSlog(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	logger.Info("login", "password", NewString("hunter2"))
	if strings.Contains(buf.String(), "hunter2") {
		t.Fatalf("slog leaked secret: %s", buf.String())
	}
	if !strings.Contains(buf.String(), Redacted) {
		t.Fatalf("slog missing redaction: %s", buf.String())
	}
}

func TestStringZero(t *testing.T) {
	s := NewString("hunter2")
	s.Zero()
	if s.Reveal() != "" {
		t.Fatalf("after Zero, Reveal() = %q, want empty", s.Reveal())
	}
	if !s.IsZero() {
		t.Fatal("after Zero, IsZero() = false")
	}
}

func TestBytesRedaction(t *testing.T) {
	b := NewBytes([]byte("cert-bytes"))
	if b.String() != Redacted {
		t.Errorf("String() = %q, want %q", b.String(), Redacted)
	}
	out, _ := json.Marshal(b)
	if strings.Contains(string(out), "cert-bytes") {
		t.Fatalf("JSON leaked: %s", out)
	}
	if string(b.Reveal()) != "cert-bytes" {
		t.Errorf("Reveal() = %q", b.Reveal())
	}
	b.Zero()
	if !b.IsZero() {
		t.Fatal("Bytes.Zero did not clear")
	}
}
