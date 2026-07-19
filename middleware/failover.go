package middleware

import (
	"context"
	"errors"
	"fmt"

	"github.com/xavidop/mamori"
)

// Failover tries the primary provider first and, on a transport/backend error
// (anything other than not-found), falls through to each replica in order. A
// not-found result is authoritative and returned immediately - replicas are not
// consulted, since a definitive "no such value" should not be masked by a stale
// replica.
//
// All providers must share the same scheme (the primary's scheme is reported).
func Failover(primary mamori.Provider, replicas ...mamori.Provider) mamori.Provider {
	all := append([]mamori.Provider{primary}, replicas...)
	resolve := func(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
		var errs []error
		for _, p := range all {
			v, err := p.Resolve(ctx, ref)
			if err == nil {
				return v, nil
			}
			if errors.Is(err, mamori.ErrNotFound) {
				return mamori.Value{}, err
			}
			errs = append(errs, err)
		}
		return mamori.Value{}, fmt.Errorf("mamori/middleware: all %d providers failed: %w", len(all), errors.Join(errs...))
	}
	// Watch delegates to the primary if it is watchable.
	return newWrapper(primary, resolve, nil)
}
