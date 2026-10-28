package server

import (
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/captain-corgi/go-bot-example/internal/problem"
)

const invalidHostDetail = "Host header must be 127.0.0.1 or localhost on the bound port."

// HostValidationMiddleware guards against DNS rebinding by allowing only
// requests whose Host header is 127.0.0.1 or localhost, optionally followed by
// a port. When boundPort > 0, an explicit port must equal boundPort. All other
// Host values are rejected with an RFC 7807 400 Bad Request response.
func HostValidationMiddleware(boundPort int, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isAllowedHost(r.Host, boundPort) {
			problem.BadRequest(w, r, invalidHostDetail)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isAllowedHost(hostHeader string, boundPort int) bool {
	if hostHeader == "" {
		return false
	}
	host := hostHeader
	if strings.Contains(hostHeader, ":") {
		h, port, err := net.SplitHostPort(hostHeader)
		if err != nil || !isAllowedPort(port, boundPort) {
			return false
		}
		host = h
	}
	return host == LoopbackHost || strings.EqualFold(host, "localhost")
}

func isAllowedPort(port string, boundPort int) bool {
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return false
	}
	return boundPort <= 0 || n == boundPort
}
