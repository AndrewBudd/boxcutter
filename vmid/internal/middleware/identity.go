package middleware

import (
	"context"
	"net"
	"net/http"

	"github.com/AndrewBudd/boxcutter/vmid/internal/registry"
)

type ctxKey int

const vmRecordKey ctxKey = 0

func VMFromContext(ctx context.Context) (*registry.VMRecord, bool) {
	rec, ok := ctx.Value(vmRecordKey).(*registry.VMRecord)
	return rec, ok
}

// Identity looks up the requesting VM by source IP and attaches
// its record to the request context. Returns 403 if the IP isn't registered.
func Identity(reg *registry.Registry) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			host, _, err := net.SplitHostPort(r.RemoteAddr)
			if err != nil {
				http.Error(w, "bad remote address", http.StatusBadRequest)
				return
			}

			rec, ok := reg.LookupIP(host)
			if !ok {
				http.Error(w, "unknown VM", http.StatusForbidden)
				return
			}

			ctx := context.WithValue(r.Context(), vmRecordKey, rec)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
