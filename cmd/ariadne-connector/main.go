package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mirmik/ariadne/internal/cliargs"
	"github.com/mirmik/ariadne/internal/connector"
	"github.com/mirmik/ariadne/internal/identity"
	"github.com/mirmik/ariadne/internal/sshtunnel"
	"github.com/mirmik/ariadne/internal/transport"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "ariadne-connector:", err)
		os.Exit(1)
	}
}

func run() error {
	defaultIdentityPath, err := identity.DefaultPath()
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("ariadne-connector", flag.ContinueOnError)
	relayURL := flags.String("relay", "http://127.0.0.1:47471", "node-plane relay base URL")
	relaySSH := flags.String("relay-ssh", "", "reach node plane through OpenSSH (user@host or user@host:port)")
	relayCertificatePin := flags.String("relay-cert-pin", "", "optional SHA-256 pin of the relay TLS leaf certificate")
	relayFallback := flags.String("relay-fallback", "auto", "WSS fallback for quic:// relay: auto, none, or an HTTPS/WSS URL")
	alias := flags.String("alias", "", "reported human-readable label (claimed on the management plane)")
	identityPath := flags.String("identity", defaultIdentityPath, "persistent Ed25519 identity file")
	sshAddress := flags.String("ssh-address", "127.0.0.1:8022", "optional local sshd TCP address used by ari proxy")
	shell := flags.String("shell", "", "shell executable for zero-config ari shell (defaults to SHELL, then sh)")
	allowInsecureRelay := flags.Bool("allow-insecure-relay", false, "allow plaintext relay outside loopback")
	maxConcurrentExec := flags.Int("max-concurrent-exec", 4, "maximum concurrent exec requests")
	maxExecTimeout := flags.Duration("max-exec-timeout", 10*time.Minute, "maximum exec duration")
	maxOutput := flags.Int("max-output", 1<<20, "maximum captured bytes for each output stream")
	maxStreams := flags.Int("max-streams", 64, "maximum simultaneous shell and SSH proxy streams")
	verbose := flags.Bool("verbose", false, "enable debug logs")
	showVersion := flags.Bool("version", false, "print version and exit")
	if err := flags.Parse(cliargs.Current()); err != nil {
		return err
	}
	if *showVersion {
		fmt.Println(version)
		return nil
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", flags.Args())
	}
	if *alias == "" {
		return errors.New("--alias is required")
	}
	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	runContext, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var tunnel *sshtunnel.Tunnel
	if *relaySSH != "" {
		tunnel, err = sshtunnel.Start(runContext, sshtunnel.Config{
			Destination:   *relaySSH,
			RemoteAddress: "127.0.0.1:47471",
		})
		if err != nil {
			return err
		}
		defer tunnel.Close()
		*relayURL = tunnel.URL
	}
	relayTransport, err := configureRelayTransport(*relayURL, *relayFallback, *relayCertificatePin, logger)
	if err != nil {
		return err
	}
	if relayTransport.dial == nil {
		if err := transport.ValidateRelayURL(relayTransport.url, *allowInsecureRelay); err != nil {
			return err
		}
	}
	if tunnel != nil && relayTransport.dial != nil {
		return errors.New("--relay-ssh cannot be combined with a QUIC relay URL")
	}
	if tunnel != nil && *relayCertificatePin != "" {
		return errors.New("--relay-cert-pin is not used with --relay-ssh")
	}
	nodeIdentity, created, err := identity.LoadOrCreate(*identityPath)
	if err != nil {
		return err
	}
	if created {
		logger.Info("created node identity", "path", *identityPath, "node_id", nodeIdentity.NodeID())
	}
	instance, err := connector.New(connector.Config{
		RelayURL:          relayTransport.url,
		Alias:             *alias,
		Version:           version,
		Identity:          nodeIdentity,
		SSHAddress:        *sshAddress,
		Shell:             *shell,
		MaxConcurrentExec: *maxConcurrentExec,
		MaxExecTimeout:    *maxExecTimeout,
		MaxOutputBytes:    *maxOutput,
		MaxStreams:        *maxStreams,
		HTTPClient:        relayTransport.httpClient,
		Dial:              relayTransport.dial,
		Logger:            logger,
	}, nil)
	if err != nil {
		return err
	}
	if tunnel == nil {
		return instance.Run(runContext)
	}
	connectorContext, cancelConnector := context.WithCancel(runContext)
	defer cancelConnector()
	connectorErrors := make(chan error, 1)
	go func() { connectorErrors <- instance.Run(connectorContext) }()
	select {
	case <-runContext.Done():
		logger.Info("shutting down")
		cancelConnector()
		tunnel.Close()
		select {
		case <-connectorErrors:
		case <-time.After(5 * time.Second):
			logger.Warn("connector session did not stop before shutdown timeout")
		}
		return nil
	case err := <-connectorErrors:
		return err
	case <-tunnel.Done():
		cancelConnector()
		select {
		case <-connectorErrors:
		case <-time.After(5 * time.Second):
			return errors.New("connector session did not stop after SSH tunnel ended")
		}
		if runContext.Err() != nil {
			return nil
		}
		if err := tunnel.Err(); err != nil {
			return fmt.Errorf("SSH node tunnel ended: %w", err)
		}
		return errors.New("SSH node tunnel ended")
	}
}
