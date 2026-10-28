// Command grokbotd runs the Grok Bot local daemon bound strictly to the
// 127.0.0.1 loopback interface.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/captain-corgi/go-bot-example/internal/api"
	"github.com/captain-corgi/go-bot-example/internal/server"
	"github.com/captain-corgi/go-bot-example/internal/session"
)

const shutdownTimeout = 10 * time.Second

func main() {
	addr := flag.String("addr", "127.0.0.1:8787", "loopback bind address (host must be 127.0.0.1)")
	flag.Parse()

	if err := run(*addr); err != nil {
		fmt.Fprintln(os.Stderr, "grokbotd:", err)
		os.Exit(1)
	}
}

func run(addr string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	manager, err := session.NewManager(session.Options{NewTransport: newAgentTransport})
	if err != nil {
		return err
	}
	router := server.NewRouter()
	api.NewHandler(manager).Register(router)

	srv, err := server.New(ctx, addr, router)
	if err != nil {
		return err
	}

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve() }()
	log.Printf("grokbotd listening on http://%s", srv.Addr())

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	// Tearing sessions down first ends in-flight SSE streams so the HTTP
	// server can drain its connections.
	sessionErr := manager.Shutdown(shutdownCtx)
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return errors.Join(err, sessionErr)
	}
	return errors.Join(<-errCh, sessionErr)
}
