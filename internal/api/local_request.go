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
	"time"

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
// false when it did. It applies refuseForeignHost and then refuseForeignOrigin.
func refuseNonLocalRequest(r *http.Request) (localRequestRefusal, bool) {
	if refusal, refused := refuseForeignHost(r); refused {
		return refusal, true
	}
	return refuseForeignOrigin(r)
}

// refuseForeignHost refuses a request that does not name the server by a
// loopback host (localhost, 127.0.0.1, or [::1]). A page served under another
// hostname that resolves to a loopback address has that hostname as its
// origin, so its requests are refused here whatever their method.
func refuseForeignHost(r *http.Request) (localRequestRefusal, bool) {
	if isLoopbackHost(r.Host) {
		return localRequestRefusal{}, false
	}
	return localRequestRefusal{
		code:   codeRequestHostNotLocal,
		reason: "its Host header does not name a loopback address (localhost, 127.0.0.1, or [::1])",
	}, true
}

// refuseForeignOrigin refuses a request from a page this server did not serve.
// When the request carries an Origin, that origin must be the server's own
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
func refuseForeignOrigin(r *http.Request) (localRequestRefusal, bool) {
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

// localRequestGuard refuses a request that did not come from this local
// server. Every request must name the server by a loopback host. A request
// that is not GET, HEAD, or OPTIONS must also pass refuseForeignOrigin. The
// safe methods change nothing, and a browser does not let a page from another
// origin read their responses. The WebSocket upgrade is a GET, so
// Hub.HandleUpgrade applies the full check itself.
func localRequestGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			if refusal, refused := refuseForeignHost(r); refused {
				writeLocalRequestRefusal(w, r, refusal, "internal/api.localRequestGuard", "Nothing was served.")
				return
			}
		default:
			if refusal, refused := refuseNonLocalRequest(r); refused {
				writeLocalRequestRefusal(w, r, refusal, "internal/api.localRequestGuard", "Nothing was changed.")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// ephemeralBindAttempts bounds how often an ephemeral port is chosen again
// when the IPv6 loopback already holds the port chosen on IPv4.
const ephemeralBindAttempts = 8

// listenLoopback binds port on the IPv4 loopback and, when the host has one,
// on the IPv6 loopback. The server is then reachable only from this machine,
// at both addresses that "localhost" can resolve to.
//
// A requested port (not 0) must be free on both loopbacks. The bind fails when
// another process already accepts connections there (see CheckLoopbackPortFree)
// or when the IPv6 loopback holds the port, as it fails for a busy IPv4 port.
// Otherwise a client that resolves "localhost" to the other address would
// reach the other process. An ephemeral port is chosen on IPv4 and used on
// IPv6 too, and is chosen again when IPv6 already holds it. Any other IPv6
// failure means the host has no usable IPv6 loopback, so the server listens on
// IPv4 only. Callers read the port from Server.Addr.
func listenLoopback(port int) ([]net.Listener, error) {
	if port != 0 {
		if err := CheckLoopbackPortFree(port); err != nil {
			return nil, err
		}
	}
	for attempt := 1; ; attempt++ {
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
		case !errors.Is(err, syscall.EADDRINUSE):
			slog.Debug("http: listening on the IPv4 loopback only", "ipv6", v6Addr, "error", err)
			return []net.Listener{v4}, nil
		}
		_ = v4.Close()
		if port != 0 || attempt == ephemeralBindAttempts {
			return nil, fmt.Errorf("listen %s: %w", v6Addr, err)
		}
	}
}

// CheckLoopbackPortFree returns an error when another process already accepts
// connections on port at the IPv4 or IPv6 loopback address. On macOS, a
// listener on the wildcard address does not make a loopback bind fail, so
// without this check a new server could start beside an earlier one that
// listens on every interface. The check waits up to
// defaults.ServerPortReleaseWait, so a server that is still shutting down, as
// right after `peasant web stop`, can release the port first.
func CheckLoopbackPortFree(port int) error {
	deadline := time.Now().Add(defaults.ServerPortReleaseWait)
	for {
		addr, served := servedLoopbackAddr(port)
		if !served {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("listen %s: another process already accepts connections on port %d, so no server was started; if it is an earlier `peasant web start`, run `peasant web stop --port %d`, then retry", addr, port, port)
		}
		time.Sleep(defaults.ServerPortProbeInterval)
	}
}

// servedLoopbackAddr returns the first loopback address at port that accepts
// a connection.
func servedLoopbackAddr(port int) (string, bool) {
	for _, host := range []string{defaults.LoopbackIPv4, defaults.LoopbackIPv6} {
		addr := net.JoinHostPort(host, strconv.Itoa(port))
		conn, err := net.DialTimeout("tcp", addr, defaults.ServerPortProbeTimeout)
		if err == nil {
			_ = conn.Close()
			return addr, true
		}
	}
	return "", false
}
