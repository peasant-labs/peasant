//go:build unix

package api

import (
	"context"
	"net"
	"strconv"
	"strings"
	"syscall"
	"testing"

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
		err := NewServer(ServerConfig{Port: port}).Listen(context.Background())
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
		released, err := net.Listen("tcp4", net.JoinHostPort(defaults.LoopbackIPv4, strconv.Itoa(port)))
		_ = syscall.Close(fd)
		if err != nil {
			t.Fatalf("the failed Listen kept the IPv4 loopback port %d: %v", port, err)
		}
		_ = released.Close()
		return
	}
	t.Fatal("every chosen port was also held on the IPv4 loopback")
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
