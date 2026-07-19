// Package flipt implements a mamori provider for Flipt
// (https://www.flipt.io), the open-source, self-hosted feature-flag and
// configuration platform.
//
// The provider evaluates a flag with the official Flipt Go evaluation SDK
// (go.flipt.io/flipt/sdk/go) over the HTTP transport and returns the evaluated
// result as the resolved value.
//
// # Scheme
//
//	flipt://<namespace>/<flag-key>[#attachment][?entity=<id>]
//
// The path carries the Flipt namespace and flag key, both required:
//
//	Beta bool   `source:"flipt://production/new-checkout"`
//	Tier string `source:"flipt://production/plan-tier?entity=user-42"`
//
// Boolean flags resolve to the string "true" or "false". Variant flags resolve
// to the matched variant key. When the fragment is "#attachment", a variant
// flag resolves to its variant attachment (a JSON string) instead of the
// variant key; the attachment fragment has no effect on boolean flags.
//
// # Entity
//
// Flipt evaluates against an entity id, used for percentage rollouts and
// segment targeting. It is taken from the ?entity= option and defaults to
// "mamori" when unset.
//
// # Authentication
//
// The Flipt server address comes from WithURL or, when unset, the FLIPT_URL
// environment variable (default http://localhost:8080). An optional client
// token is supplied via WithToken or the FLIPT_TOKEN environment variable and
// is sent as a static bearer token. Both are read lazily on first resolve, so
// the provider is safe to register from init even when no configuration is
// present at process start.
//
// Flag values are not secrets, so Value.Sensitive is false and Value.Version is
// a content hash (mamori.VersionHash), which still gives mamori cheap, correct
// change detection.
//
// Flipt has no native change-notification API for evaluation, so this provider
// is not watchable; mamori wraps it in its polling adapter automatically.
package flipt

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/xavidop/mamori"

	flipterrors "go.flipt.io/flipt/errors"
	"go.flipt.io/flipt/rpc/flipt/evaluation"
	sdk "go.flipt.io/flipt/sdk/go"
	flipthttp "go.flipt.io/flipt/sdk/go/http"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// scheme is the URL scheme this provider handles.
const scheme = "flipt"

// defaultURL is the address the provider targets when neither WithURL nor
// FLIPT_URL is set. It matches Flipt's default HTTP listen address.
const defaultURL = "http://localhost:8080"

// defaultEntity is the entity id used for evaluation when ?entity= is unset.
const defaultEntity = "mamori"

// attachmentFragment is the special #key value selecting a variant's
// attachment rather than its variant key.
const attachmentFragment = "attachment"

// evaluator is the minimal surface of the Flipt evaluation client used by this
// provider. The real *sdk.Evaluation returned by sdk.SDK.Evaluation satisfies
// it, and tests inject an in-memory fake implementing the same two methods.
type evaluator interface {
	Boolean(ctx context.Context, v *evaluation.EvaluationRequest) (*evaluation.BooleanEvaluationResponse, error)
	Variant(ctx context.Context, v *evaluation.EvaluationRequest) (*evaluation.VariantEvaluationResponse, error)
}

// Provider resolves flipt:// refs by evaluating flags against a Flipt server. It
// is safe for concurrent use.
type Provider struct {
	url   string
	token string

	// mu guards lazy construction and caching of the evaluation client.
	mu     sync.Mutex
	client evaluator
}

// Option configures a Provider.
type Option func(*Provider)

// WithURL sets the Flipt server address explicitly (e.g. https://flipt.example.com).
// When unset, the provider reads FLIPT_URL from the environment at resolve time,
// falling back to http://localhost:8080.
func WithURL(url string) Option {
	return func(p *Provider) { p.url = strings.TrimRight(url, "/") }
}

// WithToken sets the Flipt client token sent as a static bearer credential. When
// unset, the provider reads FLIPT_TOKEN from the environment at resolve time.
func WithToken(token string) Option {
	return func(p *Provider) { p.token = token }
}

// New constructs a Flipt provider. Without options it targets FLIPT_URL (or
// http://localhost:8080) and reads FLIPT_TOKEN lazily on first resolve, so it is
// safe to register from init even when no configuration is present at process
// start.
//
// Users who need explicit configuration call
// mamori.WithProvider(flipt.New(flipt.WithURL("https://flipt.example.com"))).
func New(opts ...Option) *Provider {
	p := &Provider{}
	for _, o := range opts {
		o(p)
	}
	return p
}

// newWithClient builds a provider backed by a pre-supplied evaluation client. It
// is the injection seam used by tests (and internally after lazy construction).
func newWithClient(e evaluator) *Provider {
	return &Provider{client: e}
}

func init() { mamori.Register(New()) }

// Scheme returns "flipt".
func (p *Provider) Scheme() string { return scheme }

// evaluatorFor returns the cached evaluation client, building the real Flipt SDK
// client lazily on first use from the configured (or ambient) URL and token.
func (p *Provider) evaluatorFor() evaluator {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.client != nil {
		return p.client
	}

	url := p.url
	if url == "" {
		url = strings.TrimRight(os.Getenv("FLIPT_URL"), "/")
	}
	if url == "" {
		url = defaultURL
	}
	token := p.token
	if token == "" {
		token = os.Getenv("FLIPT_TOKEN")
	}

	transport := flipthttp.NewTransport(url, flipthttp.WithHTTPClient(&http.Client{Timeout: 30 * time.Second}))
	var opts []sdk.Option
	if token != "" {
		opts = append(opts, sdk.WithAuthenticationProvider(sdk.StaticTokenAuthenticationProvider(token)))
	}
	p.client = sdk.New(transport, opts...).Evaluation()
	return p.client
}

// Resolve evaluates the flag named by ref.Path (namespace/flag-key) for the
// requested entity and returns the evaluated result. A flag that does not exist
// is reported as ErrNotFound.
func (p *Provider) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
	namespace, flag, err := parsePath(ref.Path)
	if err != nil {
		return mamori.Value{}, err
	}
	if ref.Key != "" && ref.Key != attachmentFragment {
		return mamori.Value{}, fmt.Errorf("mamori/flipt: ref %q has unsupported fragment %q; only #attachment is recognized", ref.Raw, ref.Key)
	}

	entity := ref.Opt("entity")
	if entity == "" {
		entity = defaultEntity
	}

	req := &evaluation.EvaluationRequest{
		NamespaceKey: namespace,
		FlagKey:      flag,
		EntityId:     entity,
	}
	ev := p.evaluatorFor()

	// Try a variant evaluation first. A boolean flag reports an invalid-type
	// error, in which case we fall back to a boolean evaluation.
	vResp, vErr := ev.Variant(ctx, req)
	switch {
	case vErr == nil:
		payload := vResp.GetVariantKey()
		flagType := "variant"
		if ref.Key == attachmentFragment {
			payload = vResp.GetVariantAttachment()
			flagType = "variant-attachment"
		}
		return p.value(namespace, flag, entity, flagType, payload), nil
	case isNotFound(vErr):
		return mamori.Value{}, notFound(namespace, flag)
	case isInvalidType(vErr):
		// Fall through to a boolean evaluation below.
	default:
		return mamori.Value{}, fmt.Errorf("mamori/flipt: evaluating %s/%s: %w", namespace, flag, vErr)
	}

	bResp, bErr := ev.Boolean(ctx, req)
	switch {
	case bErr == nil:
		return p.value(namespace, flag, entity, "boolean", strconv.FormatBool(bResp.GetEnabled())), nil
	case isNotFound(bErr):
		return mamori.Value{}, notFound(namespace, flag)
	default:
		return mamori.Value{}, fmt.Errorf("mamori/flipt: evaluating %s/%s: %w", namespace, flag, bErr)
	}
}

// value builds a Value from an evaluated payload. Flag values are not secrets,
// so Sensitive is false and Version is a content hash. Metadata carries only
// non-payload annotations (namespace, flag, entity, flag type).
func (p *Provider) value(namespace, flag, entity, flagType, payload string) mamori.Value {
	b := []byte(payload)
	return mamori.Value{
		Bytes:     b,
		Version:   mamori.VersionHash(b),
		Sensitive: false,
		Metadata: map[string]string{
			"namespace": namespace,
			"flag":      flag,
			"entity":    entity,
			"type":      flagType,
		},
	}
}

// notFound builds an ErrNotFound-wrapping error for a missing flag.
func notFound(namespace, flag string) error {
	return fmt.Errorf("mamori/flipt: flag %q not found in namespace %q: %w", flag, namespace, mamori.ErrNotFound)
}

// isNotFound reports whether err signals that the flag does not exist. Both SDK
// transports surface backend errors as gRPC status errors, so a NotFound code is
// authoritative; the Flipt typed error is also checked for robustness.
func isNotFound(err error) bool {
	if status.Code(err) == codes.NotFound {
		return true
	}
	return flipterrors.AsMatch[flipterrors.ErrNotFound](err)
}

// isInvalidType reports whether err signals that the flag exists but was
// evaluated with the wrong evaluation kind (e.g. a variant call on a boolean
// flag). Flipt reports this as InvalidArgument / ErrInvalid.
func isInvalidType(err error) bool {
	if status.Code(err) == codes.InvalidArgument {
		return true
	}
	return flipterrors.AsMatch[flipterrors.ErrInvalid](err)
}

// parsePath splits "<namespace>/<flag-key>" into its two required, non-empty
// segments.
func parsePath(path string) (namespace, flag string, err error) {
	trimmed := strings.Trim(path, "/")
	namespace, flag, ok := strings.Cut(trimmed, "/")
	if !ok || namespace == "" || flag == "" {
		return "", "", fmt.Errorf("mamori/flipt: path %q must be <namespace>/<flag-key>", path)
	}
	return namespace, flag, nil
}
