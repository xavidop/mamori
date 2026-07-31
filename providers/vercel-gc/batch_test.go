package vercelgc

import (
	"context"
	"testing"

	"github.com/xavidop/mamori"
)

func TestResolveBatchIsOneRequestPerStore(t *testing.T) {
	f := newFake()
	f.set(testStore, "a", `"1"`)
	f.set(testStore, "b", `"2"`)
	f.set(testStore, "c", `"3"`)
	f.set("ecfg_other", "d", `"4"`)
	p := f.provider()

	refs := []mamori.Ref{
		ref(t, "vercel-gc://a"),
		ref(t, "vercel-gc://b"),
		ref(t, "vercel-gc://c"),
		ref(t, "vercel-gc://ecfg_other/d"),
	}

	got, err := p.ResolveBatch(context.Background(), refs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("got %d values, want 4", len(got))
	}
	for tag, want := range map[string]string{
		"vercel-gc://a":            "1",
		"vercel-gc://b":            "2",
		"vercel-gc://c":            "3",
		"vercel-gc://ecfg_other/d": "4",
	} {
		if string(got[tag].Bytes) != want {
			t.Errorf("%s: got %q, want %q", tag, got[tag].Bytes, want)
		}
	}

	if _, items := f.counts(); items != 2 {
		t.Errorf("got %d items requests, want 2 (one per store)", items)
	}
}

// Per the BatchProvider contract, a missing key is omitted so mamori applies
// the field default rather than failing the whole batch.
func TestResolveBatchOmitsNotFound(t *testing.T) {
	f := newFake()
	f.set(testStore, "present", `"yes"`)
	p := f.provider()

	got, err := p.ResolveBatch(context.Background(), []mamori.Ref{
		ref(t, "vercel-gc://present"),
		ref(t, "vercel-gc://absent"),
	})
	if err != nil {
		t.Fatalf("a missing key must not fail the batch, got %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d values, want 1", len(got))
	}
	if _, ok := got["vercel-gc://absent"]; ok {
		t.Error("absent key must be omitted from the result map")
	}
}

// The batch installs its snapshot, so a Load followed by watching costs no
// redundant fetch.
func TestResolveBatchInstallsSnapshot(t *testing.T) {
	f := newFake()
	f.set(testStore, "a", `"1"`)
	p := f.provider()
	ctx := context.Background()

	if _, err := p.ResolveBatch(ctx, []mamori.Ref{ref(t, "vercel-gc://a")}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_, itemsAfterBatch := f.counts()

	if _, err := p.Resolve(ctx, ref(t, "vercel-gc://a")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, items := f.counts(); items != itemsAfterBatch {
		t.Errorf("Resolve after ResolveBatch refetched items: %d then %d", itemsAfterBatch, items)
	}
}

func TestResolveBatchEmpty(t *testing.T) {
	f := newFake()
	p := f.provider()

	got, err := p.ResolveBatch(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d values, want 0", len(got))
	}
	if digests, items := f.counts(); digests != 0 || items != 0 {
		t.Errorf("an empty batch must make no requests, got %d digest and %d items", digests, items)
	}
}

func TestProviderImplementsBatchProvider(t *testing.T) {
	var _ mamori.BatchProvider = (*Provider)(nil)
}

// Vercel offers no streaming or blocking read, so faking a Watch with a ticker
// is forbidden by the provider contract.
func TestProviderIsNotWatchable(t *testing.T) {
	if _, ok := any(New()).(mamori.WatchableProvider); ok {
		t.Fatal("vercel-gc must not implement WatchableProvider: Vercel has no native change notification")
	}
}
