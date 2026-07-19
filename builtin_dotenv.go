package mamori

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// dotenvProvider is the built-in dotenv: provider. It reads a single variable
// from a .env file (KEY=VALUE lines) and hot-reloads it with fsnotify, without
// touching the process environment.
//
//	DBPassword secret.String `source:"dotenv://.env#DB_PASSWORD"`
//	APIKey     secret.String `source:"dotenv:///etc/app/.env#API_KEY"`
//
// It complements the env: provider (which reads os.Getenv) and file:// with
// flatten:"env" (which decodes a whole .env into a struct): dotenv: pulls one
// named variable out of a specific file. Without a #key it returns the whole
// file's raw bytes.
//
// The parser understands a leading "export ", single- and double-quoted values
// (with the usual escapes inside double quotes), full-line "#" comments, and
// trailing " #" comments on unquoted values. Values are not marked Sensitive by
// default; wrap the field in secret.String for redaction.
type dotenvProvider struct{}

func (dotenvProvider) Scheme() string { return "dotenv" }

func (dotenvProvider) Resolve(_ context.Context, ref Ref) (Value, error) {
	info, err := os.Stat(ref.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return Value{}, ErrNotFound
		}
		return Value{}, err
	}
	b, err := os.ReadFile(ref.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return Value{}, ErrNotFound
		}
		return Value{}, err
	}
	ver := fmt.Sprintf("%d-%d", info.Size(), info.ModTime().UnixNano())

	// No #key: return the whole file (like file://).
	if ref.Key == "" {
		return Value{Bytes: b, Version: ver}, nil
	}

	vars := parseDotenv(b)
	v, ok := vars[ref.Key]
	if !ok {
		return Value{}, fmt.Errorf("mamori: dotenv: key %q not present in %s: %w", ref.Key, ref.Path, ErrNotFound)
	}
	return Value{Bytes: []byte(v), Version: ver}, nil
}

// Watch hot-reloads the .env file with fsnotify (shared with the file provider).
func (p dotenvProvider) Watch(ctx context.Context, ref Ref) (<-chan Update, error) {
	return watchFilePath(ctx, ref.Path, func(c context.Context) (Value, error) {
		return p.Resolve(c, ref)
	})
}

// parseDotenv parses .env-style KEY=VALUE lines into a map. It tolerates a
// leading "export ", quoted values, and comments.
func parseDotenv(b []byte) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		out[key] = parseDotenvValue(strings.TrimSpace(val))
	}
	return out
}

func parseDotenvValue(v string) string {
	if len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'' {
		// Single quotes: literal, no escapes.
		return v[1 : len(v)-1]
	}
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		// Double quotes: unescape the common sequences.
		inner := v[1 : len(v)-1]
		r := strings.NewReplacer(`\n`, "\n", `\r`, "\r", `\t`, "\t", `\"`, `"`, `\\`, `\`)
		return r.Replace(inner)
	}
	// Unquoted: strip a trailing " # comment", then trim.
	if i := strings.Index(v, " #"); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSpace(v)
}
