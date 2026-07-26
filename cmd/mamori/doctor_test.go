package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xavidop/mamori"
)

func TestDoctorTableHealthy(t *testing.T) {
	w := newHealthyWatcher(t)
	srv := httptest.NewServer(mamori.Handler(w))
	defer srv.Close()

	var outBuf, errBuf bytes.Buffer
	code := doctorCmd([]string{"--endpoint", srv.URL, "--insecure"}, &outBuf, &errBuf)
	if code != 0 {
		t.Fatalf("doctorCmd() code = %d, stderr = %s", code, errBuf.String())
	}
	if errBuf.String() != "" {
		t.Errorf("stderr = %q, want empty", errBuf.String())
	}
	out := outBuf.String()
	if !strings.Contains(out, "Level") {
		t.Errorf("table output missing field path %q:\n%s", "Level", out)
	}
	if !strings.Contains(out, "HEALTHY") {
		t.Errorf("table output missing a HEALTHY summary:\n%s", out)
	}
}

func TestDoctorTableUnreachableExit3(t *testing.T) {
	var outBuf, errBuf bytes.Buffer
	code := doctorCmd([]string{"--endpoint", "unix:///nonexistent/doctor-test.sock"}, &outBuf, &errBuf)
	if code != 3 {
		t.Errorf("doctorCmd() code = %d, want 3", code)
	}
	if errBuf.String() == "" {
		t.Error("stderr = empty, want an error message")
	}
}

func TestDoctorJSONPassesBodyThroughUnchanged(t *testing.T) {
	// A fixed, static body (rather than a real Watcher's Handler, whose
	// Report recomputes Age/GeneratedAt on every single read) so the
	// server's response is byte-identical across requests: this test is
	// about doctor --json not re-marshaling, not about whether two
	// separate GETs of a live, time-sensitive report happen to agree.
	wantBody := []byte(`{"Fields":[{"Path":"Level","Scheme":"ct","Ref":"ct://level","Version":"v1","LastOK":"2026-01-01T00:00:00Z","Age":0,"Stale":false,"LastError":"","LastKind":"","Sensitive":false}],"Snapshot":1,"Live":1,"Pinned":false,"Healthy":true,"GeneratedAt":"2026-01-01T00:00:00Z"}`)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		rw.WriteHeader(http.StatusOK)
		_, _ = rw.Write(wantBody)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	var outBuf, errBuf bytes.Buffer
	code := doctorCmd([]string{"--endpoint", srv.URL, "--insecure", "--json"}, &outBuf, &errBuf)
	if code != 0 {
		t.Fatalf("doctorCmd() code = %d, stderr = %s", code, errBuf.String())
	}

	got := strings.TrimRight(outBuf.String(), "\n")
	want := strings.TrimRight(string(wantBody), "\n")
	if got != want {
		t.Errorf("doctor --json output does not match the raw response body verbatim:\ngot:  %s\nwant: %s", got, want)
	}

	// The raw body must also be exactly what json.Marshal(mamori.Report)
	// produces: doctor --json must not re-marshal (which could reorder or
	// otherwise not byte-for-byte match what the server actually sent).
	var rep mamori.Report
	if err := json.Unmarshal([]byte(got), &rep); err != nil {
		t.Fatalf("doctor --json output does not decode as a Report: %v", err)
	}
}

func TestDoctorCompareDetectsDrift(t *testing.T) {
	root := moduleRoot(t)
	fixtureDir := filepath.Join(root, "testdata", "example")
	t.Chdir(fixtureDir)

	structs, err := Extract([]string{"./..."}, "")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	var sourcePaths []string
	seen := map[string]bool{}
	for _, s := range structs {
		for _, f := range s.Fields {
			if !seen[f.Path] {
				seen[f.Path] = true
				sourcePaths = append(sourcePaths, f.Path)
			}
		}
	}
	if len(sourcePaths) < 2 {
		t.Fatalf("fixture has only %d field(s), need at least 2 for a drift test", len(sourcePaths))
	}

	// Build a live report whose field set differs from the source by
	// exactly one MISSING field (in source, dropped from live) and one
	// EXTRA field (in live, not present in source at all).
	missing := sourcePaths[len(sourcePaths)-1]
	const extra = "LegacyField.NoLongerInSource"

	var fields []mamori.FieldStatus
	for _, p := range sourcePaths[:len(sourcePaths)-1] {
		fields = append(fields, mamori.FieldStatus{Path: p, Scheme: "env"})
	}
	fields = append(fields, mamori.FieldStatus{Path: extra, Scheme: "env"})

	body, err := json.Marshal(mamori.Report{Fields: fields, Healthy: true})
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		rw.WriteHeader(http.StatusOK)
		_, _ = rw.Write(body)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	var outBuf, errBuf bytes.Buffer
	code := doctorCmd([]string{"--endpoint", srv.URL, "--insecure", "--compare", "./..."}, &outBuf, &errBuf)
	if code != 0 {
		t.Fatalf("doctorCmd() code = %d, stderr = %s", code, errBuf.String())
	}

	out := outBuf.String()
	if !strings.Contains(out, "only in source (not live): "+missing) {
		t.Errorf("output does not flag %q as missing from the live report:\n%s", missing, out)
	}
	if !strings.Contains(out, "only in live (not source): "+extra) {
		t.Errorf("output does not flag %q as extra in the live report:\n%s", extra, out)
	}
}

func TestDoctorCompareNoDriftReportsMatch(t *testing.T) {
	root := moduleRoot(t)
	fixtureDir := filepath.Join(root, "testdata", "example")
	t.Chdir(fixtureDir)

	structs, err := Extract([]string{"./..."}, "")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	seen := map[string]bool{}
	var fields []mamori.FieldStatus
	for _, s := range structs {
		for _, f := range s.Fields {
			if seen[f.Path] {
				continue
			}
			seen[f.Path] = true
			fields = append(fields, mamori.FieldStatus{Path: f.Path, Scheme: "env"})
		}
	}

	body, err := json.Marshal(mamori.Report{Fields: fields, Healthy: true})
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		rw.WriteHeader(http.StatusOK)
		_, _ = rw.Write(body)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	var outBuf, errBuf bytes.Buffer
	code := doctorCmd([]string{"--endpoint", srv.URL, "--insecure", "--compare", "./..."}, &outBuf, &errBuf)
	if code != 0 {
		t.Fatalf("doctorCmd() code = %d, stderr = %s", code, errBuf.String())
	}
	if strings.Contains(outBuf.String(), "only in") {
		t.Errorf("expected no drift, got:\n%s", outBuf.String())
	}
}

// TestStatusOnceMatchesFetchReport exercises statusCmd's non-watch path: a
// single render and exit with the health exit code, the same classification
// fetchReport produces.
func TestStatusOnceMatchesFetchReport(t *testing.T) {
	w := newHealthyWatcher(t)
	srv := httptest.NewServer(mamori.Handler(w))
	defer srv.Close()

	var outBuf, errBuf bytes.Buffer
	code := statusCmd([]string{"--endpoint", srv.URL, "--insecure"}, &outBuf, &errBuf)
	if code != 0 {
		t.Fatalf("statusCmd() code = %d, stderr = %s", code, errBuf.String())
	}
	if !strings.Contains(outBuf.String(), "HEALTHY") {
		t.Errorf("status output missing a HEALTHY summary:\n%s", outBuf.String())
	}
}

func TestStatusOnceUnreachableExit3(t *testing.T) {
	var outBuf, errBuf bytes.Buffer
	code := statusCmd([]string{"--endpoint", "unix:///nonexistent/status-test.sock"}, &outBuf, &errBuf)
	if code != 3 {
		t.Errorf("statusCmd() code = %d, want 3", code)
	}
}
