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
	managementListen := flags.String("management-listen", "127.0.0.1:8088", "loopback-only management HTTP listen address")
	nodeListen := flags.String("node-listen", "127.0.0.1:47471", "connector-facing HTTP listen address")
	nodeTLSCertificate := flags.String("node-tls-cert", "", "node-plane TLS certificate path")
	nodeTLSKey := flags.String("node-tls-key", "", "node-plane TLS private key path")
	allowManagementNetwork := flags.Bool("allow-management-network", false, "allow unauthenticated management HTTP on a trusted non-loopback network")
	allowInsecureNodeListen := flags.Bool("allow-insecure-node-listen", false, "allow plaintext node plane on a non-loopback address")
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
	if (*nodeTLSCertificate == "") != (*nodeTLSKey == "") {
		return errors.New("--node-tls-cert and --node-tls-key must be provided together")
	}
	if err := transport.ValidateListenAddress(*managementListen, false, *allowManagementNetwork); err != nil {
		return fmt.Errorf("management listener: %w", err)
	}
	if err := transport.ValidateListenAddress(*nodeListen, *nodeTLSCertificate != "", *allowInsecureNodeListen); err != nil {
		return fmt.Errorf("node listener: %w", err)
	}
	if *managementListen == *nodeListen {
		return errors.New("management and node listeners must use different addresses")
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	relayConfig := relay.DefaultConfig()
	relayConfig.Version = version
	relayServer := relay.New(relayConfig, logger)
	defer relayServer.Close()

	managementServer := &http.Server{
		Addr:              *managementListen,
		Handler:           relayServer.ManagementHandler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    32 << 10,
	}
	nodeServer := &http.Server{
		Addr:              *nodeListen,
		Handler:           relayServer.NodeHandler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    32 << 10,
	}
	type serverResult struct {
		name string
		err  error
	}
	serverErrors := make(chan serverResult, 2)
	go func() {
		logger.Info("management plane listening", "address", *managementListen)
		serverErrors <- serverResult{name: "management", err: managementServer.ListenAndServe()}
	}()
	go func() {
		logger.Info("node plane listening", "address", *nodeListen, "tls", *nodeTLSCertificate != "")
		if *nodeTLSCertificate != "" {
			serverErrors <- serverResult{name: "node", err: nodeServer.ListenAndServeTLS(*nodeTLSCertificate, *nodeTLSKey)}
			return
		}
		serverErrors <- serverResult{name: "node", err: nodeServer.ListenAndServe()}
	}()

	signalContext, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var runError error
	select {
	case result := <-serverErrors:
		if !errors.Is(result.err, http.ErrServerClosed) {
			runError = fmt.Errorf("%s server: %w", result.name, result.err)
		}
	case <-signalContext.Done():
	}

	relayServer.Close()
	shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	shutdownError := errors.Join(
		managementServer.Shutdown(shutdownContext),
		nodeServer.Shutdown(shutdownContext),
	)
	if runError != nil || shutdownError != nil {
		return errors.Join(runError, shutdownError)
	}
	return nil
}
