// Command grokbotd runs the Grok Bot local daemon bound strictly to the
// 127.0.0.1 loopback interface.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/captain-corgi/go-bot-example/internal/server"
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

	srv, err := server.New(ctx, addr, server.NewRouter())
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
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	return <-errCh
}
