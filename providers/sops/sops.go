// Package sops is a mamori provider that resolves values from SOPS-encrypted
// files (https://github.com/getsops/sops).
//
// The scheme is:
//
//	sops://<path/to/file.enc.yaml>[#key]
//
// The referenced file is decrypted with the ambient SOPS key material (age via
// SOPS_AGE_KEY / SOPS_AGE_KEY_FILE, or a cloud KMS resolved through the usual
// default credential chains) and the plaintext is returned as the Value. When a
// #key fragment is present the decrypted document is treated as a structured
// (JSON or YAML) payload and the single field is selected with
// mamori.SelectKey, identically to every other provider.
//
//	DBPassword secret.String `source:"sops:///etc/secrets/db.enc.yaml#password"`
//	APIKey     secret.String `source:"sops://secrets/app.enc.json#api_key"`
//
// Values are always marked Sensitive. The Version is derived from the encrypted
// file's size and modification time, so re-resolving an unchanged file yields a
// stable version (cheap change detection) without decrypting twice for equality.
//
// The provider natively watches the encrypted file with fsnotify (mirroring the
// core file provider): it watches the parent directory so an atomic rename - the
// common secret-mount / editor-save pattern - is detected, re-decrypts, and
// emits the new value.
package sops

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/fsnotify/fsnotify"
	"github.com/getsops/sops/v3/decrypt"
	"github.com/xavidop/mamori"
	"gopkg.in/yaml.v3"
)

// scheme is the URL scheme handled by this provider.
const scheme = "sops"

// DecryptFunc decrypts the SOPS-encrypted file at path and returns the plaintext
// bytes. format is one of "yaml", "json", "dotenv" or "binary" and is derived
// from the file extension. The default implementation is
// github.com/getsops/sops/v3/decrypt.File; tests inject a fake to exercise the
// provider without live key material.
type DecryptFunc func(path, format string) ([]byte, error)

// Provider resolves sops:// refs by decrypting SOPS-encrypted files. It is safe
// for concurrent use.
type Provider struct {
	decrypt DecryptFunc
}

// Option configures a Provider.
type Option func(*Provider)

// WithDecrypt injects a custom decrypt function. This is the seam used by tests
// to supply known plaintext for a file path without any real SOPS key material.
func WithDecrypt(fn DecryptFunc) Option {
	return func(p *Provider) {
		if fn != nil {
			p.decrypt = fn
		}
	}
}

// New constructs a Provider. By default it decrypts with
// github.com/getsops/sops/v3/decrypt.File, which uses the ambient SOPS key
// material (SOPS_AGE_KEY / SOPS_AGE_KEY_FILE, or a cloud KMS via the default
// credential chain). Users who need explicit configuration can pass options,
// e.g. mamori.WithProvider(sops.New(sops.WithDecrypt(fn))).
func New(opts ...Option) *Provider {
	p := &Provider{decrypt: decrypt.File}
	for _, o := range opts {
		o(p)
	}
	return p
}

// init registers a provider using the ambient SOPS key material.
func init() { mamori.Register(New()) }

// Scheme returns "sops".
func (p *Provider) Scheme() string { return scheme }

// Resolve decrypts the file referenced by ref and returns its plaintext (or the
// selected key). A missing file is reported as mamori.ErrNotFound.
func (p *Provider) Resolve(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
	if err := ctx.Err(); err != nil {
		return mamori.Value{}, err
	}

	path := ref.Path
	// Stat first so a missing file is reported as ErrNotFound before we attempt
	// to decrypt, and so we can build a size+mtime version identifier.
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return mamori.Value{}, mamori.ErrNotFound
		}
		return mamori.Value{}, err
	}

	format := formatForPath(path)
	plaintext, err := p.decrypt(path, format)
	if err != nil {
		if os.IsNotExist(err) {
			return mamori.Value{}, mamori.ErrNotFound
		}
		return mamori.Value{}, fmt.Errorf("sops: decrypt %q: %w", path, err)
	}

	// Version tracks the encrypted file's size and mtime, mirroring the core
	// file provider: unchanged files yield a stable version, and any write bumps
	// it. NEVER incorporate the plaintext into diagnostics or the version key.
	version := fmt.Sprintf("%d-%d", info.Size(), info.ModTime().UnixNano())

	out := plaintext
	if ref.Key != "" {
		out, err = selectKey(plaintext, format, ref.Key)
		if err != nil {
			return mamori.Value{}, err
		}
	}

	return mamori.Value{
		Bytes:     out,
		Version:   version,
		Sensitive: true,
	}, nil
}

// Watch implements mamori.WatchableProvider using fsnotify. It watches the
// parent directory of the encrypted file (so an atomic replace via rename is
// detected) and re-decrypts the target on relevant events. The channel is
// closed when ctx is cancelled; no goroutine is leaked.
func (p *Provider) Watch(ctx context.Context, ref mamori.Ref) (<-chan mamori.Update, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	target := filepath.Clean(ref.Path)
	dir := filepath.Dir(target)
	if err := w.Add(dir); err != nil {
		_ = w.Close()
		return nil, err
	}

	ch := make(chan mamori.Update, 1)
	go func() {
		defer close(ch)
		defer func() { _ = w.Close() }()

		emit := func() {
			v, err := p.Resolve(ctx, ref)
			select {
			case ch <- mamori.Update{Value: v, Err: err}:
			case <-ctx.Done():
			}
		}
		// Emit the current value as a baseline.
		emit()

		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-w.Events:
				if !ok {
					return
				}
				if filepath.Clean(ev.Name) == target {
					emit()
				}
			case werr, ok := <-w.Errors:
				if !ok {
					return
				}
				select {
				case ch <- mamori.Update{Err: werr}:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return ch, nil
}

// formatForPath maps a file extension to the SOPS store format. Unknown
// extensions fall back to "binary", matching the sops CLI default.
func formatForPath(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".yaml", ".yml":
		return "yaml"
	case ".json":
		return "json"
	case ".env", ".dotenv":
		return "dotenv"
	default:
		return "binary"
	}
}

// selectKey extracts ref.Key from a decrypted document. JSON payloads are passed
// straight to mamori.SelectKey; YAML (and any other structured format) is first
// converted to JSON so selection behaves identically to every other provider.
func selectKey(plaintext []byte, format, key string) ([]byte, error) {
	if format == "json" {
		return mamori.SelectKey(plaintext, key)
	}
	jsonBytes, err := yamlToJSON(plaintext)
	if err != nil {
		return nil, fmt.Errorf("sops: cannot select key %q: %w", key, err)
	}
	return mamori.SelectKey(jsonBytes, key)
}

// yamlToJSON parses YAML (a superset of JSON) into a generic value and re-encodes
// it as JSON, so mamori.SelectKey can select a field.
func yamlToJSON(in []byte) ([]byte, error) {
	var v any
	if err := yaml.Unmarshal(in, &v); err != nil {
		return nil, err
	}
	return json.Marshal(normalizeYAML(v))
}

// normalizeYAML converts any map[any]any produced by the YAML decoder into
// map[string]any so the result is JSON-encodable. yaml.v3 already uses string
// keys for string-keyed maps, but this keeps encoding robust for edge cases.
func normalizeYAML(v any) any {
	switch t := v.(type) {
	case map[any]any:
		m := make(map[string]any, len(t))
		for k, val := range t {
			m[fmt.Sprintf("%v", k)] = normalizeYAML(val)
		}
		return m
	case map[string]any:
		for k, val := range t {
			t[k] = normalizeYAML(val)
		}
		return t
	case []any:
		for i, val := range t {
			t[i] = normalizeYAML(val)
		}
		return t
	default:
		return v
	}
}

// Ensure Provider satisfies the optional interfaces at compile time.
var (
	_ mamori.Provider          = (*Provider)(nil)
	_ mamori.WatchableProvider = (*Provider)(nil)
)
