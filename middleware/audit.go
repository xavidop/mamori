package middleware

import (
	"context"
	"log/slog"
	"time"

	"github.com/xavidop/mamori"
)

// Audit logs every Resolve - scheme, ref, latency, and outcome - WITHOUT the
// resolved value, so an audit trail never leaks a value fetched from a backend.
// The ref is logged through Ref.Redacted, the same redaction Report and Status
// apply, so a ref carrying an inline credential in a query option does not
// appear in the log in the clear.
func Audit(logger *slog.Logger, inner mamori.Provider) mamori.Provider {
	if logger == nil {
		logger = slog.Default()
	}
	resolve := func(ctx context.Context, ref mamori.Ref) (mamori.Value, error) {
		start := time.Now()
		v, err := inner.Resolve(ctx, ref)
		attrs := []slog.Attr{
			slog.String("scheme", ref.Scheme),
			slog.String("ref", ref.Redacted()),
			slog.Duration("latency", time.Since(start)),
		}
		if err != nil {
			logger.LogAttrs(ctx, slog.LevelWarn, "mamori resolve failed",
				append(attrs, slog.String("error", err.Error()))...)
		} else {
			logger.LogAttrs(ctx, slog.LevelInfo, "mamori resolve",
				append(attrs, slog.String("version", v.Version), slog.Bool("sensitive", v.Sensitive))...)
		}
		return v, err
	}
	return newWrapper(inner, resolve, nil)
}
