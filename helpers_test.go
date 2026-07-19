package mamori

import (
	"errors"
	"testing"
)

func TestSelectKey(t *testing.T) {
	payload := []byte(`{"password":"s3cr3t","port":5432,"tls":true,"nested":{"a":1}}`)

	got, err := SelectKey(payload, "password")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "s3cr3t" {
		t.Errorf("password = %q, want s3cr3t", got)
	}

	got, err = SelectKey(payload, "port")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "5432" {
		t.Errorf("port = %q, want 5432", got)
	}

	got, err = SelectKey(payload, "nested")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"a":1}` {
		t.Errorf("nested = %q, want {\"a\":1}", got)
	}

	if _, err := SelectKey(payload, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing key err = %v, want ErrNotFound", err)
	}

	same, err := SelectKey(payload, "")
	if err != nil || string(same) != string(payload) {
		t.Errorf("empty key should return payload unchanged")
	}
}
