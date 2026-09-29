package api

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/peasant-labs/peasant/internal/defaults"
)

// localRequestCode is the error code of a request that did not come from this
// local server.
type localRequestCode string

const (
	// codeRequestHostNotLocal: the Host header does not name a loopback address.
	codeRequestHostNotLocal localRequestCode = "request_host_not_local"
	// codeRequestOriginNotLocal: the request came from a page this server did
	// not serve.
	codeRequestOriginNotLocal localRequestCode = "request_origin_not_local"
)

// fetchSite is a Sec-Fetch-Site value.
type fetchSite string

// The Sec-Fetch-Site values that do not name another origin. An absent header
// means the client is not a browser that sends Fetch Metadata.
const (
	fetchSiteAbsent     fetchSite = ""
	fetchSiteSameOrigin fetchSite = "same-origin"
	fetchSiteNone       fetchSite = "none"
)

// localRequestRefusal says why a request did not come from this local server.
type localRequestRefusal struct {
	code   localRequestCode
	reason string
}

// refuseNonLocalRequest reports why r did not come from this local server, or
// false when it did.
//
// A local request names the server by a loopback host (localhost, 127.0.0.1,
// or [::1]). When it carries an Origin, that origin must be the server's own
// http origin at the same host and port, so a page from another site or
// another local port is refused.
//
// A request without an Origin is accepted. Browsers send Origin on every
// state-changing request and on every WebSocket handshake, so a request
// without one comes from a local client that is not a browser, such as
// `peasant web stop`, the TUI, or curl. The loopback bind already limits those
// clients to this machine. The exception is a request whose Sec-Fetch-Site
// header says it came from another origin. Only a browser sends that header,
// so the request is refused.
func refuseNonLocalRequest(r *http.Request) (localRequestRefusal, bool) {
	if !isLoopbackHost(r.Host) {
		return localRequestRefusal{
			code:   codeRequestHostNotLocal,
			reason: "its Host header does not name a loopback address (localhost, 127.0.0.1, or [::1])",
		}, true
	}
	origin := r.Header.Get(defaults.HeaderOrigin)
	if origin == "" {
		switch fetchSite(r.Header.Get(defaults.HeaderSecFetchSite)) {
		case fetchSiteAbsent, fetchSiteSameOrigin, fetchSiteNone:
			return localRequestRefusal{}, false
		}
		return localRequestRefusal{
			code:   codeRequestOriginNotLocal,
			reason: "a browser sent it from another origin",
		}, true
	}
	if !strings.EqualFold(origin, "http://"+r.Host) {
		return localRequestRefusal{
			code:   codeRequestOriginNotLocal,
			reason: "its Origin is not a page this server served",
		}, true
	}
	return localRequestRefusal{}, false
}

// isLoopbackHost reports whether a Host header value names a loopback address
// by one of defaults.LocalhostAddrs, with or without a port.
func isLoopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	return slices.ContainsFunc(defaults.LocalhostAddrs, func(addr string) bool {
		return strings.EqualFold(addr, host)
	})
}

// writeLocalRequestRefusal answers a refused request with 403 and a JSON error
// that names the reason, the refusing site, and what did not happen.
func writeLocalRequestRefusal(w http.ResponseWriter, r *http.Request, refusal localRequestRefusal, site, outcome string) {
	slog.Warn("http: refused a request that did not come from this local server",
		"method", r.Method,
		"path", r.URL.Path,
		"code", refusal.code,
		"host", r.Host,
		"origin", r.Header.Get(defaults.HeaderOrigin),
	)
	message := fmt.Sprintf("The %s %s request was refused because %s in %s. %s Open the dashboard at the loopback address that `peasant web start` prints, and retry from there.",
		r.Method, r.URL.Path, refusal.reason, site, outcome)
	writeAPIError(w, http.StatusForbidden, message, string(refusal.code))
}

// localWriteGuard refuses a state-changing request that did not come from this
// local server. GET, HEAD, and OPTIONS pass through: they change nothing, and a
// browser does not let another site read their responses. The WebSocket
// upgrade is a GET, so Hub.HandleUpgrade applies the same check itself.
func localWriteGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		if refusal, refused := refuseNonLocalRequest(r); refused {
			writeLocalRequestRefusal(w, r, refusal, "internal/api.localWriteGuard", "Nothing was changed.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// listenLoopback binds port on the IPv4 loopback and, when the host has one,
// on the IPv6 loopback. The server is then reachable only from this machine,
// at both addresses that "localhost" can resolve to.
//
// When the port is requested (not 0) and the IPv6 loopback already has it in
// use, the bind fails as a busy IPv4 port does. Otherwise a browser that
// resolves "localhost" to ::1 would reach the other process. Any other IPv6
// failure means the host has no usable IPv6 loopback, so the server listens on
// IPv4 only. An ephemeral port is chosen on IPv4. The same port is used on
// IPv6 when it is free there, and callers read the port from Server.Addr.
func listenLoopback(port int) ([]net.Listener, error) {
	v4Addr := net.JoinHostPort(defaults.LoopbackIPv4, strconv.Itoa(port))
	v4, err := net.Listen("tcp4", v4Addr)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", v4Addr, err)
	}
	bound := v4.Addr().(*net.TCPAddr).Port
	v6Addr := net.JoinHostPort(defaults.LoopbackIPv6, strconv.Itoa(bound))
	v6, err := net.Listen("tcp6", v6Addr)
	switch {
	case err == nil:
		return []net.Listener{v4, v6}, nil
	case port != 0 && errors.Is(err, syscall.EADDRINUSE):
		_ = v4.Close()
		return nil, fmt.Errorf("listen %s: %w", v6Addr, err)
	default:
		slog.Debug("http: listening on the IPv4 loopback only", "ipv6", v6Addr, "error", err)
		return []net.Listener{v4}, nil
	}
}
