package mamori

import (
	"errors"
	"testing"
)

// rfc6901Doc is the example document from RFC 6901 section 5.
var rfc6901Doc = []byte(`{
   "foo": ["bar", "baz"],
   "": 0,
   "a/b": 1,
   "c%d": 2,
   "e^f": 3,
   "g|h": 4,
   "i\\j": 5,
   "k\"l": 6,
   " ": 7,
   "m~n": 8
}`)

func TestSelectPointerRFC6901(t *testing.T) {
	tests := []struct{ ptr, want string }{
		{"/foo", `["bar", "baz"]`},
		{"/foo/0", "bar"},
		{"/foo/1", "baz"},
		{"/", "0"},
		{"/a~1b", "1"},
		{"/c%d", "2"},
		{"/e^f", "3"},
		{"/g|h", "4"},
		{"/i\\j", "5"},
		{"/k\"l", "6"},
		{"/ ", "7"},
		{"/m~0n", "8"},
	}
	for _, tt := range tests {
		got, err := SelectKey(rfc6901Doc, tt.ptr)
		if err != nil {
			t.Fatalf("SelectKey(%q) error: %v", tt.ptr, err)
		}
		if string(got) != tt.want {
			t.Errorf("SelectKey(%q) = %q, want %q", tt.ptr, got, tt.want)
		}
	}
}

// replicaDoc is the array-of-objects shape from spec section 5.2.
var replicaDoc = []byte(`{"replicas":[` +
	`{"host":"r0.db","creds":{"user":"app","password":"p0"}},` +
	`{"host":"r1.db","creds":{"user":"app","password":"p1"}},` +
	`{"host":"r2.db","creds":{"user":"app","password":"p2"}},` +
	`{"host":"r3.db","creds":{"user":"app","password":"p3"}},` +
	`{"host":"r4.db","creds":{"user":"app","password":"p4"}},` +
	`{"host":"r5.db","creds":{"user":"app","password":"p5"}}]}`)

func TestSelectPointerArrayOfObjects(t *testing.T) {
	got, err := SelectKey(replicaDoc, "/replicas/5/creds/password")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if string(got) != "p5" {
		t.Errorf("= %q, want p5", got)
	}
	got, err = SelectKey(replicaDoc, "/replicas/0/host")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if string(got) != "r0.db" {
		t.Errorf("= %q, want r0.db", got)
	}
}

func TestSelectPointerErrors(t *testing.T) {
	tests := []struct {
		name string
		ptr  string
		want error
	}{
		{"absent key", "/replicas/0/missing", ErrNotFound},
		{"index out of range", "/replicas/99", ErrNotFound},
		{"descend into scalar", "/replicas/0/host/nope", ErrInvalid},
		{"non-numeric array token", "/replicas/five", ErrInvalid},
		{"leading zero index", "/replicas/05", ErrInvalid},
		{"dash token", "/replicas/-", ErrInvalid},
		{"bad escape", "/replicas/0/a~2b", ErrInvalid},
		{"trailing tilde", "/replicas/0/a~", ErrInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := SelectKey(replicaDoc, tt.ptr)
			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestSelectPointerPreservesBytes(t *testing.T) {
	// A pointer-selected object must come back byte-identical, not
	// key-reordered by a marshal round trip.
	doc := []byte(`{"outer":{"z":1,"a":2,"m":3}}`)
	got, err := SelectKey(doc, "/outer")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"z":1,"a":2,"m":3}` {
		t.Errorf("= %q, want byte-preserved object", got)
	}
}
