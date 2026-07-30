package main

import (
	"strings"
	"testing"
)

func TestRenderDiffTextEmpty(t *testing.T) {
	var sb strings.Builder
	renderDiffText(&sb, Diff{}, "")

	got := sb.String()
	if !strings.Contains(got, "no configuration surface changes") {
		t.Errorf("want an explicit no-change line, got %q", got)
	}
}

func TestRenderDiffTextFieldVerdicts(t *testing.T) {
	d := Diff{Structs: []StructDiff{{
		Package: "acme/svc", TypeName: "Config", Kind: ChangeModified,
		Fields: []FieldDiff{
			{Path: "StripeKey", Kind: ChangeAdded, Field: &Field{
				Path: "StripeKey", GoType: "secret.String",
				Refs: []string{"aws-sm://prod/stripe#key"}, Sensitive: true}},
			{Path: "LegacyKey", Kind: ChangeRemoved, Field: &Field{
				Path: "LegacyKey", GoType: "secret.String",
				Refs: []string{"aws-sm://prod/legacy#key"}, Sensitive: true}},
		},
	}}}

	var sb strings.Builder
	renderDiffText(&sb, d, "")
	got := sb.String()

	if !strings.Contains(got, "acme/svc.Config") {
		t.Errorf("want the struct header, got %q", got)
	}
	if !strings.Contains(got, "+ StripeKey") {
		t.Errorf("want an added marker for StripeKey, got %q", got)
	}
	if !strings.Contains(got, "- LegacyKey") {
		t.Errorf("want a removed marker for LegacyKey, got %q", got)
	}
}

func TestRenderDiffTextCallsOutBecameSensitive(t *testing.T) {
	d := Diff{Structs: []StructDiff{{
		Package: "acme/svc", TypeName: "Config", Kind: ChangeModified,
		Fields: []FieldDiff{{
			Path: "Key", Kind: ChangeModified, BecameSensitive: true,
			Attrs: []AttrChange{{Name: "Sensitive", Base: "false", Head: "true"}},
		}},
	}}}

	var sb strings.Builder
	renderDiffText(&sb, d, "")
	got := sb.String()

	if !strings.Contains(got, "now reads secret material") {
		t.Errorf("want an explicit became-sensitive callout, got %q", got)
	}
}

func TestRenderDiffTextChainChange(t *testing.T) {
	d := Diff{Structs: []StructDiff{{
		Package: "acme/svc", TypeName: "Config", Kind: ChangeModified,
		Fields: []FieldDiff{{
			Path: "Port", Kind: ChangeModified,
			Refs: []RefChange{{Kind: ChangeAdded, Ref: "aws-ps://svc/port", BasePos: -1, HeadPos: 1}},
		}},
	}}}

	var sb strings.Builder
	renderDiffText(&sb, d, "")
	got := sb.String()

	if !strings.Contains(got, "chain +") || !strings.Contains(got, "aws-ps://svc/port") {
		t.Errorf("want a chain addition line, got %q", got)
	}
}

func TestPrivilegeLinesSchemeNeutralByDefault(t *testing.T) {
	d := PrivilegeDelta{
		Added:   map[string][]string{"aws-sm": {"prod/stripe"}},
		Removed: map[string][]string{"vault": {"kv/data/old"}},
	}

	got := strings.Join(privilegeLines(d, ""), "\n")

	if !strings.Contains(got, "+ aws-sm  prod/stripe") {
		t.Errorf("want a neutral added line, got %q", got)
	}
	if !strings.Contains(got, "- vault  kv/data/old") {
		t.Errorf("want a neutral removed line, got %q", got)
	}
	if strings.Contains(got, "arn:aws") {
		t.Errorf("must not render ARNs without --policy-format, got %q", got)
	}
}

func TestPrivilegeLinesAWSIAM(t *testing.T) {
	d := PrivilegeDelta{Added: map[string][]string{
		"aws-sm": {"prod/stripe"},
		"aws-ps": {"/svc/port"},
	}}

	got := strings.Join(privilegeLines(d, "aws-iam"), "\n")

	if !strings.Contains(got, "secretsmanager:GetSecretValue") {
		t.Errorf("want the SM action, got %q", got)
	}
	if !strings.Contains(got, "arn:aws:secretsmanager:*:*:secret:prod/stripe") {
		t.Errorf("want the SM ARN, got %q", got)
	}
	if !strings.Contains(got, "ssm:GetParameter") {
		t.Errorf("want the SSM action, got %q", got)
	}
	if !strings.Contains(got, "arn:aws:ssm:*:*:parameter/svc/port") {
		t.Errorf("want the SSM ARN with no doubled slash, got %q", got)
	}
}

func TestPrivilegeLinesGCP(t *testing.T) {
	d := PrivilegeDelta{Added: map[string][]string{"gcp-sm": {"my-project/api-key"}}}

	got := strings.Join(privilegeLines(d, "gcp"), "\n")

	if !strings.Contains(got, "projects/my-project/secrets/api-key") {
		t.Errorf("want the GCP resource name, got %q", got)
	}
}

func TestPrivilegeLinesKeepsUnrenderableSchemesVisible(t *testing.T) {
	d := PrivilegeDelta{Added: map[string][]string{
		"aws-sm": {"prod/stripe"},
		"vault":  {"kv/data/api"},
	}}

	got := strings.Join(privilegeLines(d, "aws-iam"), "\n")

	// vault has no IAM vocabulary but must not vanish from the report.
	if !strings.Contains(got, "kv/data/api") {
		t.Errorf("want the vault path still visible under --policy-format=aws-iam, got %q", got)
	}
}

func TestRenderDiffTextIsDeterministic(t *testing.T) {
	d := Diff{
		Structs: []StructDiff{{
			Package: "acme/svc", TypeName: "Config", Kind: ChangeModified,
			Fields: []FieldDiff{{Path: "A", Kind: ChangeModified,
				Attrs: []AttrChange{{Name: "GoType", Base: "int", Head: "int64"}}}},
		}},
		Privilege: PrivilegeDelta{Added: map[string][]string{
			"aws-sm": {"a", "b"}, "vault": {"c"}, "gcp-sm": {"d/e"},
		}},
	}

	var first, second strings.Builder
	renderDiffText(&first, d, "")
	renderDiffText(&second, d, "")

	if first.String() != second.String() {
		t.Errorf("renderDiffText is not deterministic:\nfirst:\n%s\nsecond:\n%s", first.String(), second.String())
	}
}
