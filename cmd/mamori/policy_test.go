package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// runPolicy runs policyCmd against the fixture package (testdata/example)
// and returns its stdout, stderr, and exit code, using the same
// t.Chdir-to-the-fixture-module approach as runExplain/runSchema
// (explain_test.go, schema_test.go): Extract has no Dir parameter, and
// golang.org/x/tools/go/packages resolves patterns relative to the
// process's current working directory.
func runPolicy(t *testing.T, fixtureDir string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	enterFixtureModule(t, fixtureDir)

	var outBuf, errBuf bytes.Buffer
	code = policyCmd(args, &outBuf, &errBuf)
	return outBuf.String(), errBuf.String(), code
}

func TestPolicyAWSIAM(t *testing.T) {
	root := moduleRoot(t)
	fixtureDir := filepath.Join(root, "testdata", "example")

	stdout, stderr, code := runPolicy(t, fixtureDir, "--format=aws-iam", "--type=Config", "./...")
	if code != 0 {
		t.Fatalf("policyCmd() code = %d, stderr = %s", code, stderr)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty (Config has aws-sm/aws-ps refs)", stderr)
	}

	var doc map[string]any
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\n%s", err, stdout)
	}
	if got, want := doc["Version"], "2012-10-17"; got != want {
		t.Errorf("Version = %v, want %v", got, want)
	}
	statements, ok := doc["Statement"].([]any)
	if !ok {
		t.Fatalf("Statement is not an array: %v", doc["Statement"])
	}
	if len(statements) != 2 {
		t.Fatalf("len(Statement) = %d, want 2 (one secretsmanager, one ssm)", len(statements))
	}

	compareGolden(t, root, "policy.aws-iam.golden", stdout)

	// Deterministic: running again produces byte-identical output.
	stdout2, _, code2 := runPolicy(t, fixtureDir, "--format=aws-iam", "--type=Config", "./...")
	if code2 != 0 {
		t.Fatalf("policyCmd() (2nd run) code = %d", code2)
	}
	if stdout2 != stdout {
		t.Errorf("policyCmd() output is not deterministic:\n--- 1st ---\n%s\n--- 2nd ---\n%s", stdout, stdout2)
	}
}

func TestPolicyGCP(t *testing.T) {
	root := moduleRoot(t)
	fixtureDir := filepath.Join(root, "testdata", "example")

	stdout, stderr, code := runPolicy(t, fixtureDir, "--format=gcp", "--type=Config", "./...")
	if code != 0 {
		t.Fatalf("policyCmd() code = %d, stderr = %s", code, stderr)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty (Config has a gcp-sm ref)", stderr)
	}

	var doc map[string]any
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\n%s", err, stdout)
	}
	if got, want := doc["role"], "roles/secretmanager.secretAccessor"; got != want {
		t.Errorf("role = %v, want %v", got, want)
	}
	resources, ok := doc["resources"].([]any)
	if !ok {
		t.Fatalf("resources is not an array: %v", doc["resources"])
	}
	if len(resources) != 1 || resources[0] != "projects/acme-prod/secrets/db-password" {
		t.Errorf("resources = %v, want [projects/acme-prod/secrets/db-password]", resources)
	}

	compareGolden(t, root, "policy.gcp.golden", stdout)
}

func TestPolicyExternalSecret(t *testing.T) {
	root := moduleRoot(t)
	fixtureDir := filepath.Join(root, "testdata", "example")

	stdout, stderr, code := runPolicy(t, fixtureDir, "--format=external-secret", "--type=Config", "./...")
	if code != 0 {
		t.Fatalf("policyCmd() code = %d, stderr = %s", code, stderr)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty (Config has aws-sm/aws-ps/gcp-sm refs)", stderr)
	}

	if !strings.HasPrefix(stdout, "apiVersion: external-secrets.io/v1\n") {
		t.Errorf("stdout missing apiVersion header:\n%s", stdout)
	}
	if !strings.Contains(stdout, "kind: ExternalSecret\n") {
		t.Errorf("stdout missing kind:\n%s", stdout)
	}
	if !strings.Contains(stdout, "remoteRef:\n") {
		t.Errorf("stdout missing remoteRef:\n%s", stdout)
	}

	compareGolden(t, root, "policy.external-secret.golden", stdout)
}

func TestPolicyUnknownFormatExits2(t *testing.T) {
	root := moduleRoot(t)
	fixtureDir := filepath.Join(root, "testdata", "example")

	stdout, stderr, code := runPolicy(t, fixtureDir, "--format=bogus", "./...")
	if code != 2 {
		t.Errorf("policyCmd() code = %d, want 2", code)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	for _, want := range []string{"aws-iam", "gcp", "external-secret"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr = %q, want it to mention supported format %q", stderr, want)
		}
	}
}

func TestPolicyMissingFormatExits2(t *testing.T) {
	root := moduleRoot(t)
	fixtureDir := filepath.Join(root, "testdata", "example")

	stdout, stderr, code := runPolicy(t, fixtureDir, "./...")
	if code != 2 {
		t.Errorf("policyCmd() code = %d, want 2", code)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	if stderr == "" {
		t.Errorf("stderr is empty, want a note that --format is required/unsupported")
	}
}

func TestPolicyNoRelevantRefsIsEmptyWithNote(t *testing.T) {
	root := moduleRoot(t)
	fixtureDir := filepath.Join(root, "testdata", "example")

	stdout, stderr, code := runPolicy(t, fixtureDir, "--format=aws-iam", "--type=ServerConfig", "./...")
	if code != 0 {
		t.Fatalf("policyCmd() code = %d, stderr = %s", code, stderr)
	}
	if stderr == "" {
		t.Errorf("stderr is empty, want a note that no relevant refs were found")
	}
	if !strings.Contains(stderr, "aws-iam") {
		t.Errorf("stderr = %q, want it to mention the format", stderr)
	}

	var doc map[string]any
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\n%s", err, stdout)
	}
	if got, want := doc["Version"], "2012-10-17"; got != want {
		t.Errorf("Version = %v, want %v (still a valid, if empty, IAM document)", got, want)
	}
	statements, ok := doc["Statement"].([]any)
	if !ok {
		t.Fatalf("Statement is not an array: %v", doc["Statement"])
	}
	if len(statements) != 0 {
		t.Errorf("Statement = %v, want empty (ServerConfig has no aws-sm/aws-ps refs)", statements)
	}
}

func TestPolicyNoRelevantRefsGCPIsEmptyWithNote(t *testing.T) {
	root := moduleRoot(t)
	fixtureDir := filepath.Join(root, "testdata", "example")

	stdout, stderr, code := runPolicy(t, fixtureDir, "--format=gcp", "--type=ServerConfig", "./...")
	if code != 0 {
		t.Fatalf("policyCmd() code = %d, stderr = %s", code, stderr)
	}
	if stderr == "" {
		t.Errorf("stderr is empty, want a note that no relevant refs were found")
	}

	var doc map[string]any
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\n%s", err, stdout)
	}
	resources, ok := doc["resources"].([]any)
	if !ok {
		t.Fatalf("resources is not an array: %v", doc["resources"])
	}
	if len(resources) != 0 {
		t.Errorf("resources = %v, want empty", resources)
	}
}

func TestPolicyNoRelevantRefsExternalSecretIsEmptyWithNote(t *testing.T) {
	root := moduleRoot(t)
	fixtureDir := filepath.Join(root, "testdata", "example")

	stdout, stderr, code := runPolicy(t, fixtureDir, "--format=external-secret", "--type=ServerConfig", "./...")
	if code != 0 {
		t.Fatalf("policyCmd() code = %d, stderr = %s", code, stderr)
	}
	if stderr == "" {
		t.Errorf("stderr is empty, want a note that no relevant refs were found")
	}
	if !strings.Contains(stdout, "data: []\n") {
		t.Errorf("stdout missing empty data list:\n%s", stdout)
	}
}
