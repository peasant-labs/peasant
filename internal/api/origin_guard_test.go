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
	server := NewServer(ServerConfig{Port: 0, Hub: NewHub(&mockDataProvider{})})
	ctx, cancel := context.WithCancel(context.Background())
	if err := server.Listen(ctx); err != nil {
		cancel()
		t.Fatalf("listen origin guard server: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("stop origin guard server: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("origin guard server did not stop")
		}
	})
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

	interfaceIPs := nonLoopbackInterfaceIPs(t)
	if len(interfaceIPs) == 0 {
		t.Log("this host has no non-loopback interface address; only the loopback reachability was checked")
	}
	for _, ip := range interfaceIPs {
		target := net.JoinHostPort(ip.String(), port)
		conn, err := net.DialTimeout("tcp", target, 2*time.Second)
		if err == nil {
			_ = conn.Close()
			t.Errorf("server accepted a connection at non-loopback address %s", target)
		}
	}
}

// TestServerListenRefusesPortBusyOnIPv6Loopback proves a requested port that
// another process holds on the IPv6 loopback fails the bind. Otherwise a
// browser that resolves localhost to ::1 would reach that process.
func TestServerListenRefusesPortBusyOnIPv6Loopback(t *testing.T) {
	t.Parallel()
	if !hostHasIPv6Loopback(t) {
		t.Skip("this host has no IPv6 loopback")
	}
	holder, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Fatalf("hold an IPv6 loopback port: %v", err)
	}
	defer holder.Close()
	port := holder.Addr().(*net.TCPAddr).Port

	server := NewServer(ServerConfig{Port: port})
	err = server.Listen(context.Background())
	if err == nil {
		for _, ln := range server.lns {
			_ = ln.Close()
		}
		t.Fatalf("Listen on port %d succeeded while the IPv6 loopback held it", port)
	}
	want := "listen " + net.JoinHostPort("::1", strconv.Itoa(port))
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("Listen error = %v, want it to name %q", err, want)
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
