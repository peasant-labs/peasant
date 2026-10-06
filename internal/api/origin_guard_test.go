package api

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/origin-guard.yaml
var originGuardYAML []byte

type originGuardCase struct {
	Name         string `yaml:"name"`
	Method       string `yaml:"method"`
	Path         string `yaml:"path"`
	Upgrade      bool   `yaml:"upgrade"`
	Host         string `yaml:"host"`
	Origin       string `yaml:"origin"`
	SecFetchSite string `yaml:"secFetchSite"`
	Status       int    `yaml:"status"`
	Code         string `yaml:"code"`
}

// The closed sets a fixture case may name, so a typo cannot turn a case into
// a different request that happens to get the same status.
var (
	originGuardMethods = map[string]struct{}{
		http.MethodGet: {}, http.MethodHead: {}, http.MethodOptions: {},
		http.MethodPost: {}, http.MethodPut: {}, http.MethodPatch: {}, http.MethodDelete: {},
	}
	originGuardFetchSites = map[string]struct{}{
		"": {}, "cross-site": {}, "same-site": {}, "same-origin": {}, "none": {},
	}
)

type originGuardFixture struct {
	RequiredNames []string          `yaml:"requiredNames"`
	Cases         []originGuardCase `yaml:"cases"`
}

func loadOriginGuardFixture(source []byte) (originGuardFixture, error) {
	var fixture originGuardFixture
	decoder := yaml.NewDecoder(bytes.NewReader(source))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		return fixture, fmt.Errorf("decode origin guard fixture: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fixture, fmt.Errorf("origin guard fixture must contain exactly one YAML document: %v", err)
	}
	required := make(map[string]struct{}, len(fixture.RequiredNames))
	for _, name := range fixture.RequiredNames {
		if name == "" {
			return fixture, fmt.Errorf("origin guard fixture has an empty required name")
		}
		if _, duplicate := required[name]; duplicate {
			return fixture, fmt.Errorf("origin guard fixture repeats required name %q", name)
		}
		required[name] = struct{}{}
	}
	seen := make(map[string]struct{}, len(fixture.Cases))
	for _, c := range fixture.Cases {
		if c.Name == "" || c.Method == "" || c.Path == "" || c.Host == "" || c.Status == 0 {
			return fixture, fmt.Errorf("origin guard fixture has an incomplete case %q", c.Name)
		}
		if _, ok := originGuardMethods[c.Method]; !ok {
			return fixture, fmt.Errorf("origin guard fixture case %q names unknown method %q", c.Name, c.Method)
		}
		if _, ok := originGuardFetchSites[c.SecFetchSite]; !ok {
			return fixture, fmt.Errorf("origin guard fixture case %q names unknown Sec-Fetch-Site %q", c.Name, c.SecFetchSite)
		}
		if c.Upgrade && (c.Method != http.MethodGet || c.Path != defaults.RouteWS.String()) {
			return fixture, fmt.Errorf("origin guard fixture case %q upgrades a request that is not GET %s", c.Name, defaults.RouteWS)
		}
		if (c.Status == http.StatusForbidden) != (c.Code != "") {
			return fixture, fmt.Errorf("origin guard fixture case %q must name a code exactly when it expects 403", c.Name)
		}
		if _, duplicate := seen[c.Name]; duplicate {
			return fixture, fmt.Errorf("origin guard fixture repeats case %q", c.Name)
		}
		if _, ok := required[c.Name]; !ok {
			return fixture, fmt.Errorf("origin guard fixture has unknown case %q", c.Name)
		}
		seen[c.Name] = struct{}{}
	}
	for name := range required {
		if _, ok := seen[name]; !ok {
			return fixture, fmt.Errorf("origin guard fixture is missing required case %q", name)
		}
	}
	return fixture, nil
}

// startOriginGuardServer listens and serves the production server with a
// WebSocket hub and no store, and stops it when the test ends.
func startOriginGuardServer(t *testing.T) *Server {
	t.Helper()
	server, _ := startHelperGroupServerHandle(t, ServerConfig{Hub: NewHub(&mockDataProvider{})})
	return server
}

func TestOriginGuardFixture(t *testing.T) {
	fixture, err := loadOriginGuardFixture(originGuardYAML)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			server := startOriginGuardServer(t)
			addr := server.Addr().String()
			_, port, err := net.SplitHostPort(addr)
			if err != nil {
				t.Fatalf("split server address %q: %v", addr, err)
			}
			expand := func(value string) string { return strings.ReplaceAll(value, "{port}", port) }

			var body io.Reader
			if !c.Upgrade {
				body = strings.NewReader("{}")
			}
			request, err := http.NewRequest(c.Method, "http://"+addr+c.Path, body)
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			request.Host = expand(c.Host)
			if c.Origin != "" {
				request.Header.Set("Origin", expand(c.Origin))
			}
			if c.SecFetchSite != "" {
				request.Header.Set("Sec-Fetch-Site", c.SecFetchSite)
			}
			if c.Upgrade {
				request.Header.Set("Connection", "Upgrade")
				request.Header.Set("Upgrade", "websocket")
				request.Header.Set("Sec-WebSocket-Version", "13")
				request.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
			}

			client := &http.Client{Timeout: 5 * time.Second}
			response, err := client.Do(request)
			if err != nil {
				t.Fatalf("%s %s: %v", c.Method, c.Path, err)
			}
			defer response.Body.Close()
			if response.StatusCode != c.Status {
				// A 101 body is the open WebSocket stream, which never ends.
				var body []byte
				if response.StatusCode != http.StatusSwitchingProtocols {
					body, _ = io.ReadAll(response.Body)
				}
				t.Fatalf("%s %s status = %d, want %d; body %s", c.Method, c.Path, response.StatusCode, c.Status, body)
			}
			if c.Code == "" {
				return
			}
			var envelope errorEnvelope
			if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
				t.Fatalf("decode refusal body: %v", err)
			}
			if envelope.Code != c.Code {
				t.Errorf("refusal code = %q, want %q", envelope.Code, c.Code)
			}
			if !strings.Contains(envelope.Error, "peasant web start") {
				t.Errorf("refusal message %q does not tell the user where to retry", envelope.Error)
			}
		})
	}
}

// TestOriginGuardFixtureRejectsMutations proves the loader refuses a fixture
// whose cases no longer say what their names claim.
func TestOriginGuardFixtureRejectsMutations(t *testing.T) {
	const row = `  - {name: delete from a foreign origin is refused, method: DELETE, path: /api/v1/annotations, host: "localhost:{port}", origin: "https://site.example", status: 403, code: request_origin_not_local}
`
	if !bytes.Contains(originGuardYAML, []byte(row)) {
		t.Fatal("the fixture no longer has the row the mutations edit")
	}
	mutate := func(from, to string) []byte {
		return bytes.Replace(originGuardYAML, []byte(from), []byte(to), 1)
	}
	mutations := map[string][]byte{
		"unknown method":         mutate("method: DELETE", "method: DELTE"),
		"unknown Sec-Fetch-Site": mutate("secFetchSite: cross-site", "secFetchSite: cross-sit"),
		"upgrade on a POST":      mutate("method: DELETE,", "method: POST, upgrade: true,"),
		"refusal without a code": mutate(", code: request_origin_not_local}\n", "}\n"),
		"deleted required case":  mutate(row, ""),
		"unknown field":          mutate("requiredNames:", "unknown: true\nrequiredNames:"),
		"trailing document":      append(append([]byte{}, originGuardYAML...), []byte("\n---\nextra: true\n")...),
	}
	for name, source := range mutations {
		if bytes.Equal(source, originGuardYAML) {
			t.Fatalf("mutation %q did not change the fixture", name)
		}
		if _, err := loadOriginGuardFixture(source); err == nil {
			t.Errorf("mutation %q unexpectedly validated", name)
		}
	}
}

// TestServerListenBindsLoopbackOnly proves the server is reachable at the
// loopback addresses and at no other address of this host.
func TestServerListenBindsLoopbackOnly(t *testing.T) {
	t.Parallel()
	server := startOriginGuardServer(t)
	_, port, err := net.SplitHostPort(server.Addr().String())
	if err != nil {
		t.Fatalf("split server address: %v", err)
	}

	assertHealthAt(t, net.JoinHostPort("127.0.0.1", port))
	if hostHasIPv6Loopback(t) {
		assertHealthAt(t, net.JoinHostPort("::1", port))
	}

	t.Run("non-loopback addresses refuse", func(t *testing.T) {
		interfaceIPs := nonLoopbackInterfaceIPs(t)
		if len(interfaceIPs) == 0 {
			t.Skip("this host has no non-loopback interface address")
		}
		for _, ip := range interfaceIPs {
			target := net.JoinHostPort(ip.String(), port)
			conn, err := net.DialTimeout("tcp", target, 2*time.Second)
			if err == nil {
				_ = conn.Close()
				t.Errorf("server accepted a connection at non-loopback address %s", target)
			}
		}
	})
}

// TestServerListenRefusesPortBusyOnIPv6Loopback proves the port check refuses
// a requested port that another process serves on the IPv6 loopback, before
// any bind. Otherwise a browser that resolves localhost to ::1 would reach
// that process.
func TestServerListenRefusesPortBusyOnIPv6Loopback(t *testing.T) {
	t.Parallel()
	if !hostHasIPv6Loopback(t) {
		t.Skip("this host has no IPv6 loopback")
	}
	// Another process may hold the same port number on the IPv4 loopback, and
	// then the bind fails there first. Choose a new port when that happens.
	for attempt := 1; attempt <= 5; attempt++ {
		holder, err := net.Listen("tcp6", "[::1]:0")
		if err != nil {
			t.Fatalf("hold an IPv6 loopback port: %v", err)
		}
		port := holder.Addr().(*net.TCPAddr).Port
		err = NewServer(ServerConfig{Port: port}).Listen(context.Background())
		_ = holder.Close()
		if err == nil {
			t.Fatalf("Listen on port %d succeeded while the IPv6 loopback held it", port)
		}
		if strings.Contains(err.Error(), "listen "+net.JoinHostPort(defaults.LoopbackIPv4, strconv.Itoa(port))) {
			continue
		}
		want := "listen " + net.JoinHostPort(defaults.LoopbackIPv6, strconv.Itoa(port))
		if !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "another process already accepts connections") {
			t.Fatalf("Listen error = %v, want the port check to name %q", err, want)
		}
		return
	}
	t.Fatal("every chosen port was also held on the IPv4 loopback")
}

// TestServerListenRefusesPortServedOnWildcardAddress proves a requested port
// that another process serves on the wildcard address fails the bind. On
// macOS a loopback bind succeeds beside such a listener, so the new server
// would start while the other one still listens on every interface.
func TestServerListenRefusesPortServedOnWildcardAddress(t *testing.T) {
	t.Parallel()
	holder, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("hold a wildcard port: %v", err)
	}
	defer holder.Close()
	port := holder.Addr().(*net.TCPAddr).Port

	server := NewServer(ServerConfig{Port: port})
	err = server.Listen(context.Background())
	if err == nil {
		for _, ln := range server.lns {
			_ = ln.Close()
		}
		t.Fatalf("Listen on port %d succeeded while a wildcard listener served it", port)
	}
	want := fmt.Sprintf("port %d", port)
	if !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "peasant web stop") {
		t.Fatalf("Listen error = %v, want it to name %q and `peasant web stop`", err, want)
	}
}

// TestServerListenRefusesPortServedOnIPv6WildcardAddress proves a requested
// port that another process serves on the IPv6-only wildcard address fails
// the start. The IPv4 loopback does not answer for that listener, so only the
// IPv6 half of the port check sees it.
func TestServerListenRefusesPortServedOnIPv6WildcardAddress(t *testing.T) {
	t.Parallel()
	if !hostHasIPv6Loopback(t) {
		t.Skip("this host has no IPv6 loopback")
	}
	holder, err := net.Listen("tcp6", "[::]:0")
	if err != nil {
		t.Fatalf("hold an IPv6 wildcard port: %v", err)
	}
	defer holder.Close()
	port := holder.Addr().(*net.TCPAddr).Port

	server := NewServer(ServerConfig{Port: port})
	err = server.Listen(context.Background())
	if err == nil {
		for _, ln := range server.lns {
			_ = ln.Close()
		}
		t.Fatalf("Listen on port %d succeeded while an IPv6 wildcard listener served it", port)
	}
	want := fmt.Sprintf("port %d", port)
	if !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "peasant web stop") {
		t.Fatalf("Listen error = %v, want it to name %q and `peasant web stop`", err, want)
	}
}

// TestServerListenWaitsForPortRelease proves a server that is still shutting
// down, as right after `peasant web stop`, does not fail the next start when
// it releases the port within the wait.
func TestServerListenWaitsForPortRelease(t *testing.T) {
	t.Parallel()
	holder, err := net.Listen("tcp4", net.JoinHostPort(defaults.LoopbackIPv4, "0"))
	if err != nil {
		t.Fatalf("hold a loopback port: %v", err)
	}
	port := holder.Addr().(*net.TCPAddr).Port
	release := time.AfterFunc(defaults.ServerPortReleaseWait/4, func() { _ = holder.Close() })
	defer release.Stop()

	server := NewServer(ServerConfig{Port: port})
	if err := server.Listen(context.Background()); err != nil {
		t.Fatalf("Listen on port %d after its holder released it: %v", port, err)
	}
	for _, ln := range server.lns {
		_ = ln.Close()
	}
}

func assertHealthAt(t *testing.T, hostport string) {
	t.Helper()
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Get("http://" + hostport + "/api/v1/health")
	if err != nil {
		t.Fatalf("GET health at %s: %v", hostport, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET health at %s status = %d, want 200", hostport, response.StatusCode)
	}
}

func hostHasIPv6Loopback(t *testing.T) bool {
	t.Helper()
	probe, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		return false
	}
	_ = probe.Close()
	return true
}

// nonLoopbackInterfaceIPs lists the unicast addresses of this host's up,
// non-tunnel interfaces that a local dial can reach without a zone: IPv4 and
// global IPv6, never loopback or link-local. Point-to-point tunnels are left
// out because a dial to their address can wait for a timeout instead of being
// refused.
func nonLoopbackInterfaceIPs(t *testing.T) []net.IP {
	t.Helper()
	interfaces, err := net.Interfaces()
	if err != nil {
		t.Fatalf("list network interfaces: %v", err)
	}
	var ips []net.IP
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&(net.FlagLoopback|net.FlagPointToPoint) != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			t.Fatalf("list addresses of interface %s: %v", iface.Name, err)
		}
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipNet.IP
			if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsMulticast() || ip.IsUnspecified() {
				continue
			}
			ips = append(ips, ip)
		}
	}
	return ips
}
