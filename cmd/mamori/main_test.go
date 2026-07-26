package main

import "testing"

func TestUnknownSubcommandExits2(t *testing.T) {
	if got := run([]string{"bogus"}); got != 2 {
		t.Errorf("run([bogus]) = %d, want 2", got)
	}
	if got := run(nil); got != 2 {
		t.Errorf("run(nil) = %d, want 2", got)
	}
}

func TestVersionSubcommand(t *testing.T) {
	if got := run([]string{"version"}); got != 0 {
		t.Errorf("run([version]) = %d, want 0", got)
	}
	if got := run([]string{"--version"}); got != 0 {
		t.Errorf("run([--version]) = %d, want 0", got)
	}
}

func TestHelpSubcommand(t *testing.T) {
	if got := run([]string{"help"}); got != 0 {
		t.Errorf("run([help]) = %d, want 0", got)
	}
	if got := run([]string{"--help"}); got != 0 {
		t.Errorf("run([--help]) = %d, want 0", got)
	}
}

func TestStubSubcommandsReturnZero(t *testing.T) {
	// explain and schema are real commands now (tasks 3-4), but a bare
	// invocation (no --type, no flags at all) still succeeds: patterns
	// default to the current directory, which has no source-tagged
	// structs, so both return an empty result and exit 0, matching a
	// stub's exit code without being one. doctor/status are no longer
	// stubs either (task 6): see TestDoctorSubcommandRequiresEndpoint and
	// TestStatusSubcommandRequiresEndpoint below for why they were
	// removed from this loop.
	for _, sub := range []string{"explain", "schema"} {
		if got := run([]string{sub}); got != 0 {
			t.Errorf("run([%s]) = %d, want 0", sub, got)
		}
	}
}

// TestDoctorSubcommandRequiresEndpoint documents why "doctor" was removed
// from TestStubSubcommandsReturnZero above (task 6): doctor is a live
// command, a thin client of a running process's admin endpoint (client.go).
// With no --endpoint at all there is nothing to connect to, which
// classifies as exit 3 (never got an HTTP response), the same code any
// other unreachable endpoint produces, not a silent success.
func TestDoctorSubcommandRequiresEndpoint(t *testing.T) {
	if got := run([]string{"doctor"}); got != 3 {
		t.Errorf("run([doctor]) = %d, want 3 (no --endpoint)", got)
	}
}

// TestStatusSubcommandRequiresEndpoint is status's counterpart to
// TestDoctorSubcommandRequiresEndpoint above.
func TestStatusSubcommandRequiresEndpoint(t *testing.T) {
	if got := run([]string{"status"}); got != 3 {
		t.Errorf("run([status]) = %d, want 3 (no --endpoint)", got)
	}
}

// TestPolicySubcommandRequiresFormat documents why "policy" was removed
// from TestStubSubcommandsReturnZero above (task 5): unlike
// explain/schema, policy has no sensible "do nothing, report success"
// behavior for a bare invocation, since --format is required and there is
// no default format to fall back to (see policy.go's policyUsage: emitting
// an artifact for an arbitrarily-chosen default format would not be
// "nothing happened", it would be a wrong artifact silently produced).
// isSupportedPolicyFormat treats a missing --format the same as an unknown
// one, so this is exit 2, the same usage-error code main.go's own
// unknown-subcommand path uses.
func TestPolicySubcommandRequiresFormat(t *testing.T) {
	if got := run([]string{"policy"}); got != 2 {
		t.Errorf("run([policy]) = %d, want 2 (missing --format)", got)
	}
}
