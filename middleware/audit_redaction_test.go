package middleware_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/middleware"
)

type okProvider struct{}

func (okProvider) Scheme() string { return "pg" }
func (okProvider) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
	return mamori.Value{Bytes: []byte("v")}, nil
}

// TestAuditNeverLogsAnInlineCredential is the regression guard for an audit
// record leaving the process with a live secret in it. Some providers accept a
// credential as a query option, so Ref.Raw can carry one.
func TestAuditNeverLogsAnInlineCredential(t *testing.T) {
	ref, err := mamori.ParseRef("pg://host/db?password=hunter2&sslmode=require")
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	if !strings.Contains(ref.Raw, "hunter2") {
		t.Fatalf("fixture is wrong: Raw does not carry the credential: %q", ref.Raw)
	}

	var buf bytes.Buffer
	p := middleware.Audit(slog.New(slog.NewTextHandler(&buf, nil)), okProvider{})
	if _, err := p.Resolve(context.Background(), ref); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	out := buf.String()
	if out == "" {
		t.Fatal("nothing was logged; the guard would not catch a regression")
	}
	if strings.Contains(out, "hunter2") {
		t.Fatalf("audit log leaked the credential: %s", out)
	}
	if !strings.Contains(out, "sslmode=require") {
		t.Fatalf("audit log dropped the non-sensitive option, losing diagnostic value: %s", out)
	}
}
