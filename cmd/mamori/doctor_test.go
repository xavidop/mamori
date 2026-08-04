package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	enterFixtureModule(t, fixtureDir)

	structs, err := Extract([]string{"./..."}, "", nil)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	// Only KindSource fields, for the same reason
	// TestDoctorCompareNoDriftReportsMatch filters: doctor --compare builds its
	// source-side path set from the fields that can actually be resolved to a
	// live value, so a validate-only or WithDerive-declared path is never in it
	// and can never be reported "only in source". Taking the last path of an
	// unfiltered list picked whichever kind the fixture happened to declare
	// last, which made this test depend on fixture declaration order.
	var sourcePaths []string
	seen := map[string]bool{}
	for _, s := range structs {
		for _, f := range s.Fields {
			if f.Kind != KindSource {
				continue
			}
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
	enterFixtureModule(t, fixtureDir)

	structs, err := Extract([]string{"./..."}, "", nil)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	// Only a KindSource field is ever resolved to a live value, so only those
	// belong in a synthetic "live" Report: a validate-only field would
	// otherwise show up here (Extract now returns it too) and falsely report
	// as live-only drift.
	seen := map[string]bool{}
	var fields []mamori.FieldStatus
	for _, s := range structs {
		for _, f := range s.Fields {
			if f.Kind != KindSource {
				continue
			}
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

// TestDoctorCompareIgnoresDeclaredDerivedField is the regression test for a
// review finding: a live report's Derived FieldStatus entries used to be
// compared against the source-tagged field set at face value, so a declared
// WithDerive write path - which by construction carries no `source` tag and
// therefore can never appear in Extract's output, however correctly it is
// configured - was always flagged "only in live (not source)": permanent,
// unfixable false drift on an otherwise perfectly healthy process.
// runCompare now excludes a Derived live field from the comparison entirely.
func TestDoctorCompareIgnoresDeclaredDerivedField(t *testing.T) {
	root := moduleRoot(t)
	fixtureDir := filepath.Join(root, "testdata", "example")
	enterFixtureModule(t, fixtureDir)

	structs, err := Extract([]string{"./..."}, "", nil)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	// Only a KindSource field is ever resolved to a live value; see the same
	// filter's comment in TestDoctorCompareNoDriftReportsMatch.
	seen := map[string]bool{}
	var fields []mamori.FieldStatus
	for _, s := range structs {
		for _, f := range s.Fields {
			if f.Kind != KindSource {
				continue
			}
			if seen[f.Path] {
				continue
			}
			seen[f.Path] = true
			fields = append(fields, mamori.FieldStatus{Path: f.Path, Scheme: "env"})
		}
	}
	// A declared derive write path: no source tag anywhere in the fixture, by
	// construction, so it must never be reported as drift.
	const derivedPath = "DSN"
	fields = append(fields, mamori.FieldStatus{Path: derivedPath, Derived: true})

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
	if strings.Contains(out, derivedPath) && strings.Contains(out, "only in live") {
		t.Errorf("a declared derived field was reported as live-only drift, want it excluded entirely:\n%s", out)
	}
	if strings.Contains(out, "only in") {
		t.Errorf("expected no drift once the derived field is excluded from the comparison, got:\n%s", out)
	}
}

// TestDoctorCompareIgnoresValidateOnlyField pins that widening Extract does not
// make --compare report drift. A validate-only field never appears in a live
// report, so an unfiltered source set calls it "only in source" on a healthy
// config.
func TestDoctorCompareIgnoresValidateOnlyField(t *testing.T) {
	root := moduleRoot(t)
	fixtureDir := filepath.Join(root, "testdata", "example")
	enterFixtureModule(t, fixtureDir)

	structs, err := Extract([]string{"./..."}, "", nil)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	// Build a live Report containing only the fixture's source-tagged paths:
	// a real live process never resolves anything for a validate-only field
	// (it has no ref), so it is never part of an actual Report either.
	seen := map[string]bool{}
	var fields []mamori.FieldStatus
	for _, s := range structs {
		for _, f := range s.Fields {
			if f.Kind != KindSource {
				continue
			}
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
	out := outBuf.String()
	if strings.Contains(out, "Computed") {
		t.Errorf("a validate-only field was reported as drift, want it excluded entirely:\n%s", out)
	}
	if strings.Contains(out, "only in") {
		t.Errorf("expected no drift once the validate-only field is excluded from the comparison, got:\n%s", out)
	}
}

// TestDoctorTableRendersDerivedColumn is the regression test for the other
// half of the same review finding: writeReportTable rendered a Derived row
// with blank SCHEME/REF/VERSION and no column explaining why, which reads as
// a misconfigured or half-broken source field rather than a field that was
// never supposed to have a ref in the first place. A DERIVED column now
// makes that explicit for every row.
func TestDoctorTableRendersDerivedColumn(t *testing.T) {
	body, err := json.Marshal(mamori.Report{
		Fields: []mamori.FieldStatus{
			{Path: "Level", Scheme: "env", Ref: "env://LEVEL", Version: "v1"},
			{Path: "DSN", Derived: true},
		},
		Healthy: true,
	})
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
	code := doctorCmd([]string{"--endpoint", srv.URL, "--insecure"}, &outBuf, &errBuf)
	if code != 0 {
		t.Fatalf("doctorCmd() code = %d, stderr = %s", code, errBuf.String())
	}
	out := outBuf.String()
	if !strings.Contains(out, "DERIVED") {
		t.Fatalf("table header missing a DERIVED column:\n%s", out)
	}

	var dsnLine, levelLine string
	for _, l := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(l, "DSN"):
			dsnLine = l
		case strings.HasPrefix(l, "Level"):
			levelLine = l
		}
	}
	if dsnLine == "" {
		t.Fatalf("no row rendered for DSN:\n%s", out)
	}
	if !strings.HasSuffix(strings.TrimRight(dsnLine, " \t"), "true") {
		t.Errorf("DSN row does not end with DERIVED=true: %q", dsnLine)
	}
	if levelLine == "" {
		t.Fatalf("no row rendered for Level:\n%s", out)
	}
	if !strings.HasSuffix(strings.TrimRight(levelLine, " \t"), "false") {
		t.Errorf("Level row does not end with DERIVED=false: %q", levelLine)
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

// TestDecodeReportToleratesAVersionSkewBothWays pins that the CLI and the
// process it points at may run different mamori versions. They are separate
// binaries on separate release cadences, and an operator debugging an incident
// runs whichever CLI they have against whichever the pod was built with.
func TestDecodeReportToleratesAVersionSkewBothWays(t *testing.T) {
	base := `"Fields":[],"Snapshot":1,"Live":1,"Pinned":false,"Healthy":true,"GeneratedAt":"2026-01-01T00:00:00Z"`
	tests := []struct {
		name string
		body string
	}{
		{"a process older than this CLI", "{" + base + "}"},
		{"a process this CLI matches", "{" + base + `,"Source":"backend","Bootstrap":null}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := decodeReport([]byte(tt.body)); err != nil {
				t.Fatalf("decodeReport: %v", err)
			}
		})
	}
}

// TestDecodeReportStillRejectsANonReport pins that tolerating an unknown
// optional key did not turn the shape check into "accept any JSON object".
func TestDecodeReportStillRejectsANonReport(t *testing.T) {
	for _, body := range []string{`{}`, `{"hello":"world"}`, `{"Fields":[],"Snapshot":1}`} {
		if _, err := decodeReport([]byte(body)); err == nil {
			t.Fatalf("decodeReport(%s) = nil error, want a rejection", body)
		}
	}
	// A key in neither the required nor the optional set is still refused.
	full := `{"Fields":[],"Snapshot":1,"Live":1,"Pinned":false,"Healthy":true,"GeneratedAt":"2026-01-01T00:00:00Z","Surprise":1}`
	if _, err := decodeReport([]byte(full)); err == nil {
		t.Fatal("decodeReport accepted an unknown top-level field")
	}
}

// TestDoctorTableReportsTheBootstrapCache pins that a process serving a
// restored snapshot says so in the CLI, which is otherwise indistinguishable
// from a healthy one line for line.
func TestDoctorTableReportsTheBootstrapCache(t *testing.T) {
	tests := []struct {
		name   string
		report mamori.Report
		want   string
	}{
		{
			name: "serving from the snapshot",
			report: mamori.Report{
				Healthy: true, Source: mamori.SourceBootstrapCache,
				Bootstrap: &mamori.BootstrapStatus{Present: true, Restored: true, Age: 2 * time.Hour, FingerprintMatch: true},
			},
			want: "BOOTSTRAP CACHE: serving a snapshot written 2h0m0s ago",
		},
		{
			name: "configured with no snapshot yet",
			report: mamori.Report{
				Healthy: true, Source: mamori.SourceBackend,
				Bootstrap: &mamori.BootstrapStatus{Problem: "no snapshot has been written"},
			},
			want: "bootstrap cache: no snapshot (no snapshot has been written)",
		},
		{
			name: "a drifted snapshot",
			report: mamori.Report{
				Healthy: true, Source: mamori.SourceBackend,
				Bootstrap: &mamori.BootstrapStatus{Present: true, Age: time.Minute},
			},
			want: "for a DIFFERENT config struct",
		},
		{
			name:   "not configured at all",
			report: mamori.Report{Healthy: true, Source: mamori.SourceBackend},
			want:   "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			rep := tt.report
			writeReportTable(&out, &rep)
			switch {
			case tt.want == "" && strings.Contains(out.String(), "bootstrap"):
				t.Fatalf("output mentions the bootstrap cache for a process not using one:\n%s", out.String())
			case tt.want != "" && !strings.Contains(out.String(), tt.want):
				t.Fatalf("output missing %q:\n%s", tt.want, out.String())
			}
		})
	}
}
