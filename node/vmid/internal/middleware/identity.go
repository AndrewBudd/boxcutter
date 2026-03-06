package middleware

import (
	"context"
	"net/http"

	"github.com/AndrewBudd/boxcutter/node/vmid/internal/registry"
)

type ctxKey int

const (
	vmRecordKey ctxKey = 0
	connMarkKey ctxKey = 1
)

func VMFromContext(ctx context.Context) (*registry.VMRecord, bool) {
	rec, ok := ctx.Value(vmRecordKey).(*registry.VMRecord)
	return rec, ok
}

// MarkFromContext returns the fwmark injected by ConnContext.
func MarkFromContext(ctx context.Context) (int, bool) {
	m, ok := ctx.Value(connMarkKey).(int)
	return m, ok
}

// WithMark returns a context with the fwmark value set.
func WithMark(ctx context.Context, mark int) context.Context {
	return context.WithValue(ctx, connMarkKey, mark)
}

// Identity looks up the requesting VM by fwmark (set via ConnContext)
// and attaches its record to the request context. Returns 403 if not found.
func Identity(reg *registry.Registry) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mark, ok := MarkFromContext(r.Context())
			if !ok || mark == 0 {
				http.Error(w, "unknown VM", http.StatusForbidden)
				return
			}

			rec, ok := reg.LookupMark(mark)
			if !ok {
				http.Error(w, "unknown VM", http.StatusForbidden)
				return
			}

			ctx := context.WithValue(r.Context(), vmRecordKey, rec)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
