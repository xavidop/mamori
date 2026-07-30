package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	baseFixture      = "testdata/diff/base.json"
	headFixture      = "testdata/diff/head.json"
	headMinusFixture = "testdata/diff/head-minus.json"
	notArrayFixture  = "testdata/diff/notarray.json"
)

func runDiff(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut strings.Builder
	code = diffCmd(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestDiffCmdHelpIsSuccessOnStdout(t *testing.T) {
	code, stdout, stderr := runDiff(t, "--help")

	if code != 0 {
		t.Errorf("want exit 0 for help, got %d", code)
	}
	if !strings.Contains(stdout, "usage: mamori diff") {
		t.Errorf("want usage on stdout, got %q", stdout)
	}
	if stderr != "" {
		t.Errorf("want empty stderr for help, got %q", stderr)
	}
}

func TestDiffCmdDefaultExitsZeroDespiteFindings(t *testing.T) {
	code, stdout, _ := runDiff(t, baseFixture, headFixture)

	if code != 0 {
		t.Errorf("want exit 0 without --exit-code, got %d", code)
	}
	if !strings.Contains(stdout, "StripeKey") {
		t.Errorf("want the added field reported, got %q", stdout)
	}
}

func TestDiffCmdExitCodeAny(t *testing.T) {
	code, _, _ := runDiff(t, baseFixture, headFixture, "--exit-code=any")
	if code != 2 {
		t.Errorf("want exit 2 when findings exist with --exit-code=any, got %d", code)
	}
}

func TestDiffCmdBareExitCodeMeansAny(t *testing.T) {
	code, _, _ := runDiff(t, baseFixture, headFixture, "--exit-code")
	if code != 2 {
		t.Errorf("want a bare --exit-code to behave as any, got %d", code)
	}
}

func TestDiffCmdExitCodeAnyOnIdenticalInput(t *testing.T) {
	code, _, _ := runDiff(t, baseFixture, baseFixture, "--exit-code=any")
	if code != 0 {
		t.Errorf("want exit 0 when nothing changed, got %d", code)
	}
}

func TestDiffCmdExitCodePrivilegeSignalsOnGrowth(t *testing.T) {
	code, _, _ := runDiff(t, baseFixture, headFixture, "--exit-code=privilege")
	if code != 2 {
		t.Errorf("want exit 2 when the privilege surface grew, got %d", code)
	}
}

func TestDiffCmdExitCodePrivilegeIgnoresShrink(t *testing.T) {
	// head to head-minus is a pure shrink: aws-sm prod/stripe is lost and
	// nothing is gained. Reversing base and head would NOT be a pure shrink,
	// because base carries prod/legacy, which head lacks.
	code, _, _ := runDiff(t, headFixture, headMinusFixture, "--exit-code=privilege")
	if code != 0 {
		t.Errorf("want exit 0 when the privilege surface only shrank, got %d", code)
	}
}

func TestDiffCmdExitCodeAnyStillSignalsOnPureShrink(t *testing.T) {
	// The same pair under --exit-code=any IS a finding: something changed.
	code, _, _ := runDiff(t, headFixture, headMinusFixture, "--exit-code=any")
	if code != 2 {
		t.Errorf("want exit 2 for a shrink under --exit-code=any, got %d", code)
	}
}

func TestDiffCmdMissingFileIsExitOne(t *testing.T) {
	code, _, stderr := runDiff(t, filepath.Join("testdata", "diff", "nope.json"), headFixture)

	if code != 1 {
		t.Errorf("want exit 1 for a missing file, got %d", code)
	}
	if !strings.Contains(stderr, "nope.json") {
		t.Errorf("want the failing path named on stderr, got %q", stderr)
	}
}

func TestDiffCmdNonArrayJSONIsExitOne(t *testing.T) {
	code, _, stderr := runDiff(t, notArrayFixture, headFixture)

	if code != 1 {
		t.Errorf("want exit 1 for a non-array top level, got %d", code)
	}
	if !strings.Contains(stderr, notArrayFixture) {
		t.Errorf("want the offending file named, got %q", stderr)
	}
}

func TestDiffCmdWrongOperandCountIsExitOne(t *testing.T) {
	code, _, stderr := runDiff(t, baseFixture)

	if code != 1 {
		t.Errorf("want exit 1 for one operand, got %d", code)
	}
	if !strings.Contains(stderr, "usage: mamori diff") {
		t.Errorf("want usage echoed on stderr, got %q", stderr)
	}
}

func TestDiffCmdJSONAndMarkdownAreMutuallyExclusive(t *testing.T) {
	code, _, stderr := runDiff(t, baseFixture, headFixture, "--json", "--markdown")

	if code != 1 {
		t.Errorf("want exit 1 for conflicting formats, got %d", code)
	}
	if !strings.Contains(stderr, "mutually exclusive") {
		t.Errorf("want an explicit conflict message, got %q", stderr)
	}
}

func TestDiffCmdUnknownFlagIsExitOne(t *testing.T) {
	code, _, stderr := runDiff(t, baseFixture, headFixture, "--nope")

	if code != 1 {
		t.Errorf("want exit 1 for an unknown flag, got %d", code)
	}
	if !strings.Contains(stderr, "--nope") {
		t.Errorf("want the unknown flag named, got %q", stderr)
	}
}

func TestDiffCmdBadExitCodeValueIsExitOne(t *testing.T) {
	code, _, stderr := runDiff(t, baseFixture, headFixture, "--exit-code=maybe")

	if code != 1 {
		t.Errorf("want exit 1 for a bad --exit-code value, got %d", code)
	}
	if !strings.Contains(stderr, "maybe") {
		t.Errorf("want the bad value named, got %q", stderr)
	}
}

func TestDiffCmdBadPolicyFormatIsExitOne(t *testing.T) {
	code, _, stderr := runDiff(t, baseFixture, headFixture, "--policy-format=azure")

	if code != 1 {
		t.Errorf("want exit 1 for an unsupported policy format, got %d", code)
	}
	if !strings.Contains(stderr, "azure") {
		t.Errorf("want the bad format named, got %q", stderr)
	}
}

func TestDiffCmdMarkdownOutput(t *testing.T) {
	code, stdout, _ := runDiff(t, baseFixture, headFixture, "--markdown")

	if code != 0 {
		t.Fatalf("want exit 0, got %d", code)
	}
	if !strings.Contains(stdout, "### `acme/svc.Config`") {
		t.Errorf("want markdown output, got %q", stdout)
	}
}

func TestDiffCmdJSONOutput(t *testing.T) {
	code, stdout, _ := runDiff(t, baseFixture, headFixture, "--json")

	if code != 0 {
		t.Fatalf("want exit 0, got %d", code)
	}
	if !strings.HasPrefix(strings.TrimSpace(stdout), "{") {
		t.Errorf("want a JSON object, got %q", stdout)
	}
}

func TestDiffCmdStdinOperand(t *testing.T) {
	data, err := os.ReadFile(headFixture)
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}

	var out, errOut strings.Builder
	c := &diffConfig{stdin: bytes.NewReader(data)}
	code := c.run([]string{baseFixture, "-"}, &out, &errOut)

	if code != 0 {
		t.Fatalf("want exit 0, got %d (stderr: %q)", code, errOut.String())
	}
	if !strings.Contains(out.String(), "StripeKey") {
		t.Errorf("want the head read from stdin, got %q", out.String())
	}
}

func TestDiffCmdRejectsTwoStdinOperands(t *testing.T) {
	code, _, stderr := runDiff(t, "-", "-")

	if code != 1 {
		t.Errorf("want exit 1 for two stdin operands, got %d", code)
	}
	if !strings.Contains(stderr, "only one operand") {
		t.Errorf("want an explicit message, got %q", stderr)
	}
}

func TestDiffCmdPolicyFormatThreadsToARenderer(t *testing.T) {
	code, stdout, _ := runDiff(t, baseFixture, headFixture, "--policy-format=aws-iam")

	if code != 0 {
		t.Fatalf("want exit 0, got %d", code)
	}
	if !strings.Contains(stdout, "arn:aws:secretsmanager:*:*:secret:prod/stripe") {
		t.Errorf("want a real ARN threaded through to the renderer, got %q", stdout)
	}
}

func TestDiffCmdPolicyFormatSpaceSeparatedMatchesEquals(t *testing.T) {
	_, eqOut, eqErr := runDiff(t, baseFixture, headFixture, "--policy-format=aws-iam")
	_, spaceOut, spaceErr := runDiff(t, baseFixture, headFixture, "--policy-format", "aws-iam")

	if eqOut != spaceOut {
		t.Errorf("want --policy-format aws-iam to match --policy-format=aws-iam on stdout\n=: %q\nspace: %q", eqOut, spaceOut)
	}
	if eqErr != spaceErr {
		t.Errorf("want --policy-format aws-iam to match --policy-format=aws-iam on stderr\n=: %q\nspace: %q", eqErr, spaceErr)
	}
}

func TestDiffCmdMarkdownWithExitCodeAnyRendersAndSignals(t *testing.T) {
	code, stdout, _ := runDiff(t, baseFixture, headFixture, "--markdown", "--exit-code=any")

	if code != 2 {
		t.Errorf("want exit 2, got %d", code)
	}
	if !strings.Contains(stdout, "### `acme/svc.Config`") {
		t.Errorf("want markdown still rendered even though the exit code signals findings, got %q", stdout)
	}
}

func TestRunDispatchesDiff(t *testing.T) {
	// `mamori diff` with no operands must reach diffCmd (exit 1), not the
	// top-level unknown-subcommand path (exit 2).
	if code := run([]string{"diff", "--help"}); code != 0 {
		t.Errorf("want run() to dispatch diff --help to exit 0, got %d", code)
	}
}
