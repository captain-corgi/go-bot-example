package server

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/captain-corgi/go-bot-example/internal/problem"
)

func startServer(t *testing.T, handler http.Handler) *Server {
	t.Helper()
	srv, err := New(context.Background(), "127.0.0.1:0", handler)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
		if err := <-errCh; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	return srv
}

func doRequest(t *testing.T, srv *Server, host, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://"+srv.Addr().String()+path, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Host = host
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func TestNewRefusesNonLoopback(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:0", ":0", "[::]:0", "[::1]:0", "10.0.0.1:0", "localhost:0"} {
		t.Run(addr, func(t *testing.T) {
			srv, err := New(context.Background(), addr, NewRouter())
			if err == nil {
				_ = srv.Shutdown(context.Background())
				t.Fatal("expected startup refusal")
			}
			if !errors.Is(err, ErrNonLoopbackAddr) {
				t.Errorf("error %v does not wrap ErrNonLoopbackAddr", err)
			}
		})
	}
}

func TestNewRejectsNilHandler(t *testing.T) {
	if _, err := New(context.Background(), "127.0.0.1:0", nil); err == nil {
		t.Fatal("expected error for nil handler")
	}
}

func TestServerEndToEnd(t *testing.T) {
	rt := NewRouter()
	rt.HandleFunc("GET /ping", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "pong")
	})
	srv := startServer(t, rt)

	tcpAddr, ok := srv.Addr().(*net.TCPAddr)
	if !ok || !tcpAddr.IP.Equal(net.IPv4(127, 0, 0, 1)) {
		t.Fatalf("server bound to %v, want 127.0.0.1", srv.Addr())
	}
	if srv.Port() != tcpAddr.Port || srv.Port() == 0 {
		t.Fatalf("Port() = %d, want %d", srv.Port(), tcpAddr.Port)
	}
	port := strconv.Itoa(srv.Port())

	t.Run("valid host reaches route", func(t *testing.T) {
		for _, host := range []string{"127.0.0.1:" + port, "localhost:" + port, "localhost"} {
			resp := doRequest(t, srv, host, "/ping")
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusOK || string(body) != "pong" {
				t.Errorf("Host %q: status %d body %q", host, resp.StatusCode, body)
			}
		}
	})

	t.Run("valid host unknown route is 404 problem", func(t *testing.T) {
		resp := doRequest(t, srv, "127.0.0.1:"+port, "/nope")
		if resp.StatusCode != http.StatusNotFound || resp.Header.Get("Content-Type") != problem.ContentType {
			t.Errorf("status %d content-type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
		}
	})

	t.Run("rebinding host is 400 problem", func(t *testing.T) {
		for _, host := range []string{"evil.com:" + port, "127.0.0.1.evil.com", "127.0.0.1:1", "0.0.0.0:" + port} {
			resp := doRequest(t, srv, host, "/ping")
			if resp.StatusCode != http.StatusBadRequest || resp.Header.Get("Content-Type") != problem.ContentType {
				t.Errorf("Host %q: status %d content-type %q", host, resp.StatusCode, resp.Header.Get("Content-Type"))
			}
		}
	})
}

func TestShutdownWithoutServe(t *testing.T) {
	srv, err := New(context.Background(), "127.0.0.1:0", NewRouter())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if _, err := net.Dial("tcp4", srv.Addr().String()); err == nil {
		t.Error("listener still accepting after Shutdown")
	}
}
