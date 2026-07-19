package middleware

import (
	"context"
	"strings"

	"github.com/xavidop/mamori"
)

// Prefix rewrites every ref's path by prepending prefix before delegating to the
// inner provider. It is useful for multi-tenant setups where each tenant's config
// lives under a namespace (e.g. "tenant-a/") but structs reference bare keys. The
// rewrite applies to both Resolve and native Watch.
func Prefix(prefix string, inner mamori.Provider) mamori.Provider {
	mapRef := func(ref mamori.Ref) mamori.Ref {
		out := ref
		out.Path = joinPath(prefix, ref.Path)
		out.Raw = out.String()
		return out
	}
	resolve := func(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
		return inner.Resolve(ctx, mapRef(ref))
	}
	return newWrapper(inner, resolve, mapRef)
}

func joinPath(prefix, path string) string {
	prefix = strings.TrimSuffix(prefix, "/")
	path = strings.TrimPrefix(path, "/")
	if prefix == "" {
		return path
	}
	return prefix + "/" + path
}
