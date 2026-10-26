//go:build unix

package api

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
)

// TestServerListenRefusesPortBoundOnIPv6Loopback proves a requested port that
// another socket holds on the IPv6 loopback without accepting connections
// fails the bind itself, and that the IPv4 listener bound first is released.
func TestServerListenRefusesPortBoundOnIPv6Loopback(t *testing.T) {
	t.Parallel()
	if !hostHasIPv6Loopback(t) {
		t.Skip("this host has no IPv6 loopback")
	}
	// Another process may hold the same port number on the IPv4 loopback, and
	// then the bind fails there first. Choose a new port when that happens.
	for attempt := 1; attempt <= 5; attempt++ {
		fd, port := bindIPv6LoopbackWithoutListening(t)
		// The refusal below relies on a non-listening bind blocking a later
		// listen. Verify that premise here: an environment that does not
		// enforce it cannot exercise the refusal.
		if !hostRefusesListenOnBoundIPv6Loopback(t, port) {
			_ = syscall.Close(fd)
			t.Skip("this host does not refuse a listen on an IPv6 loopback port that a non-listening socket already bound")
		}
		server := NewServer(ServerConfig{Port: port})
		err := server.Listen(context.Background())
		if err == nil {
			_ = syscall.Close(fd)
			t.Fatalf("Listen on port %d succeeded while the IPv6 loopback held it", port)
		}
		if strings.Contains(err.Error(), "listen "+net.JoinHostPort(defaults.LoopbackIPv4, strconv.Itoa(port))) {
			_ = syscall.Close(fd)
			continue
		}
		want := "listen " + net.JoinHostPort(defaults.LoopbackIPv6, strconv.Itoa(port))
		if !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "address already in use") {
			_ = syscall.Close(fd)
			t.Fatalf("Listen error = %v, want it to name %q as in use", err, want)
		}
		// The failed Listen must not keep the IPv4 loopback listener it bound
		// before the IPv6 bind failed.
		if addr := server.Addr(); addr != nil {
			_ = syscall.Close(fd)
			t.Fatalf("the failed Listen kept a listener at %s", addr)
		}
		released, err := bindLoopbackIPv4WhenFree(t, port)
		_ = syscall.Close(fd)
		if err != nil {
			t.Fatalf("the failed Listen kept the IPv4 loopback port %d: %v", port, err)
		}
		_ = released.Close()
		return
	}
	t.Fatal("every chosen port was also held on the IPv4 loopback")
}

// hostRefusesListenOnBoundIPv6Loopback reports whether a listen on the IPv6
// loopback port fails with "address already in use" while a non-listening
// socket still holds it. The server's refusal relies on that behavior, so a
// host that does not enforce it cannot run the refusal assertion.
func hostRefusesListenOnBoundIPv6Loopback(t *testing.T, port int) bool {
	t.Helper()
	ln, err := net.Listen("tcp6", net.JoinHostPort(defaults.LoopbackIPv6, strconv.Itoa(port)))
	if err == nil {
		_ = ln.Close()
		return false
	}
	return errors.Is(err, syscall.EADDRINUSE)
}

// bindLoopbackIPv4WhenFree binds the IPv4 loopback port, retrying for up to
// defaults.ServerPortReleaseWait. A parallel test can bind the same port
// number in the window between the server's release and this check, so a
// single attempt would fail spuriously while a server that kept the port
// never frees it.
func bindLoopbackIPv4WhenFree(t *testing.T, port int) (net.Listener, error) {
	t.Helper()
	addr := net.JoinHostPort(defaults.LoopbackIPv4, strconv.Itoa(port))
	deadline := time.Now().Add(defaults.ServerPortReleaseWait)
	for {
		ln, err := net.Listen("tcp4", addr)
		if err == nil {
			return ln, nil
		}
		if time.Now().After(deadline) {
			return nil, err
		}
		time.Sleep(defaults.ServerPortProbeInterval)
	}
}

// bindIPv6LoopbackWithoutListening binds a TCP socket to an ephemeral port on
// the IPv6 loopback and does not listen, so a connection to it is refused
// while a second bind to the port fails.
func bindIPv6LoopbackWithoutListening(t *testing.T) (int, int) {
	t.Helper()
	fd, err := syscall.Socket(syscall.AF_INET6, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("open an IPv6 socket: %v", err)
	}
	// Child processes that parallel tests start must not inherit the socket.
	syscall.CloseOnExec(fd)
	loopback := &syscall.SockaddrInet6{}
	copy(loopback.Addr[:], net.IPv6loopback)
	if err := syscall.Bind(fd, loopback); err != nil {
		_ = syscall.Close(fd)
		t.Fatalf("bind the IPv6 loopback: %v", err)
	}
	bound, err := syscall.Getsockname(fd)
	if err != nil {
		_ = syscall.Close(fd)
		t.Fatalf("read the bound IPv6 port: %v", err)
	}
	return fd, bound.(*syscall.SockaddrInet6).Port
}
