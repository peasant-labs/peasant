package auth

import (
	"fmt"
	"net"
	"net/url"
	"path"
	"strconv"
	"strings"
)

// NormalizeVillageURL identifies a full Village installation, including its
// base path. It strips transport-irrelevant secrets/query/fragment, but never
// collapses distinct base paths into the same account scope.
func NormalizeVillageURL(raw string) (string, error) {
	invalid := func() (string, error) {
		return "", fmt.Errorf("select peasant online scope: ambiguous or unsupported base URL; no private observations were selected; supply an absolute HTTPS installation URL (HTTP is allowed only for loopback development)")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" || u.Host == "" {
		return invalid()
	}
	u.Scheme = strings.ToLower(u.Scheme)
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "" || strings.HasSuffix(u.Host, ":") || strings.ContainsAny(host, "\\% \t\r\n") {
		return invalid()
	}
	ip := net.ParseIP(host)
	loopback := host == "localhost" || (ip != nil && ip.IsLoopback())
	if u.Scheme != "https" && !(u.Scheme == "http" && loopback) {
		return invalid()
	}
	port := u.Port()
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return invalid()
		}
		port = strconv.Itoa(n)
	}
	defaultHTTPS := u.Scheme == "https" && port == "443"
	defaultHTTP := u.Scheme == "http" && port == "80"
	if defaultHTTPS || defaultHTTP {
		port = ""
	}
	if ip != nil {
		host = ip.String()
	}
	u.Host = host
	if strings.Contains(host, ":") {
		u.Host = "[" + host + "]"
	}
	if port != "" {
		u.Host = net.JoinHostPort(host, port)
	}
	basePath := strings.TrimRight(u.Path, "/")
	ambiguousPath := strings.Contains(basePath, "\\") || strings.Contains(basePath, "//")
	uncleanPath := basePath != "" && path.Clean(basePath) != basePath
	if ambiguousPath || uncleanPath {
		return invalid()
	}
	// Encoded separators can be interpreted differently by proxies and routers.
	escaped := strings.ToLower(u.EscapedPath())
	if strings.Contains(escaped, "%2f") || strings.Contains(escaped, "%5c") {
		return invalid()
	}
	u.Path, u.RawPath = basePath, ""
	u.User, u.RawQuery, u.Fragment, u.RawFragment = nil, "", "", ""
	u.ForceQuery = false
	return u.String(), nil
}
