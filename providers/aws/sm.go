package aws

import (
	"context"
	"errors"
	"fmt"
	"sync"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	smtypes "github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
	"github.com/xavidop/mamori"
)

// schemeSM is the URL scheme handled by SMProvider.
const schemeSM = "aws-sm"

// smAPI is the minimal subset of the Secrets Manager client SMProvider uses.
// The real *secretsmanager.Client satisfies it; tests inject an in-memory fake.
type smAPI interface {
	GetSecretValue(ctx context.Context, params *secretsmanager.GetSecretValueInput, optFns ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error)
	BatchGetSecretValue(ctx context.Context, params *secretsmanager.BatchGetSecretValueInput, optFns ...func(*secretsmanager.Options)) (*secretsmanager.BatchGetSecretValueOutput, error)
}

// SMProvider resolves aws-sm://<secret-id>[#json-key] refs against AWS Secrets
// Manager. The zero-effort path uses the default AWS credential chain. It is
// safe for concurrent use.
type SMProvider struct {
	opts   options
	mu     sync.Mutex
	client smAPI
}

// Compile-time interface checks.
var (
	_ mamori.Provider      = (*SMProvider)(nil)
	_ mamori.BatchProvider = (*SMProvider)(nil)
)

// NewSecretsManager constructs a Secrets Manager provider. The underlying AWS
// client is built lazily on first Resolve using the default credential chain,
// so construction never performs I/O and never fails.
func NewSecretsManager(opts ...Option) *SMProvider {
	return &SMProvider{opts: newOptions(opts)}
}

// newSMWithClient returns a provider backed by a caller-supplied client. It is
// the injection seam used by tests to supply an in-memory fake.
func newSMWithClient(c smAPI) *SMProvider {
	return &SMProvider{client: c}
}

// Scheme returns "aws-sm".
func (p *SMProvider) Scheme() string { return schemeSM }

// getClient returns the cached client, building the real one on first use.
func (p *SMProvider) getClient(ctx context.Context) (smAPI, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.client != nil {
		return p.client, nil
	}
	cfg, err := loadConfig(ctx, p.opts)
	if err != nil {
		return nil, fmt.Errorf("aws-sm: load config: %w", err)
	}
	p.client = secretsmanager.NewFromConfig(cfg)
	return p.client, nil
}

// Resolve fetches the current value of a single secret. When ref.Key is set and
// the payload is a JSON object, the named field is selected. A missing secret is
// reported as an error satisfying errors.Is(err, mamori.ErrNotFound).
func (p *SMProvider) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
	client, err := p.getClient(ctx)
	if err != nil {
		return mamori.Value{}, err
	}
	out, err := client.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{
		SecretId: awssdk.String(ref.Path),
	})
	if err != nil {
		return mamori.Value{}, mapSMError(ref, err)
	}
	return smValue(ref.Key, smBytes(out.SecretString, out.SecretBinary), out.VersionId)
}

// ResolveBatch resolves many secrets in one BatchGetSecretValue call. The result
// is keyed by each input ref's Raw string; secrets that do not exist (reported
// in the response's Errors list) are omitted so mamori can apply defaults.
func (p *SMProvider) ResolveBatch(ctx context.Context, refs []mamori.Ref) (map[string]mamori.Value, error) {
	if len(refs) == 0 {
		return map[string]mamori.Value{}, nil
	}
	client, err := p.getClient(ctx)
	if err != nil {
		return nil, err
	}

	// Multiple refs may target the same secret with different #json-keys, so
	// group refs by secret id and request each id once.
	ids := make([]string, 0, len(refs))
	byID := make(map[string][]mamori.Ref, len(refs))
	for _, r := range refs {
		if _, seen := byID[r.Path]; !seen {
			ids = append(ids, r.Path)
		}
		byID[r.Path] = append(byID[r.Path], r)
	}

	out, err := client.BatchGetSecretValue(ctx, &secretsmanager.BatchGetSecretValueInput{
		SecretIdList: ids,
	})
	if err != nil {
		return nil, fmt.Errorf("aws-sm: batch resolve: %w", err)
	}

	result := make(map[string]mamori.Value, len(out.SecretValues))
	for i := range out.SecretValues {
		e := out.SecretValues[i]
		for _, r := range byID[awssdk.ToString(e.Name)] {
			v, verr := smValue(r.Key, smBytes(e.SecretString, e.SecretBinary), e.VersionId)
			if verr != nil {
				continue // omit refs whose key selection fails
			}
			result[r.Raw] = v
		}
	}
	// Entries in out.Errors (including ResourceNotFoundException) are omitted.
	return result, nil
}

// smBytes returns the string payload if present, else the binary payload.
func smBytes(secretString *string, secretBinary []byte) []byte {
	if secretString != nil {
		return []byte(*secretString)
	}
	return secretBinary
}

// smValue assembles a mamori.Value from a secret's raw bytes and version id,
// applying #json-key selection when key is non-empty.
func smValue(key string, data []byte, versionID *string) (mamori.Value, error) {
	if key != "" {
		sel, err := mamori.SelectKey(data, key)
		if err != nil {
			return mamori.Value{}, err
		}
		data = sel
	}
	v := mamori.Value{Bytes: data, Sensitive: true}
	if id := awssdk.ToString(versionID); id != "" {
		v.Version = id
	} else {
		v.Version = mamori.VersionHash(data)
	}
	return v, nil
}

// mapSMError maps a Secrets Manager not-found error to mamori.ErrNotFound and
// otherwise annotates the error with the ref for diagnostics.
func mapSMError(ref mamori.Ref, err error) error {
	var nf *smtypes.ResourceNotFoundException
	if errors.As(err, &nf) {
		return fmt.Errorf("aws-sm: secret %q not found: %w", ref.Path, mamori.ErrNotFound)
	}
	return fmt.Errorf("aws-sm: resolve %q: %w", ref.Path, err)
}
