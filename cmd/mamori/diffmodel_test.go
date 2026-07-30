package main

import (
	"reflect"
	"testing"
)

// si builds a one-struct slice with the given fields, for brevity in tests.
func si(pkg, typeName string, fields ...Field) []StructInfo {
	return []StructInfo{{Package: pkg, TypeName: typeName, Fields: fields}}
}

func TestComputeDiffStructAddedAndRemoved(t *testing.T) {
	base := si("acme/svc", "Config", Field{Path: "Port", GoType: "int", Source: "env:PORT", Refs: []string{"env:PORT"}})
	head := si("acme/svc", "Other", Field{Path: "Port", GoType: "int", Source: "env:PORT", Refs: []string{"env:PORT"}})

	got := computeDiff(base, head)

	if len(got.Structs) != 2 {
		t.Fatalf("want 2 struct diffs, got %d: %+v", len(got.Structs), got.Structs)
	}
	// Deterministic order: sorted by package then type name, so "Config" precedes "Other".
	if got.Structs[0].TypeName != "Config" || got.Structs[0].Kind != ChangeRemoved {
		t.Errorf("want Config removed, got %+v", got.Structs[0])
	}
	if got.Structs[1].TypeName != "Other" || got.Structs[1].Kind != ChangeAdded {
		t.Errorf("want Other added, got %+v", got.Structs[1])
	}
}

func TestComputeDiffFieldAddedAndRemoved(t *testing.T) {
	base := si("acme/svc", "Config",
		Field{Path: "Port", GoType: "int", Source: "env:PORT", Refs: []string{"env:PORT"}})
	head := si("acme/svc", "Config",
		Field{Path: "Workers", GoType: "int", Source: "env:WORKERS", Refs: []string{"env:WORKERS"}})

	got := computeDiff(base, head)

	if len(got.Structs) != 1 {
		t.Fatalf("want 1 struct diff, got %d", len(got.Structs))
	}
	sd := got.Structs[0]
	if sd.Kind != ChangeModified {
		t.Errorf("want struct modified, got %q", sd.Kind)
	}
	if len(sd.Fields) != 2 {
		t.Fatalf("want 2 field diffs, got %d: %+v", len(sd.Fields), sd.Fields)
	}
	// Fields sort by path: "Port" precedes "Workers".
	if sd.Fields[0].Path != "Port" || sd.Fields[0].Kind != ChangeRemoved {
		t.Errorf("want Port removed, got %+v", sd.Fields[0])
	}
	if sd.Fields[1].Path != "Workers" || sd.Fields[1].Kind != ChangeAdded {
		t.Errorf("want Workers added, got %+v", sd.Fields[1])
	}
}

func TestComputeDiffIdenticalIsEmpty(t *testing.T) {
	in := si("acme/svc", "Config",
		Field{Path: "Port", GoType: "int", Source: "env:PORT", Refs: []string{"env:PORT"}})

	got := computeDiff(in, in)

	if len(got.Structs) != 0 {
		t.Errorf("want no struct diffs for identical input, got %+v", got.Structs)
	}
	if !got.Empty() {
		t.Error("want Empty() true for identical input")
	}
}

func TestDiffAttrsReportsEachAttributeOldToNew(t *testing.T) {
	base := Field{Path: "W", GoType: "int", Source: "env:W", Refs: []string{"env:W"},
		Default: "4", HasDefault: true, Optional: false, Sensitive: false, OnFail: "", Validate: "gte=1"}
	head := Field{Path: "W", GoType: "int64", Source: "env:W", Refs: []string{"env:W"},
		Default: "8", HasDefault: true, Optional: true, Sensitive: false, OnFail: "fail", Validate: "gte=1"}

	got := diffAttrs(base, head)

	want := []AttrChange{
		{Name: "GoType", Base: "int", Head: "int64"},
		{Name: "Default", Base: "4", Head: "8"},
		{Name: "Optional", Base: "false", Head: "true"},
		{Name: "OnFail", Base: "", Head: "fail"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("diffAttrs mismatch\n got: %+v\nwant: %+v", got, want)
	}
}

func TestDiffAttrsHasDefaultTogglesShowAsPresence(t *testing.T) {
	base := Field{Path: "W", Default: "", HasDefault: false}
	head := Field{Path: "W", Default: "4", HasDefault: true}

	got := diffAttrs(base, head)

	want := []AttrChange{{Name: "Default", Base: "(none)", Head: "4"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("diffAttrs mismatch\n got: %+v\nwant: %+v", got, want)
	}
}

func TestDiffChainAddedRemovedMoved(t *testing.T) {
	cases := []struct {
		name string
		base []string
		head []string
		want []RefChange
	}{
		{
			name: "position added at the end",
			base: []string{"env:PORT"},
			head: []string{"env:PORT", "aws-ps://svc/port"},
			want: []RefChange{{Kind: ChangeAdded, Ref: "aws-ps://svc/port", BasePos: -1, HeadPos: 1}},
		},
		{
			name: "position removed",
			base: []string{"env:PORT", "aws-ps://svc/port"},
			head: []string{"env:PORT"},
			want: []RefChange{{Kind: ChangeRemoved, Ref: "aws-ps://svc/port", BasePos: 1, HeadPos: -1}},
		},
		{
			name: "same refs reordered is a precedence change",
			base: []string{"env:PORT", "aws-ps://svc/port"},
			head: []string{"aws-ps://svc/port", "env:PORT"},
			want: []RefChange{
				{Kind: ChangeMoved, Ref: "aws-ps://svc/port", BasePos: 1, HeadPos: 0},
				{Kind: ChangeMoved, Ref: "env:PORT", BasePos: 0, HeadPos: 1},
			},
		},
		{
			name: "identical chain reports nothing",
			base: []string{"env:PORT"},
			head: []string{"env:PORT"},
			want: nil,
		},
		{
			name: "added and removed at once",
			base: []string{"env:PORT", "aws-ps://old"},
			head: []string{"env:PORT", "aws-ps://new"},
			want: []RefChange{
				{Kind: ChangeAdded, Ref: "aws-ps://new", BasePos: -1, HeadPos: 1},
				{Kind: ChangeRemoved, Ref: "aws-ps://old", BasePos: 1, HeadPos: -1},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := diffChain(tc.base, tc.head)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("diffChain mismatch\n got: %+v\nwant: %+v", got, tc.want)
			}
		})
	}
}

func TestComputeDiffPopulatesChainChanges(t *testing.T) {
	base := si("acme/svc", "Config",
		Field{Path: "Port", GoType: "string", Source: "env:PORT", Refs: []string{"env:PORT"}})
	head := si("acme/svc", "Config",
		Field{Path: "Port", GoType: "string", Source: "env:PORT,aws-ps://svc/port",
			Refs: []string{"env:PORT", "aws-ps://svc/port"}})

	got := computeDiff(base, head)

	if len(got.Structs) != 1 || len(got.Structs[0].Fields) != 1 {
		t.Fatalf("want one field diff, got %+v", got.Structs)
	}
	fd := got.Structs[0].Fields[0]
	if fd.Kind != ChangeModified {
		t.Errorf("want modified, got %q", fd.Kind)
	}
	want := []RefChange{{Kind: ChangeAdded, Ref: "aws-ps://svc/port", BasePos: -1, HeadPos: 1}}
	if !reflect.DeepEqual(fd.Refs, want) {
		t.Errorf("Refs mismatch\n got: %+v\nwant: %+v", fd.Refs, want)
	}
	if len(fd.Attrs) != 0 {
		t.Errorf("want no attr changes for a pure chain edit, got %+v", fd.Attrs)
	}
}

func TestComputeDiffFlagsBecameSensitive(t *testing.T) {
	base := si("acme/svc", "Config",
		Field{Path: "Key", GoType: "string", Source: "env:KEY", Refs: []string{"env:KEY"}, Sensitive: false})
	head := si("acme/svc", "Config",
		Field{Path: "Key", GoType: "secret.String", Source: "aws-sm://prod/stripe#key",
			Refs: []string{"aws-sm://prod/stripe#key"}, Sensitive: true})

	fd := computeDiff(base, head).Structs[0].Fields[0]

	if !fd.BecameSensitive {
		t.Error("want BecameSensitive true when Sensitive goes false to true")
	}
}

func TestComputeDiffDoesNotFlagSensitiveGoingAway(t *testing.T) {
	base := si("acme/svc", "Config",
		Field{Path: "Key", GoType: "secret.String", Source: "aws-sm://prod/k", Refs: []string{"aws-sm://prod/k"}, Sensitive: true})
	head := si("acme/svc", "Config",
		Field{Path: "Key", GoType: "string", Source: "env:KEY", Refs: []string{"env:KEY"}, Sensitive: false})

	fd := computeDiff(base, head).Structs[0].Fields[0]

	if fd.BecameSensitive {
		t.Error("want BecameSensitive false when Sensitive goes true to false")
	}
	// It is still reported as an ordinary attribute change.
	found := false
	for _, a := range fd.Attrs {
		if a.Name == "Sensitive" && a.Base == "true" && a.Head == "false" {
			found = true
		}
	}
	if !found {
		t.Errorf("want a Sensitive true->false attr change, got %+v", fd.Attrs)
	}
}

func TestComputeDiffChainOnlyChangeStillCountsAsModified(t *testing.T) {
	base := si("acme/svc", "Config",
		Field{Path: "P", Source: "env:A", Refs: []string{"env:A"}})
	head := si("acme/svc", "Config",
		Field{Path: "P", Source: "env:B", Refs: []string{"env:B"}})

	got := computeDiff(base, head)

	if len(got.Structs) != 1 {
		t.Fatalf("a chain-only change must still produce a struct diff, got %+v", got.Structs)
	}
}
