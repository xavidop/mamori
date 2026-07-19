package dynamodb

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	awsdynamodb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/xavidop/mamori"
	"github.com/xavidop/mamori/providertest"
)

// ---------------------------------------------------------------------------
// In-memory fake for DynamoDB GetItem.
// ---------------------------------------------------------------------------

// fakeDDB is a tiny in-memory stand-in for the DynamoDB client. Items are stored
// under a composite storage key derived from their primary-key attributes so the
// same GetItem key lookup works for simple and composite primary keys.
type fakeDDB struct {
	mu       sync.Mutex
	items    map[string]map[string]ddbtypes.AttributeValue
	errOnGet error // when set, GetItem returns it (to exercise error mapping)
}

func newFakeDDB() *fakeDDB {
	return &fakeDDB{items: map[string]map[string]ddbtypes.AttributeValue{}}
}

// put stores item under the given table, keyed by the listed key attribute names.
func (f *fakeDDB) put(table string, item map[string]ddbtypes.AttributeValue, keyNames ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	keyAttrs := make(map[string]ddbtypes.AttributeValue, len(keyNames))
	for _, n := range keyNames {
		if av, ok := item[n]; ok {
			keyAttrs[n] = av
		}
	}
	f.items[storageKey(table, keyAttrs)] = item
}

func (f *fakeDDB) GetItem(ctx context.Context, in *awsdynamodb.GetItemInput, _ ...func(*awsdynamodb.Options)) (*awsdynamodb.GetItemOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.errOnGet != nil {
		return nil, f.errOnGet
	}
	item, ok := f.items[storageKey(awssdk.ToString(in.TableName), in.Key)]
	if !ok {
		return &awsdynamodb.GetItemOutput{}, nil // no Item -> not found
	}
	return &awsdynamodb.GetItemOutput{Item: item}, nil
}

// storageKey builds a deterministic key from a table name and its primary-key
// attributes, so put and GetItem agree on where an item lives.
func storageKey(table string, keyAttrs map[string]ddbtypes.AttributeValue) string {
	names := make([]string, 0, len(keyAttrs))
	for n := range keyAttrs {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString(table)
	for _, n := range names {
		b.WriteByte('\x00')
		b.WriteString(n)
		b.WriteByte('=')
		b.WriteString(scalarString(keyAttrs[n]))
	}
	return b.String()
}

func scalarString(av ddbtypes.AttributeValue) string {
	switch v := av.(type) {
	case *ddbtypes.AttributeValueMemberS:
		return v.Value
	case *ddbtypes.AttributeValueMemberN:
		return v.Value
	default:
		return ""
	}
}

func s(v string) ddbtypes.AttributeValue { return &ddbtypes.AttributeValueMemberS{Value: v} }
func n(v string) ddbtypes.AttributeValue { return &ddbtypes.AttributeValueMemberN{Value: v} }

func mustParse(t *testing.T, raw string) mamori.Ref {
	t.Helper()
	ref, err := mamori.ParseRef(raw)
	if err != nil {
		t.Fatalf("ParseRef(%q): %v", raw, err)
	}
	return ref
}

// ---------------------------------------------------------------------------
// Conformance kit against the in-memory fake.
// ---------------------------------------------------------------------------

func TestConformance(t *testing.T) {
	fake := newFakeDDB()
	seed := func(_ context.Context, key, val string) error {
		fake.put("conf", map[string]ddbtypes.AttributeValue{
			"pk":    s(key),
			"value": s(val),
		}, "pk")
		return nil
	}
	providertest.Run(t, providertest.Config{
		New:    func() mamori.Provider { return newWithClient(fake) },
		Ref:    func(key string) string { return "dynamodb://conf/" + key + "#value" },
		Seed:   seed,
		Mutate: seed, // put overwrites, so seed doubles as mutate
	})
}

// ---------------------------------------------------------------------------
// Registration & scheme.
// ---------------------------------------------------------------------------

func TestRegisteredScheme(t *testing.T) {
	found := false
	for _, sc := range mamori.RegisteredSchemes() {
		if sc == scheme {
			found = true
		}
	}
	if !found {
		t.Errorf("scheme %q was not registered by init()", scheme)
	}
}

func TestScheme(t *testing.T) {
	if got := New().Scheme(); got != scheme {
		t.Errorf("Scheme() = %q, want %q", got, scheme)
	}
	if got := New(WithRegion("eu-west-1")).Scheme(); got != scheme {
		t.Errorf("Scheme() with region = %q, want %q", got, scheme)
	}
}

// ---------------------------------------------------------------------------
// Resolve unit tests.
// ---------------------------------------------------------------------------

func TestResolveWholeItemJSON(t *testing.T) {
	fake := newFakeDDB()
	fake.put("users", map[string]ddbtypes.AttributeValue{
		"pk":   s("u-1"),
		"name": s("neo"),
		"age":  n("30"),
	}, "pk")
	p := newWithClient(fake)

	v, err := p.Resolve(context.Background(), mustParse(t, "dynamodb://users/u-1"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(v.Bytes, &got); err != nil {
		t.Fatalf("whole-item payload is not JSON: %v (%s)", err, v.Bytes)
	}
	if got["pk"] != "u-1" || got["name"] != "neo" {
		t.Errorf("unexpected item JSON: %s", v.Bytes)
	}
	if got["age"].(float64) != 30 {
		t.Errorf("age = %v, want 30", got["age"])
	}
	if v.Version == "" {
		t.Error("Version must be set")
	}
	if v.Sensitive {
		t.Error("value must not be Sensitive by default")
	}
}

func TestResolveAttrString(t *testing.T) {
	fake := newFakeDDB()
	fake.put("users", map[string]ddbtypes.AttributeValue{"pk": s("u-1"), "email": s("neo@zion")}, "pk")
	p := newWithClient(fake)

	v, err := p.Resolve(context.Background(), mustParse(t, "dynamodb://users/u-1#email"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "neo@zion" {
		t.Errorf("Bytes = %q, want neo@zion", v.Bytes)
	}
}

func TestResolveAttrNumberPreservesForm(t *testing.T) {
	fake := newFakeDDB()
	fake.put("t", map[string]ddbtypes.AttributeValue{"pk": s("k"), "port": n("5432")}, "pk")
	p := newWithClient(fake)

	v, err := p.Resolve(context.Background(), mustParse(t, "dynamodb://t/k#port"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "5432" {
		t.Errorf("Bytes = %q, want 5432", v.Bytes)
	}
}

func TestResolveAttrBool(t *testing.T) {
	fake := newFakeDDB()
	fake.put("t", map[string]ddbtypes.AttributeValue{
		"pk":      s("k"),
		"enabled": &ddbtypes.AttributeValueMemberBOOL{Value: true},
	}, "pk")
	p := newWithClient(fake)

	v, err := p.Resolve(context.Background(), mustParse(t, "dynamodb://t/k#enabled"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "true" {
		t.Errorf("Bytes = %q, want true", v.Bytes)
	}
}

func TestResolveAttrMapJSON(t *testing.T) {
	fake := newFakeDDB()
	fake.put("t", map[string]ddbtypes.AttributeValue{
		"pk": s("k"),
		"db": &ddbtypes.AttributeValueMemberM{Value: map[string]ddbtypes.AttributeValue{
			"host": s("db.internal"),
			"port": n("5432"),
		}},
	}, "pk")
	p := newWithClient(fake)

	v, err := p.Resolve(context.Background(), mustParse(t, "dynamodb://t/k#db"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(v.Bytes, &got); err != nil {
		t.Fatalf("map attribute is not JSON: %v (%s)", err, v.Bytes)
	}
	if got["host"] != "db.internal" || got["port"].(float64) != 5432 {
		t.Errorf("unexpected map JSON: %s", v.Bytes)
	}
}

func TestResolveMissingItem(t *testing.T) {
	p := newWithClient(newFakeDDB())
	_, err := p.Resolve(context.Background(), mustParse(t, "dynamodb://users/nope"))
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("error %v does not satisfy errors.Is(err, mamori.ErrNotFound)", err)
	}
}

func TestResolveMissingAttr(t *testing.T) {
	fake := newFakeDDB()
	fake.put("users", map[string]ddbtypes.AttributeValue{"pk": s("u-1"), "name": s("neo")}, "pk")
	p := newWithClient(fake)

	_, err := p.Resolve(context.Background(), mustParse(t, "dynamodb://users/u-1#missing"))
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("absent attribute must be ErrNotFound, got %v", err)
	}
}

func TestResolveCustomPKName(t *testing.T) {
	fake := newFakeDDB()
	fake.put("t", map[string]ddbtypes.AttributeValue{"id": s("x"), "value": s("hello")}, "id")
	p := newWithClient(fake)

	v, err := p.Resolve(context.Background(), mustParse(t, "dynamodb://t/x#value?pk_name=id"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "hello" {
		t.Errorf("Bytes = %q, want hello", v.Bytes)
	}
}

func TestResolveSortKey(t *testing.T) {
	fake := newFakeDDB()
	// Composite primary key: pk + year.
	fake.put("events", map[string]ddbtypes.AttributeValue{
		"pk":      s("e-1"),
		"year":    s("2024"),
		"payload": s("data-2024"),
	}, "pk", "year")
	fake.put("events", map[string]ddbtypes.AttributeValue{
		"pk":      s("e-1"),
		"year":    s("2023"),
		"payload": s("data-2023"),
	}, "pk", "year")
	p := newWithClient(fake)

	v, err := p.Resolve(context.Background(), mustParse(t, "dynamodb://events/e-1#payload?sk=2024&sk_name=year"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "data-2024" {
		t.Errorf("Bytes = %q, want data-2024", v.Bytes)
	}
}

func TestResolveDefaultSortKeyName(t *testing.T) {
	fake := newFakeDDB()
	fake.put("events", map[string]ddbtypes.AttributeValue{
		"pk":      s("e-1"),
		"sk":      s("v2"),
		"payload": s("second"),
	}, "pk", "sk")
	p := newWithClient(fake)

	v, err := p.Resolve(context.Background(), mustParse(t, "dynamodb://events/e-1#payload?sk=v2"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(v.Bytes) != "second" {
		t.Errorf("Bytes = %q, want second", v.Bytes)
	}
}

func TestVersionAttributeWins(t *testing.T) {
	fake := newFakeDDB()
	fake.put("t", map[string]ddbtypes.AttributeValue{
		"pk":      s("k"),
		"value":   s("hello"),
		"version": n("7"),
	}, "pk")
	p := newWithClient(fake)

	v, err := p.Resolve(context.Background(), mustParse(t, "dynamodb://t/k#value"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if v.Version != "7" {
		t.Errorf("Version = %q, want 7 (from the version attribute)", v.Version)
	}
}

func TestVersionFallsBackToHash(t *testing.T) {
	fake := newFakeDDB()
	fake.put("t", map[string]ddbtypes.AttributeValue{"pk": s("k"), "value": s("hello")}, "pk")
	p := newWithClient(fake)

	v, err := p.Resolve(context.Background(), mustParse(t, "dynamodb://t/k#value"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if v.Version != mamori.VersionHash([]byte("hello")) {
		t.Errorf("Version = %q, want content hash", v.Version)
	}
}

func TestWithSensitive(t *testing.T) {
	fake := newFakeDDB()
	fake.put("t", map[string]ddbtypes.AttributeValue{"pk": s("k"), "value": s("s3cr3t")}, "pk")
	p := newWithClient(fake, WithSensitive())

	v, err := p.Resolve(context.Background(), mustParse(t, "dynamodb://t/k#value"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !v.Sensitive {
		t.Error("WithSensitive must mark the value Sensitive")
	}
}

func TestResolveBadPath(t *testing.T) {
	p := newWithClient(newFakeDDB())
	_, err := p.Resolve(context.Background(), mustParse(t, "dynamodb://onlytable"))
	if err == nil {
		t.Fatal("expected an error for a path without a partition key")
	}
	if errors.Is(err, mamori.ErrNotFound) {
		t.Errorf("bad-path error should not be ErrNotFound: %v", err)
	}
}

func TestResourceNotFoundMapsToErrNotFound(t *testing.T) {
	fake := newFakeDDB()
	fake.errOnGet = &ddbtypes.ResourceNotFoundException{Message: awssdk.String("Requested resource not found")}
	p := newWithClient(fake)

	_, err := p.Resolve(context.Background(), mustParse(t, "dynamodb://missing-table/k"))
	if !errors.Is(err, mamori.ErrNotFound) {
		t.Fatalf("ResourceNotFoundException must map to ErrNotFound, got %v", err)
	}
}
