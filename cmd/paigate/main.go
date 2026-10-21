// Command paigate runs the gateway and manages virtual keys.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"paibot.app/paigate/internal/gateway"
)

const usage = `usage:
  paigate serve [-config paigate.json]   run the gateway
  paigate check [-config paigate.json]   validate the config and exit
  paigate genkey                         print a new virtual key and its hash
  paigate hash                           read a key from stdin and print its hash
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(os.Args[2:])
	case "check":
		err = check(os.Args[2:])
	case "genkey":
		k := gateway.GenerateKey()
		fmt.Printf("key:  %s\nhash: %s\n", k, gateway.HashKey(k))
		fmt.Fprintln(os.Stderr, "Give the key to its user once; put only the hash in the config.")
	case "hash":
		err = hash()
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "paigate:", err)
		os.Exit(1)
	}
}

func loadConfig(args []string, name string) (*gateway.Config, error) {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	path := fs.String("config", "paigate.json", "path to the JSON config")
	fs.Parse(args)
	return gateway.LoadConfig(*path)
}

func check(args []string) error {
	cfg, err := loadConfig(args, "check")
	if err != nil {
		return err
	}
	if _, err := gateway.New(cfg, slog.New(slog.DiscardHandler), os.Getenv); err != nil {
		return err
	}
	fmt.Println("config ok")
	return nil
}

func serve(args []string) error {
	cfg, err := loadConfig(args, "serve")
	if err != nil {
		return err
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	g, err := gateway.New(cfg, log, os.Getenv)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           g,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		// No WriteTimeout: streamed completions can legitimately run long.
		IdleTimeout:    120 * time.Second,
		MaxHeaderBytes: 64 << 10,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("listening", "addr", cfg.Listen)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// hash reads the key from stdin so it never appears in argv or shell history.
func hash() error {
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return errors.New("no key on stdin")
	}
	k := strings.TrimSpace(line)
	if k == "" {
		return errors.New("no key on stdin")
	}
	fmt.Println(gateway.HashKey(k))
	return nil
}
