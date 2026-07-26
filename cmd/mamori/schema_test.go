package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// runSchema runs schemaCmd against the fixture package (testdata/example)
// and returns its stdout, stderr, and exit code, using the same
// t.Chdir-to-the-fixture-module approach as runExplain (explain_test.go):
// Extract has no Dir parameter, and golang.org/x/tools/go/packages resolves
// patterns relative to the process's current working directory.
//
// Unlike runExplain, it takes the fixture directory as an explicit
// parameter rather than deriving it from moduleRoot(t) (== os.Getwd())
// internally: TestSchemaJSON calls this twice (to check determinism), and
// by the second call cwd is already the fixture directory from the first
// t.Chdir, so re-deriving "root" from cwd at that point would compute the
// wrong path.
func runSchema(t *testing.T, fixtureDir string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	t.Chdir(fixtureDir)

	var outBuf, errBuf bytes.Buffer
	code = schemaCmd(args, &outBuf, &errBuf)
	return outBuf.String(), errBuf.String(), code
}

func TestSchemaJSON(t *testing.T) {
	root := moduleRoot(t)
	fixtureDir := filepath.Join(root, "testdata", "example")

	stdout, stderr, code := runSchema(t, fixtureDir, "./...")
	if code != 0 {
		t.Fatalf("schemaCmd() code = %d, stderr = %s", code, stderr)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}

	// The output must be valid JSON.
	var v any
	if err := json.Unmarshal([]byte(stdout), &v); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\n%s", err, stdout)
	}

	compareGolden(t, root, "schema.json.golden", stdout)

	// Deterministic: running again produces byte-identical output.
	stdout2, stderr2, code2 := runSchema(t, fixtureDir, "./...")
	if code2 != 0 {
		t.Fatalf("schemaCmd() (2nd run) code = %d, stderr = %s", code2, stderr2)
	}
	if stdout2 != stdout {
		t.Errorf("schemaCmd() output is not deterministic:\n--- 1st ---\n%s\n--- 2nd ---\n%s", stdout, stdout2)
	}
}

func TestSchemaTypeFilter(t *testing.T) {
	root := moduleRoot(t)
	fixtureDir := filepath.Join(root, "testdata", "example")

	stdout, stderr, code := runSchema(t, fixtureDir, "--type=Config", "./...")
	if code != 0 {
		t.Fatalf("schemaCmd() code = %d, stderr = %s", code, stderr)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}

	// Exactly one struct qualifies, so the output is a single bare schema
	// document (no array wrapper), directly usable as a JSON Schema.
	var doc map[string]any
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("stdout is not a single JSON object: %v\n%s", err, stdout)
	}
	if got, want := doc["$schema"], "https://json-schema.org/draft/2020-12/schema"; got != want {
		t.Errorf("$schema = %v, want %v", got, want)
	}
	if got, want := doc["type"], "object"; got != want {
		t.Errorf("type = %v, want %v", got, want)
	}

	if !strings.Contains(stdout, `"title": "example.com/fixture.Config"`) {
		t.Errorf("stdout missing Config's title:\n%s", stdout)
	}
	if strings.Contains(stdout, "ServerConfig") {
		t.Errorf("stdout unexpectedly includes ServerConfig with --type=Config:\n%s", stdout)
	}
	if strings.Contains(stdout, "RedisConfig") {
		t.Errorf("stdout unexpectedly includes standalone RedisConfig with --type=Config:\n%s", stdout)
	}

	props, ok := doc["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties is not an object: %v", doc["properties"])
	}
	redis, ok := props["Redis"].(map[string]any)
	if !ok {
		t.Fatalf("properties.Redis is not an object: %v", props["Redis"])
	}
	if got, want := redis["type"], "object"; got != want {
		t.Errorf("properties.Redis.type = %v, want %v", got, want)
	}
	redisProps, ok := redis["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties.Redis.properties is not an object: %v", redis["properties"])
	}
	if _, ok := redisProps["Addr"]; !ok {
		t.Errorf("properties.Redis.properties missing Addr: %v", redisProps)
	}

	workers, ok := props["Workers"].(map[string]any)
	if !ok {
		t.Fatalf("properties.Workers is not an object: %v", props["Workers"])
	}
	if got, want := workers["type"], "integer"; got != want {
		t.Errorf("properties.Workers.type = %v, want %v", got, want)
	}
	if got, want := workers["minimum"], 1.0; got != want {
		t.Errorf("properties.Workers.minimum = %v, want %v", got, want)
	}
	if got, want := workers["maximum"], 256.0; got != want {
		t.Errorf("properties.Workers.maximum = %v, want %v", got, want)
	}
	if got, want := workers["default"], 4.0; got != want {
		t.Errorf("properties.Workers.default = %v, want %v", got, want)
	}

	level, ok := props["Level"].(map[string]any)
	if !ok {
		t.Fatalf("properties.Level is not an object: %v", props["Level"])
	}
	enum, ok := level["enum"].([]any)
	if !ok {
		t.Fatalf("properties.Level.enum is not an array: %v", level["enum"])
	}
	if got, want := len(enum), 4; got != want {
		t.Errorf("len(properties.Level.enum) = %d, want %d", got, want)
	}

	serviceName, ok := props["ServiceName"].(map[string]any)
	if !ok {
		t.Fatalf("properties.ServiceName is not an object: %v", props["ServiceName"])
	}
	if got, want := serviceName["minLength"], 3.0; got != want {
		t.Errorf("properties.ServiceName.minLength = %v, want %v", got, want)
	}
	if got, want := serviceName["maxLength"], 64.0; got != want {
		t.Errorf("properties.ServiceName.maxLength = %v, want %v", got, want)
	}

	required, ok := doc["required"].([]any)
	if !ok {
		t.Fatalf("required is not an array: %v", doc["required"])
	}
	requiredSet := map[string]bool{}
	for _, r := range required {
		requiredSet[r.(string)] = true
	}
	for _, want := range []string{"LogLevel", "APIKey", "DBHost", "Level", "Region", "ServiceName"} {
		if !requiredSet[want] {
			t.Errorf("required = %v, want it to include %q", required, want)
		}
	}
	for _, notWant := range []string{"Port", "Workers"} {
		if requiredSet[notWant] {
			t.Errorf("required = %v, did not want it to include %q (has a default)", required, notWant)
		}
	}
}
