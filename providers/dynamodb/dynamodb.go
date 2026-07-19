// Package dynamodb provides a mamori value provider backed by Amazon DynamoDB.
//
// It registers a single scheme:
//
//	dynamodb://<table>/<pk>[#attr][?pk_name=<name>&sk=<value>&sk_name=<name>]
//
// A ref names a table and the string value of the item's partition key. The
// partition key attribute is called "pk" by default; override it with the
// ?pk_name option. If the table uses a composite primary key, supply the sort
// key value with ?sk (its attribute name defaults to "sk", overridable with
// ?sk_name).
//
// Resolution performs a single GetItem. When the item does not exist the
// provider returns an error satisfying errors.Is(err, mamori.ErrNotFound). The
// returned payload depends on the optional #attr fragment:
//
//   - dynamodb://users/u-42            -> the whole item, encoded as plain JSON.
//   - dynamodb://users/u-42#email      -> just the "email" attribute. Scalars
//     (S, N, BOOL, NULL, B) are stringified; maps, lists, and sets are JSON.
//
// Value.Version is taken from a top-level "version" attribute when the item
// carries one (so an application can bump it to force a refresh), otherwise it
// is a content hash of the returned bytes (mamori.VersionHash). Values are not
// marked Sensitive by default; construct the provider with WithSensitive when a
// table holds secret material.
//
// The client is built lazily on first Resolve using the default AWS credential
// chain (environment, shared config/profile, EC2/ECS/EKS role, SSO, ...). The
// region comes from the same ambient configuration unless pinned with WithRegion.
//
// DynamoDB has no cheap native change notification (DynamoDB Streams require
// additional infrastructure), so this provider does not implement
// WatchableProvider - mamori polls it. See the README for a note on Streams as a
// future push mode.
//
// Zero-config use is automatic via the registered provider. Callers who need
// explicit configuration use:
//
//	cfg, _ := mamori.Load[Config](ctx,
//	    mamori.WithProvider(dynamodb.New(dynamodb.WithRegion("us-east-1"))),
//	)
package dynamodb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	awsdynamodb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/xavidop/mamori"
)

// scheme is the URL scheme handled by Provider.
const scheme = "dynamodb"

const (
	// defaultPKName is the partition key attribute name assumed when the ref does
	// not set ?pk_name.
	defaultPKName = "pk"
	// defaultSKName is the sort key attribute name assumed when the ref sets ?sk
	// but not ?sk_name.
	defaultSKName = "sk"
	// versionAttr is the item attribute consulted for a native revision.
	versionAttr = "version"
)

// ddbAPI is the minimal subset of the DynamoDB client Provider uses. The real
// *dynamodb.Client satisfies it; tests inject an in-memory fake.
type ddbAPI interface {
	GetItem(ctx context.Context, params *awsdynamodb.GetItemInput, optFns ...func(*awsdynamodb.Options)) (*awsdynamodb.GetItemOutput, error)
}

// options holds construction-time configuration for a Provider.
type options struct {
	region    string
	sensitive bool
}

// Option customizes a Provider constructed with New.
type Option func(*options)

// WithRegion pins the AWS region for the provider's client. When unset, the
// region is resolved from the ambient AWS configuration (AWS_REGION,
// AWS_DEFAULT_REGION, the shared config file, or instance metadata).
func WithRegion(region string) Option {
	return func(o *options) { o.region = region }
}

// WithSensitive marks every value this provider returns as Sensitive, driving
// redaction downstream. DynamoDB items are not secret by default, so this is off
// unless requested.
func WithSensitive() Option {
	return func(o *options) { o.sensitive = true }
}

func newOptions(opts []Option) options {
	var o options
	for _, fn := range opts {
		fn(&o)
	}
	return o
}

// Provider resolves dynamodb://<table>/<pk>[#attr] refs against Amazon DynamoDB.
// The zero-effort path uses the default AWS credential chain. It is safe for
// concurrent use.
type Provider struct {
	opts   options
	mu     sync.Mutex
	client ddbAPI
}

// Compile-time interface check.
var _ mamori.Provider = (*Provider)(nil)

// New constructs a DynamoDB provider. The underlying AWS client is built lazily
// on first Resolve using the default credential chain, so construction never
// performs I/O and never fails.
func New(opts ...Option) *Provider {
	return &Provider{opts: newOptions(opts)}
}

// newWithClient returns a provider backed by a caller-supplied client. It is the
// injection seam used by tests to supply an in-memory fake.
func newWithClient(c ddbAPI, opts ...Option) *Provider {
	return &Provider{opts: newOptions(opts), client: c}
}

// Scheme returns "dynamodb".
func (p *Provider) Scheme() string { return scheme }

// getClient returns the cached client, building the real one on first use.
func (p *Provider) getClient(ctx context.Context) (ddbAPI, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.client != nil {
		return p.client, nil
	}
	cfg, err := loadConfig(ctx, p.opts)
	if err != nil {
		return nil, fmt.Errorf("dynamodb: load config: %w", err)
	}
	p.client = awsdynamodb.NewFromConfig(cfg)
	return p.client, nil
}

// loadConfig builds an aws.Config from the default credential chain, applying
// any provider options. It honors ctx for the credential and region resolution.
func loadConfig(ctx context.Context, o options) (awssdk.Config, error) {
	var loadOpts []func(*awsconfig.LoadOptions) error
	if o.region != "" {
		loadOpts = append(loadOpts, awsconfig.WithRegion(o.region))
	}
	return awsconfig.LoadDefaultConfig(ctx, loadOpts...)
}

// Resolve fetches a single item by primary key. When ref.Key (#attr) is set the
// named attribute is returned, otherwise the whole item as JSON. A missing item
// is reported as an error satisfying errors.Is(err, mamori.ErrNotFound).
func (p *Provider) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
	table, pk, err := splitPath(ref.Path)
	if err != nil {
		return mamori.Value{}, err
	}
	key := buildKey(ref, pk)

	client, err := p.getClient(ctx)
	if err != nil {
		return mamori.Value{}, err
	}

	out, err := client.GetItem(ctx, &awsdynamodb.GetItemInput{
		TableName: awssdk.String(table),
		Key:       key,
	})
	if err != nil {
		return mamori.Value{}, mapError(ref, err)
	}
	if len(out.Item) == 0 {
		return mamori.Value{}, fmt.Errorf("dynamodb: item %q not found in table %q: %w", pk, table, mamori.ErrNotFound)
	}

	data, err := itemBytes(out.Item, ref.Key)
	if err != nil {
		return mamori.Value{}, err
	}

	return mamori.Value{
		Bytes:     data,
		Version:   itemVersion(out.Item, data),
		Sensitive: p.opts.sensitive,
	}, nil
}

// splitPath splits a ref path of the form "<table>/<pk>" into its parts.
func splitPath(path string) (table, pk string, err error) {
	table, pk, ok := strings.Cut(path, "/")
	if !ok || table == "" || pk == "" {
		return "", "", fmt.Errorf("dynamodb: ref path %q must be <table>/<pk>", path)
	}
	return table, pk, nil
}

// buildKey assembles the GetItem primary key from the ref: a string partition
// key plus, when ?sk is present, a string sort key. Attribute names default to
// "pk"/"sk" and are overridable with ?pk_name/?sk_name.
func buildKey(ref mamori.Ref, pk string) map[string]ddbtypes.AttributeValue {
	pkName := ref.Opt("pk_name")
	if pkName == "" {
		pkName = defaultPKName
	}
	key := map[string]ddbtypes.AttributeValue{
		pkName: &ddbtypes.AttributeValueMemberS{Value: pk},
	}
	if sk := ref.Opt("sk"); sk != "" {
		skName := ref.Opt("sk_name")
		if skName == "" {
			skName = defaultSKName
		}
		key[skName] = &ddbtypes.AttributeValueMemberS{Value: sk}
	}
	return key
}

// itemBytes renders either a single attribute (attr != "") or the whole item
// (attr == "") into bytes. A requested attribute that is absent is reported as
// not-found so mamori can apply defaults.
func itemBytes(item map[string]ddbtypes.AttributeValue, attr string) ([]byte, error) {
	if attr == "" {
		var m map[string]any
		if err := attributevalue.UnmarshalMap(item, &m); err != nil {
			return nil, fmt.Errorf("dynamodb: decode item: %w", err)
		}
		b, err := json.Marshal(m)
		if err != nil {
			return nil, fmt.Errorf("dynamodb: encode item: %w", err)
		}
		return b, nil
	}
	av, ok := item[attr]
	if !ok {
		return nil, fmt.Errorf("dynamodb: attribute %q not present in item: %w", attr, mamori.ErrNotFound)
	}
	return attributeBytes(av)
}

// attributeBytes stringifies a scalar attribute and JSON-encodes a structured
// one (map, list, or set). Numbers keep their canonical DynamoDB string form,
// preserving precision.
func attributeBytes(av ddbtypes.AttributeValue) ([]byte, error) {
	switch v := av.(type) {
	case *ddbtypes.AttributeValueMemberS:
		return []byte(v.Value), nil
	case *ddbtypes.AttributeValueMemberN:
		return []byte(v.Value), nil
	case *ddbtypes.AttributeValueMemberBOOL:
		return []byte(strconv.FormatBool(v.Value)), nil
	case *ddbtypes.AttributeValueMemberB:
		return v.Value, nil
	case *ddbtypes.AttributeValueMemberNULL:
		return []byte("null"), nil
	default:
		var out any
		if err := attributevalue.Unmarshal(av, &out); err != nil {
			return nil, fmt.Errorf("dynamodb: decode attribute: %w", err)
		}
		b, err := json.Marshal(out)
		if err != nil {
			return nil, fmt.Errorf("dynamodb: encode attribute: %w", err)
		}
		return b, nil
	}
}

// itemVersion prefers a native "version" attribute (stringified) and falls back
// to a content hash of the returned bytes.
func itemVersion(item map[string]ddbtypes.AttributeValue, data []byte) string {
	if av, ok := item[versionAttr]; ok {
		if b, err := attributeBytes(av); err == nil && len(b) > 0 {
			return string(b)
		}
	}
	return mamori.VersionHash(data)
}

// mapError maps a DynamoDB table/index-not-found error to mamori.ErrNotFound and
// otherwise annotates the error with the ref for diagnostics.
func mapError(ref mamori.Ref, err error) error {
	var nf *ddbtypes.ResourceNotFoundException
	if errors.As(err, &nf) {
		return fmt.Errorf("dynamodb: %q not found: %w", ref.Path, mamori.ErrNotFound)
	}
	return fmt.Errorf("dynamodb: resolve %q: %w", ref.Path, err)
}

// init registers a lazily-initialized provider so that dynamodb:// refs resolve
// out of the box under ambient AWS credentials.
func init() {
	mamori.Register(New())
}
