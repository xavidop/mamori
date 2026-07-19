package aws

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/xavidop/mamori"
)

// schemePS is the URL scheme handled by PSProvider.
const schemePS = "aws-ps"

// ssmAPI is the minimal subset of the SSM client PSProvider uses. The real
// *ssm.Client satisfies it; tests inject an in-memory fake.
type ssmAPI interface {
	GetParameter(ctx context.Context, params *ssm.GetParameterInput, optFns ...func(*ssm.Options)) (*ssm.GetParameterOutput, error)
	GetParameters(ctx context.Context, params *ssm.GetParametersInput, optFns ...func(*ssm.Options)) (*ssm.GetParametersOutput, error)
}

// PSProvider resolves aws-ps://<parameter-name>[#json-key] refs against AWS
// Systems Manager Parameter Store. SecureString parameters are decrypted and
// marked Sensitive; String/StringList parameters are not. It is safe for
// concurrent use.
type PSProvider struct {
	opts   options
	mu     sync.Mutex
	client ssmAPI
}

// Compile-time interface checks.
var (
	_ mamori.Provider      = (*PSProvider)(nil)
	_ mamori.BatchProvider = (*PSProvider)(nil)
)

// NewParameterStore constructs a Parameter Store provider. The underlying AWS
// client is built lazily on first Resolve using the default credential chain,
// so construction never performs I/O and never fails.
func NewParameterStore(opts ...Option) *PSProvider {
	return &PSProvider{opts: newOptions(opts)}
}

// newPSWithClient returns a provider backed by a caller-supplied client. It is
// the injection seam used by tests to supply an in-memory fake.
func newPSWithClient(c ssmAPI) *PSProvider {
	return &PSProvider{client: c}
}

// Scheme returns "aws-ps".
func (p *PSProvider) Scheme() string { return schemePS }

// getClient returns the cached client, building the real one on first use.
func (p *PSProvider) getClient(ctx context.Context) (ssmAPI, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.client != nil {
		return p.client, nil
	}
	cfg, err := loadConfig(ctx, p.opts)
	if err != nil {
		return nil, fmt.Errorf("aws-ps: load config: %w", err)
	}
	p.client = ssm.NewFromConfig(cfg)
	return p.client, nil
}

// Resolve fetches the current value of a single parameter (with decryption).
// When ref.Key is set and the payload is a JSON object, the named field is
// selected. A missing parameter is reported as an error satisfying
// errors.Is(err, mamori.ErrNotFound).
func (p *PSProvider) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
	client, err := p.getClient(ctx)
	if err != nil {
		return mamori.Value{}, err
	}
	out, err := client.GetParameter(ctx, &ssm.GetParameterInput{
		Name:           awssdk.String(ref.Path),
		WithDecryption: awssdk.Bool(true),
	})
	if err != nil {
		return mamori.Value{}, mapPSError(ref, err)
	}
	if out.Parameter == nil {
		return mamori.Value{}, fmt.Errorf("aws-ps: parameter %q not found: %w", ref.Path, mamori.ErrNotFound)
	}
	return psValue(ref.Key, out.Parameter)
}

// ResolveBatch resolves many parameters in one GetParameters call (with
// decryption). The result is keyed by each input ref's Raw string; parameters
// that do not exist (reported in InvalidParameters) are omitted so mamori can
// apply defaults.
func (p *PSProvider) ResolveBatch(ctx context.Context, refs []mamori.Ref) (map[string]mamori.Value, error) {
	if len(refs) == 0 {
		return map[string]mamori.Value{}, nil
	}
	client, err := p.getClient(ctx)
	if err != nil {
		return nil, err
	}

	// Multiple refs may target the same parameter with different #json-keys, so
	// group refs by name and request each name once.
	names := make([]string, 0, len(refs))
	byName := make(map[string][]mamori.Ref, len(refs))
	for _, r := range refs {
		if _, seen := byName[r.Path]; !seen {
			names = append(names, r.Path)
		}
		byName[r.Path] = append(byName[r.Path], r)
	}

	out, err := client.GetParameters(ctx, &ssm.GetParametersInput{
		Names:          names,
		WithDecryption: awssdk.Bool(true),
	})
	if err != nil {
		return nil, fmt.Errorf("aws-ps: batch resolve: %w", err)
	}

	result := make(map[string]mamori.Value, len(out.Parameters))
	for i := range out.Parameters {
		param := out.Parameters[i]
		for _, r := range byName[awssdk.ToString(param.Name)] {
			v, verr := psValue(r.Key, &param)
			if verr != nil {
				continue // omit refs whose key selection fails
			}
			result[r.Raw] = v
		}
	}
	// Names in out.InvalidParameters (not found) are intentionally omitted.
	return result, nil
}

// psValue assembles a mamori.Value from a parameter, applying #json-key
// selection when key is non-empty. Version is the parameter's numeric revision;
// Sensitive is true only for SecureString parameters.
func psValue(key string, param *ssmtypes.Parameter) (mamori.Value, error) {
	var data []byte
	if param.Value != nil {
		data = []byte(*param.Value)
	}
	if key != "" {
		sel, err := mamori.SelectKey(data, key)
		if err != nil {
			return mamori.Value{}, err
		}
		data = sel
	}
	return mamori.Value{
		Bytes:     data,
		Version:   strconv.FormatInt(param.Version, 10),
		Sensitive: param.Type == ssmtypes.ParameterTypeSecureString,
	}, nil
}

// mapPSError maps an SSM ParameterNotFound error to mamori.ErrNotFound and
// otherwise annotates the error with the ref for diagnostics.
func mapPSError(ref mamori.Ref, err error) error {
	var nf *ssmtypes.ParameterNotFound
	if errors.As(err, &nf) {
		return fmt.Errorf("aws-ps: parameter %q not found: %w", ref.Path, mamori.ErrNotFound)
	}
	return fmt.Errorf("aws-ps: resolve %q: %w", ref.Path, err)
}
