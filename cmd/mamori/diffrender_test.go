package main

import (
	"encoding/json"
	"reflect"
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

func TestRenderDiffJSONRoundTrips(t *testing.T) {
	d := Diff{
		Structs: []StructDiff{{
			Package: "acme/svc", TypeName: "Config", Kind: ChangeModified,
			Fields: []FieldDiff{{
				Path: "Key", Kind: ChangeModified, BecameSensitive: true,
				Attrs: []AttrChange{{Name: "Sensitive", Base: "false", Head: "true"}},
				Refs:  []RefChange{{Kind: ChangeAdded, Ref: "aws-sm://prod/stripe#key", BasePos: -1, HeadPos: 0}},
			}},
		}},
		Privilege: PrivilegeDelta{Added: map[string][]string{"aws-sm": {"prod/stripe"}}},
	}

	var stdout, stderr strings.Builder
	if code := renderDiffJSON(&stdout, &stderr, d); code != 0 {
		t.Fatalf("want exit 0, got %d (stderr: %q)", code, stderr.String())
	}

	var back Diff
	if err := json.Unmarshal([]byte(stdout.String()), &back); err != nil {
		t.Fatalf("output does not decode as a Diff: %v\n%s", err, stdout.String())
	}
	if !reflect.DeepEqual(back, d) {
		t.Errorf("round trip mismatch\n got: %+v\nwant: %+v", back, d)
	}
}

func TestRenderDiffJSONEmptyIsStillValidJSON(t *testing.T) {
	var stdout, stderr strings.Builder
	if code := renderDiffJSON(&stdout, &stderr, Diff{}); code != 0 {
		t.Fatalf("want exit 0, got %d", code)
	}

	var back Diff
	if err := json.Unmarshal([]byte(stdout.String()), &back); err != nil {
		t.Fatalf("empty diff must still emit valid JSON: %v", err)
	}
	if !back.Empty() {
		t.Error("want the decoded empty diff to report Empty()")
	}
}

func TestRenderDiffMarkdownStructure(t *testing.T) {
	d := Diff{
		Structs: []StructDiff{{
			Package: "acme/svc", TypeName: "Config", Kind: ChangeModified,
			Fields: []FieldDiff{
				{Path: "StripeKey", Kind: ChangeAdded, BecameSensitive: false, Field: &Field{
					Path: "StripeKey", GoType: "secret.String", Refs: []string{"aws-sm://prod/stripe#key"}}},
			},
		}},
		Privilege: PrivilegeDelta{Added: map[string][]string{"aws-sm": {"prod/stripe"}}},
	}

	var sb strings.Builder
	renderDiffMarkdown(&sb, d, "")
	got := sb.String()

	if !strings.Contains(got, "### `acme/svc.Config`") {
		t.Errorf("want a markdown struct heading, got %q", got)
	}
	if !strings.Contains(got, "| `StripeKey` |") {
		t.Errorf("want a markdown table row for the field, got %q", got)
	}
	if !strings.Contains(got, "### Privilege delta") {
		t.Errorf("want a privilege heading, got %q", got)
	}
	if !strings.Contains(got, "```") {
		t.Errorf("want the privilege block fenced, got %q", got)
	}
}

func TestRenderDiffMarkdownEmpty(t *testing.T) {
	var sb strings.Builder
	renderDiffMarkdown(&sb, Diff{}, "")

	if !strings.Contains(sb.String(), "No configuration surface changes") {
		t.Errorf("want an explicit no-change line, got %q", sb.String())
	}
}

func TestRenderDiffMarkdownEscapesPipes(t *testing.T) {
	d := Diff{Structs: []StructDiff{{
		Package: "acme/svc", TypeName: "Config", Kind: ChangeModified,
		Fields: []FieldDiff{{Path: "V", Kind: ChangeModified,
			Attrs: []AttrChange{{Name: "Validate", Base: "", Head: "oneof=a|b"}}}},
	}}}

	var sb strings.Builder
	renderDiffMarkdown(&sb, d, "")
	got := sb.String()

	if strings.Contains(got, "oneof=a|b") {
		t.Errorf("a raw pipe would break the markdown table, got %q", got)
	}
	if !strings.Contains(got, `oneof=a\|b`) {
		t.Errorf("want the pipe escaped, got %q", got)
	}
}

func TestRenderDiffMarkdownIsDeterministic(t *testing.T) {
	d := Diff{Privilege: PrivilegeDelta{Added: map[string][]string{
		"aws-sm": {"a", "b"}, "vault": {"c"}, "gcp-sm": {"d/e"},
	}}}

	var first, second strings.Builder
	renderDiffMarkdown(&first, d, "aws-iam")
	renderDiffMarkdown(&second, d, "aws-iam")

	if first.String() != second.String() {
		t.Errorf("renderDiffMarkdown is not deterministic:\nfirst:\n%s\nsecond:\n%s", first.String(), second.String())
	}
}

func TestRenderDiffMarkdownFenceSurvivesBacktickContent(t *testing.T) {
	d := Diff{Privilege: PrivilegeDelta{Added: map[string][]string{
		"aws-sm": {"prod/```evil"},
	}}}

	var sb strings.Builder
	renderDiffMarkdown(&sb, d, "")
	got := sb.String()

	if !strings.Contains(got, "prod/```evil") {
		t.Errorf("want the path rendered verbatim, got %q", got)
	}
	// The fence must be longer than the longest backtick run in the content,
	// so the block cannot be closed early.
	if !strings.Contains(got, "````") {
		t.Errorf("want a fence longer than the content's backtick run, got %q", got)
	}
}

func TestRenderDiffMarkdownEscapesCarriageReturn(t *testing.T) {
	// A lone \r is a CommonMark line ending: left unescaped, it would end the
	// table row and let the rest of the cell's content render as fresh
	// markdown outside the table, e.g. injecting a fake bold "reviewed"
	// claim into a PR summary.
	d := Diff{Structs: []StructDiff{{
		Package: "acme/svc", TypeName: "Config", Kind: ChangeModified,
		Fields: []FieldDiff{{Path: "Port", Kind: ChangeModified,
			Attrs: []AttrChange{{Name: "Source", Base: "",
				Head: "env:PORT\r\r**Reviewed: no new permissions.**"}}}},
	}}}

	var sb strings.Builder
	renderDiffMarkdown(&sb, d, "")
	got := sb.String()

	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	found := false
	for _, l := range lines {
		if strings.Contains(l, "env:PORT") {
			found = true
			if !strings.Contains(l, "Reviewed: no new permissions.") {
				t.Errorf("want the injected text to stay on the same table row, got line %q", l)
			}
			if !strings.HasPrefix(l, "| ~ |") {
				t.Errorf("want the row to still start as a table row, got %q", l)
			}
		}
	}
	if !found {
		t.Fatalf("want the Head value rendered somewhere, got %q", got)
	}
	if strings.Contains(got, "\r") {
		t.Errorf("want the carriage return stripped, got %q", got)
	}
}

func TestRenderDiffMarkdownEscapesBacktick(t *testing.T) {
	d := Diff{Structs: []StructDiff{{
		Package: "acme/svc", TypeName: "Config", Kind: ChangeModified,
		Fields: []FieldDiff{{Path: "V", Kind: ChangeModified,
			Attrs: []AttrChange{{Name: "Validate", Base: "", Head: "oneof=a`b"}}}},
	}}}

	var sb strings.Builder
	renderDiffMarkdown(&sb, d, "")
	got := sb.String()

	if strings.Contains(got, "oneof=a`b") {
		t.Errorf("a raw backtick would break the inline code span, got %q", got)
	}
	if !strings.Contains(got, "oneof=a\\`b") {
		t.Errorf("want the backtick escaped, got %q", got)
	}
}

func TestPrivilegeLinesSanitizesNewlineInPath(t *testing.T) {
	// A newline in a ref path must not forge an extra privilege line: an
	// attacker could otherwise fabricate a bogus "- aws-sm  prod/x" removal
	// line, or pad with newlines to push the real "+" line out of a CI
	// step summary's visible area.
	d := PrivilegeDelta{Added: map[string][]string{
		"aws-sm": {"prod/x\n- aws-sm  prod/x"},
	}}

	lines := privilegeLines(d, "")
	if len(lines) != 1 {
		t.Fatalf("want exactly one privilege line, got %d: %q", len(lines), lines)
	}
	if strings.Contains(lines[0], "\n") {
		t.Errorf("want the newline sanitized out of the line, got %q", lines[0])
	}
}

func TestRenderDiffTextSanitizesNewlineInPrivilegePath(t *testing.T) {
	d := Diff{Privilege: PrivilegeDelta{Removed: map[string][]string{
		"aws-sm": {"prod/x\n- aws-sm  prod/x"},
	}}}

	var sb strings.Builder
	renderDiffText(&sb, d, "")
	got := sb.String()

	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	count := 0
	for _, l := range lines {
		if strings.Contains(l, "aws-sm") {
			count++
		}
	}
	if count != 1 {
		t.Errorf("want exactly one privilege line mentioning aws-sm, got %d in %q", count, got)
	}
}

func TestRenderDiffMarkdownSanitizesNewlineInPrivilegePath(t *testing.T) {
	d := Diff{Privilege: PrivilegeDelta{Removed: map[string][]string{
		"aws-sm": {"prod/x\n- aws-sm  prod/x"},
	}}}

	var sb strings.Builder
	renderDiffMarkdown(&sb, d, "")
	got := sb.String()

	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	count := 0
	for _, l := range lines {
		if strings.Contains(l, "aws-sm") {
			count++
		}
	}
	if count != 1 {
		t.Errorf("want exactly one privilege line mentioning aws-sm, got %d in %q", count, got)
	}
}

func TestSanitizeControlFoldsC0AndDEL(t *testing.T) {
	in := "a\rb\nc\td\x7fe"
	got := sanitizeControl(in)
	want := "a b c d e"
	if got != want {
		t.Errorf("sanitizeControl(%q) = %q, want %q", in, got, want)
	}
	if strings.ContainsAny(got, "\r\n\t\x7f") {
		t.Errorf("want no control characters left, got %q", got)
	}
}

func TestMdFenceLength(t *testing.T) {
	cases := []struct {
		name  string
		lines []string
		want  string
	}{
		{name: "no backticks uses three", lines: []string{"+ aws-sm  prod/x"}, want: "```"},
		{name: "one backtick still three", lines: []string{"a `b"}, want: "```"},
		{name: "triple run grows to four", lines: []string{"a ``` b"}, want: "````"},
		{name: "longest run across lines wins", lines: []string{"a ``` b", "c ````` d"}, want: "``````"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mdFence(tc.lines); got != tc.want {
				t.Errorf("mdFence = %q, want %q", got, tc.want)
			}
		})
	}
}
