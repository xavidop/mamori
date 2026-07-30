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
