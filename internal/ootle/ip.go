package ootle

import (
	"net"
	"net/http"
	"strings"
)

// clientIP extracts the real client IP from r, the same
// implementation as internal/faucet.ClientIP (lowercased/unexported
// here since nothing outside this package needs it), duplicated to
// keep this package self-contained. See faucet.ClientIP's doc comment
// for the full X-Forwarded-For handling rationale.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		first := strings.TrimSpace(parts[0])
		if first != "" {
			return first
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
