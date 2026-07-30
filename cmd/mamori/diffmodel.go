// This file holds the pure comparison logic behind `mamori diff`: it turns
// two []StructInfo (each decoded from a `mamori explain --json` output) into
// a Diff describing what changed. It performs no IO of any kind, which is
// what lets every case be table-tested from literals rather than fixtures.
//
// Everything here sorts its output. `mamori diff` is meant to be pasted into
// a pull request comment, where unstable ordering would produce phantom churn
// between runs and train reviewers to ignore the tool.
package main

import (
	"sort"
	"strconv"
)

// ChangeKind is the verdict for one struct, field, or chain position.
type ChangeKind string

const (
	ChangeAdded    ChangeKind = "added"
	ChangeRemoved  ChangeKind = "removed"
	ChangeModified ChangeKind = "modified"
	// ChangeMoved is used only for a chain position whose ref is present on
	// both sides but at a different index. Chain order is precedence, so a
	// reorder is a real change even though the ref set is identical.
	ChangeMoved ChangeKind = "moved"
)

// attrAbsent is what an unset Default renders as, so that "no default" and
// "a default of the empty string" do not both print as nothing.
const attrAbsent = "(none)"

// AttrChange is one Field attribute that differs, reported old to new.
type AttrChange struct {
	Name string `json:"name"`
	Base string `json:"base"`
	Head string `json:"head"`
}

// RefChange is one precedence-chain position that differs.
type RefChange struct {
	Kind ChangeKind `json:"kind"`
	Ref  string     `json:"ref"`
	// BasePos and HeadPos are the ref's index in the base and head chains,
	// or -1 where it is absent.
	BasePos int `json:"base_pos"`
	HeadPos int `json:"head_pos"`
}

// FieldDiff is one field's verdict.
type FieldDiff struct {
	Path string     `json:"path"`
	Kind ChangeKind `json:"kind"`
	// Attrs and Refs are populated only when Kind is ChangeModified.
	Attrs []AttrChange `json:"attrs,omitempty"`
	Refs  []RefChange  `json:"refs,omitempty"`
	// BecameSensitive is true when Sensitive went false to true: the service
	// began reading secret material where it previously read plain config.
	BecameSensitive bool `json:"became_sensitive,omitempty"`
	// Field is a snapshot used to render added and removed fields. It is the
	// head field for ChangeAdded and the base field for ChangeRemoved, and
	// nil for ChangeModified (where Attrs and Refs carry the detail).
	Field *Field `json:"field,omitempty"`
}

// StructDiff groups the field diffs for one struct type.
type StructDiff struct {
	Package  string      `json:"package"`
	TypeName string      `json:"type_name"`
	Kind     ChangeKind  `json:"kind"`
	Fields   []FieldDiff `json:"fields,omitempty"`
}

// PrivilegeDelta is the set of backend paths gained and lost, bucketed by
// scheme. Populated by Task 3.
type PrivilegeDelta struct {
	Added   map[string][]string `json:"added,omitempty"`
	Removed map[string][]string `json:"removed,omitempty"`
}

// Diff is the whole comparison of two explain outputs.
type Diff struct {
	Structs   []StructDiff   `json:"structs,omitempty"`
	Privilege PrivilegeDelta `json:"privilege"`
}

// Empty reports whether nothing at all changed.
func (d Diff) Empty() bool {
	return len(d.Structs) == 0 && len(d.Privilege.Added) == 0 && len(d.Privilege.Removed) == 0
}

// structKey identifies a struct across the two sides.
type structKey struct {
	pkg      string
	typeName string
}

// computeDiff compares two explain outputs. Structs match on
// (Package, TypeName) and fields on (Package, TypeName, Path). The result is
// sorted: structs by package then type name, fields by path.
func computeDiff(base, head []StructInfo) Diff {
	baseByKey := indexStructs(base)
	headByKey := indexStructs(head)

	keys := make([]structKey, 0, len(baseByKey)+len(headByKey))
	seen := map[structKey]bool{}
	for k := range baseByKey {
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	for k := range headByKey {
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].pkg != keys[j].pkg {
			return keys[i].pkg < keys[j].pkg
		}
		return keys[i].typeName < keys[j].typeName
	})

	var out []StructDiff
	for _, k := range keys {
		b, inBase := baseByKey[k]
		h, inHead := headByKey[k]
		switch {
		case inBase && !inHead:
			out = append(out, StructDiff{Package: k.pkg, TypeName: k.typeName, Kind: ChangeRemoved})
		case !inBase && inHead:
			out = append(out, StructDiff{Package: k.pkg, TypeName: k.typeName, Kind: ChangeAdded})
		default:
			fields := diffFields(b.Fields, h.Fields)
			if len(fields) == 0 {
				continue
			}
			out = append(out, StructDiff{
				Package: k.pkg, TypeName: k.typeName, Kind: ChangeModified, Fields: fields,
			})
		}
	}
	return Diff{Structs: out}
}

// indexStructs keys a slice of StructInfo by (Package, TypeName). A duplicate
// key keeps the first occurrence, matching Extract's documented deterministic
// order (packages sorted, structs in source declaration order).
func indexStructs(in []StructInfo) map[structKey]StructInfo {
	out := make(map[structKey]StructInfo, len(in))
	for _, s := range in {
		k := structKey{pkg: s.Package, typeName: s.TypeName}
		if _, dup := out[k]; dup {
			continue
		}
		out[k] = s
	}
	return out
}

// diffFields compares the fields of two matched structs, sorted by path.
func diffFields(base, head []Field) []FieldDiff {
	baseByPath := indexFields(base)
	headByPath := indexFields(head)

	paths := make([]string, 0, len(baseByPath)+len(headByPath))
	seen := map[string]bool{}
	for p := range baseByPath {
		if !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}
	for p := range headByPath {
		if !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)

	var out []FieldDiff
	for _, p := range paths {
		b, inBase := baseByPath[p]
		h, inHead := headByPath[p]
		switch {
		case inBase && !inHead:
			snapshot := b
			out = append(out, FieldDiff{Path: p, Kind: ChangeRemoved, Field: &snapshot})
		case !inBase && inHead:
			snapshot := h
			out = append(out, FieldDiff{Path: p, Kind: ChangeAdded, Field: &snapshot})
		default:
			fd := FieldDiff{
				Path:            p,
				Kind:            ChangeModified,
				Attrs:           diffAttrs(b, h),
				Refs:            diffChain(b.Refs, h.Refs),
				BecameSensitive: !b.Sensitive && h.Sensitive,
			}
			if len(fd.Attrs) == 0 && len(fd.Refs) == 0 {
				continue
			}
			out = append(out, fd)
		}
	}
	return out
}

func indexFields(in []Field) map[string]Field {
	out := make(map[string]Field, len(in))
	for _, f := range in {
		if _, dup := out[f.Path]; dup {
			continue
		}
		out[f.Path] = f
	}
	return out
}

// diffAttrs compares two matched fields attribute by attribute, in a fixed
// declared order so output never churns. Source is deliberately NOT compared
// here: it is the raw tag whose meaningful content is the chain, which Task 2
// compares at ref granularity instead. Comparing both would report every
// chain edit twice.
func diffAttrs(base, head Field) []AttrChange {
	var out []AttrChange
	add := func(name, b, h string) {
		if b != h {
			out = append(out, AttrChange{Name: name, Base: b, Head: h})
		}
	}

	add("GoType", base.GoType, head.GoType)
	add("Default", defaultAttr(base), defaultAttr(head))
	add("Optional", strconv.FormatBool(base.Optional), strconv.FormatBool(head.Optional))
	add("Sensitive", strconv.FormatBool(base.Sensitive), strconv.FormatBool(head.Sensitive))
	add("OnFail", base.OnFail, head.OnFail)
	add("Validate", base.Validate, head.Validate)
	return out
}

// defaultAttr renders a field's default, distinguishing "no default: tag" from
// "a default: tag whose value is the empty string".
func defaultAttr(f Field) string {
	if !f.HasDefault {
		return attrAbsent
	}
	return f.Default
}

// diffChain compares two precedence chains at ref granularity. A chain is an
// ordered list, and its order IS precedence, so this reports three things: a
// ref only in head (added), a ref only in base (removed), and a ref in both
// but at a different index (moved). Reporting a chain edit as one opaque
// Source string change would hide that a service acquired a new backend
// dependency, which is the whole point of the command.
//
// Output is sorted by ref so a chain edit renders identically on every run.
// A duplicated ref within one chain is compared at its first index; refs are
// not meaningfully repeatable in a chain (a second occurrence can never win),
// so no attempt is made to pair up duplicates positionally.
func diffChain(base, head []string) []RefChange {
	basePos := firstIndexOf(base)
	headPos := firstIndexOf(head)

	refs := make([]string, 0, len(basePos)+len(headPos))
	seen := map[string]bool{}
	for r := range basePos {
		if !seen[r] {
			seen[r] = true
			refs = append(refs, r)
		}
	}
	for r := range headPos {
		if !seen[r] {
			seen[r] = true
			refs = append(refs, r)
		}
	}
	sort.Strings(refs)

	var out []RefChange
	for _, r := range refs {
		b, inBase := basePos[r]
		h, inHead := headPos[r]
		switch {
		case inBase && !inHead:
			out = append(out, RefChange{Kind: ChangeRemoved, Ref: r, BasePos: b, HeadPos: -1})
		case !inBase && inHead:
			out = append(out, RefChange{Kind: ChangeAdded, Ref: r, BasePos: -1, HeadPos: h})
		case b != h:
			out = append(out, RefChange{Kind: ChangeMoved, Ref: r, BasePos: b, HeadPos: h})
		}
	}
	return out
}

// firstIndexOf maps each ref to its first index in the chain.
func firstIndexOf(chain []string) map[string]int {
	out := make(map[string]int, len(chain))
	for i, r := range chain {
		if _, dup := out[r]; dup {
			continue
		}
		out[r] = i
	}
	return out
}
