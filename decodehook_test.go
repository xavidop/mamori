package mamori

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

type hookSettings struct {
	Tags []string `mapstructure:"tags"`
}

type hookConfig struct {
	Settings hookSettings `source:"mem://s" flatten:"json"`
}

// TestWithDecodeHook verifies a user-supplied mapstructure decode hook runs on
// the flatten path: here it comma-splits a string into a []string field.
func TestWithDecodeHook(t *testing.T) {
	m := newMem("mem")
	m.put("s", `{"tags":"a,b,c"}`)

	splitHook := func(from, to reflect.Type, data any) (any, error) {
		if from.Kind() == reflect.String && to == reflect.TypeOf([]string{}) {
			s := data.(string)
			if s == "" {
				return []string{}, nil
			}
			return strings.Split(s, ","), nil
		}
		return data, nil
	}

	cfg, err := Load[hookConfig](context.Background(), WithProvider(m), WithDecodeHook(splitHook))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a", "b", "c"}
	if len(cfg.Settings.Tags) != len(want) {
		t.Fatalf("Tags = %v, want %v", cfg.Settings.Tags, want)
	}
	for i, v := range want {
		if cfg.Settings.Tags[i] != v {
			t.Errorf("Tags[%d] = %q, want %q", i, cfg.Settings.Tags[i], v)
		}
	}
}
