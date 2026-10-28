package server

import (
	"context"
	"errors"
	"net"
	"testing"
)

func TestValidateLoopbackAddrAccepts(t *testing.T) {
	tests := []struct {
		addr     string
		wantPort int
	}{
		{"127.0.0.1:0", 0},
		{"127.0.0.1:1", 1},
		{"127.0.0.1:8080", 8080},
		{"127.0.0.1:65535", 65535},
	}
	for _, tc := range tests {
		t.Run(tc.addr, func(t *testing.T) {
			host, port, err := ValidateLoopbackAddr(tc.addr)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if host != LoopbackHost || port != tc.wantPort {
				t.Errorf("got (%q, %d), want (%q, %d)", host, port, LoopbackHost, tc.wantPort)
			}
		})
	}
}

func TestValidateLoopbackAddrRejects(t *testing.T) {
	tests := []struct {
		addr        string
		nonLoopback bool
	}{
		{"0.0.0.0:8080", true},
		{":8080", true},
		{"[::]:8080", true},
		{"[::1]:8080", true},
		{"192.168.1.10:8080", true},
		{"8.8.8.8:8080", true},
		{"127.0.0.2:8080", true},
		{"localhost:8080", true},
		{"example.com:8080", true},
		{"[::ffff:127.0.0.1]:8080", true},
		{"127.0.0.1", false},
		{"127.0.0.1:", false},
		{"127.0.0.1:-1", false},
		{"127.0.0.1:65536", false},
		{"127.0.0.1:http", false},
		{"", false},
	}
	for _, tc := range tests {
		t.Run(tc.addr, func(t *testing.T) {
			_, _, err := ValidateLoopbackAddr(tc.addr)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if tc.nonLoopback && !errors.Is(err, ErrNonLoopbackAddr) {
				t.Errorf("error %v does not wrap ErrNonLoopbackAddr", err)
			}
		})
	}
}

func TestListenLoopbackBindsLoopback(t *testing.T) {
	ln, err := ListenLoopback(context.Background(), "127.0.0.1:0")
	if err != nil {
		t.Fatalf("ListenLoopback: %v", err)
	}
	defer ln.Close()

	tcpAddr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener address %T is not *net.TCPAddr", ln.Addr())
	}
	if !tcpAddr.IP.Equal(net.IPv4(127, 0, 0, 1)) {
		t.Errorf("bound IP = %v, want 127.0.0.1", tcpAddr.IP)
	}
	if tcpAddr.Port == 0 {
		t.Error("expected an assigned ephemeral port")
	}
}

func TestListenLoopbackFailsFast(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:0", ":0", "[::]:0", "[::1]:0", "192.168.1.10:0", "example.com:0"} {
		t.Run(addr, func(t *testing.T) {
			ln, err := ListenLoopback(context.Background(), addr)
			if err == nil {
				ln.Close()
				t.Fatal("expected error, got listener")
			}
			if ln != nil {
				t.Errorf("expected nil listener, got %v", ln.Addr())
			}
		})
	}
}

func TestListenLoopbackPortInUse(t *testing.T) {
	first, err := ListenLoopback(context.Background(), "127.0.0.1:0")
	if err != nil {
		t.Fatalf("ListenLoopback: %v", err)
	}
	defer first.Close()

	if ln, err := ListenLoopback(context.Background(), first.Addr().String()); err == nil {
		ln.Close()
		t.Fatal("expected error binding an in-use port")
	}
}
