package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mirmik/ariadne/internal/relay"
	"github.com/mirmik/ariadne/internal/transport"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "ariadne-relay:", err)
		os.Exit(1)
	}
}

func run() error {
	flags := flag.NewFlagSet("ariadne-relay", flag.ContinueOnError)
	listenAddress := flags.String("listen", "127.0.0.1:8088", "HTTP listen address")
	token := flags.String("token", os.Getenv("ARIADNE_TOKEN"), "shared bearer token (prefer ARIADNE_TOKEN)")
	insecureNoAuth := flags.Bool("insecure-no-auth", false, "allow relay without authentication")
	tlsCertificate := flags.String("tls-cert", "", "TLS certificate path")
	tlsKey := flags.String("tls-key", "", "TLS private key path")
	allowInsecureListen := flags.Bool("allow-insecure-listen", false, "allow plaintext HTTP on a non-loopback address")
	verbose := flags.Bool("verbose", false, "enable debug logs")
	showVersion := flags.Bool("version", false, "print version and exit")
	if err := flags.Parse(os.Args[1:]); err != nil {
		return err
	}
	if *showVersion {
		fmt.Println(version)
		return nil
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", flags.Args())
	}
	if *token == "" && !*insecureNoAuth {
		return errors.New("ARIADNE_TOKEN is required unless --insecure-no-auth is explicitly set")
	}
	if (*tlsCertificate == "") != (*tlsKey == "") {
		return errors.New("--tls-cert and --tls-key must be provided together")
	}
	if err := transport.ValidateListenAddress(*listenAddress, *tlsCertificate != "", *allowInsecureListen); err != nil {
		return err
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	relayConfig := relay.DefaultConfig()
	relayConfig.Version = version
	relayConfig.Token = *token
	relayServer := relay.New(relayConfig, logger)
	defer relayServer.Close()

	httpServer := &http.Server{
		Addr:              *listenAddress,
		Handler:           relayServer.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    32 << 10,
	}
	serverErrors := make(chan error, 1)
	go func() {
		logger.Info("relay listening", "address", *listenAddress, "tls", *tlsCertificate != "")
		var err error
		if *tlsCertificate != "" {
			err = httpServer.ListenAndServeTLS(*tlsCertificate, *tlsKey)
		} else {
			err = httpServer.ListenAndServe()
		}
		serverErrors <- err
	}()

	signalContext, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	select {
	case err := <-serverErrors:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-signalContext.Done():
	}

	relayServer.Close()
	shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownContext); err != nil {
		return fmt.Errorf("shut down HTTP server: %w", err)
	}
	return nil
}
