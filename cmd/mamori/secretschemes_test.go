package main

import (
	"strings"
	"testing"
)

// The --secret-schemes flag is shared by every command that decides whether a
// field is sensitive (see secretschemes.go). These tests pin the shared
// helpers, and each command's own parser is checked below for the wiring.

func TestMatchSecretSchemesForms(t *testing.T) {
	tests := []struct {
		name         string
		args         []string
		wantValue    string
		wantConsumed int
	}{
		{"long equals", []string{"--secret-schemes=a"}, "a", 1},
		{"short equals", []string{"-secret-schemes=a"}, "a", 1},
		{"long separate", []string{"--secret-schemes", "a"}, "a", 2},
		{"short separate", []string{"-secret-schemes", "a"}, "a", 2},
		{"not this flag", []string{"--type=Config"}, "", 0},
		{"a pattern", []string{"./..."}, "", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			value, consumed, err := matchSecretSchemes("test", tt.args, 0)
			if err != nil {
				t.Fatalf("matchSecretSchemes(%v) error: %v", tt.args, err)
			}
			if value != tt.wantValue || consumed != tt.wantConsumed {
				t.Errorf("matchSecretSchemes(%v) = (%q, %d), want (%q, %d)",
					tt.args, value, consumed, tt.wantValue, tt.wantConsumed)
			}
		})
	}

	if _, _, err := matchSecretSchemes("test", []string{"--secret-schemes"}, 0); err == nil {
		t.Error("a trailing --secret-schemes with no value = nil error, want an error")
	}
}

func TestSecretSchemeSet(t *testing.T) {
	// Absent flag means nil, so Extract keeps using the shared built-in set
	// rather than building a fresh map per call.
	set, err := secretSchemeSet("test", "")
	if err != nil {
		t.Fatalf("secretSchemeSet(\"\") error: %v", err)
	}
	if set != nil {
		t.Errorf("secretSchemeSet(\"\") = %v, want nil", set.Sorted())
	}

	set, err = secretSchemeSet("test", "mysecrets,corp-kv")
	if err != nil {
		t.Fatalf("secretSchemeSet error: %v", err)
	}
	for _, want := range []string{"mysecrets", "corp-kv"} {
		if !set.Contains(want) {
			t.Errorf("set is missing the added scheme %q: %v", want, set.Sorted())
		}
	}
	// Adding must extend the built-ins, never replace them.
	if !set.Contains("vault") {
		t.Errorf("set dropped the built-in schemes: %v", set.Sorted())
	}

	if _, err := secretSchemeSet("test", "mysecrets://prod"); err == nil {
		t.Error("a full ref was accepted as a scheme token, want an error")
	}
}

// TestEveryReadingCommandAcceptsSecretSchemes is the anti-drift check: each
// command that computes sensitivity from the scheme set must accept the flag.
// Adding a new such command without wiring the flag would leave an operator
// with one command disagreeing with the others about what is a secret.
func TestEveryReadingCommandAcceptsSecretSchemes(t *testing.T) {
	const list = "mysecrets"

	if _, _, _, schemes, err := parseExplainArgs([]string{"--secret-schemes=" + list}); err != nil {
		t.Errorf("explain: %v", err)
	} else if !schemes.Contains(list) {
		t.Error("explain did not apply --secret-schemes")
	}

	if _, _, schemes, err := parseSchemaArgs([]string{"--secret-schemes=" + list}); err != nil {
		t.Errorf("schema: %v", err)
	} else if !schemes.Contains(list) {
		t.Error("schema did not apply --secret-schemes")
	}

	if _, _, _, schemes, err := parsePolicyArgs([]string{"--secret-schemes=" + list}); err != nil {
		t.Errorf("policy: %v", err)
	} else if !schemes.Contains(list) {
		t.Error("policy did not apply --secret-schemes")
	}

	// vet drives the analyzer's own FlagSet, so it carries the raw list.
	if _, schemes, err := parseVetArgs([]string{"--secret-schemes=" + list}); err != nil {
		t.Errorf("vet: %v", err)
	} else if schemes != list {
		t.Errorf("vet schemes = %q, want %q", schemes, list)
	}
}

// TestSchemaAndPolicyRejectBadSchemeList checks the fail-loudly rule reaches
// the commands wired up in this round, not just the shared helper.
func TestSchemaAndPolicyRejectBadSchemeList(t *testing.T) {
	if _, _, _, err := parseSchemaArgs([]string{"--secret-schemes=mysecrets://prod"}); err == nil {
		t.Error("schema accepted a full ref as a scheme token")
	} else if !strings.Contains(err.Error(), "mamori schema") {
		t.Errorf("schema error does not name the command: %v", err)
	}

	if _, _, _, _, err := parsePolicyArgs([]string{"--secret-schemes=mysecrets://prod"}); err == nil {
		t.Error("policy accepted a full ref as a scheme token")
	} else if !strings.Contains(err.Error(), "mamori policy") {
		t.Errorf("policy error does not name the command: %v", err)
	}
}

// TestSecretSchemesDoesNotSwallowPatterns checks the flag's separate-argument
// form consumes exactly its value, leaving patterns and other flags intact.
func TestSecretSchemesDoesNotSwallowPatterns(t *testing.T) {
	patterns, typeName, schemes, err := parseSchemaArgs(
		[]string{"--secret-schemes", "mysecrets", "--type=Config", "./..."})
	if err != nil {
		t.Fatalf("parseSchemaArgs: %v", err)
	}
	if typeName != "Config" {
		t.Errorf("typeName = %q, want Config", typeName)
	}
	if len(patterns) != 1 || patterns[0] != "./..." {
		t.Errorf("patterns = %v, want [./...]", patterns)
	}
	if !schemes.Contains("mysecrets") {
		t.Error("scheme list was not applied")
	}
}
