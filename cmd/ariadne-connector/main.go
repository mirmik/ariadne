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
	if err := transport.ValidateRelayURL(*relayURL, *allowInsecureRelay); err != nil {
		return err
	}
	nodeIdentity, created, err := identity.LoadOrCreate(*identityPath)
	if err != nil {
		return err
	}
	if created {
		logger.Info("created node identity", "path", *identityPath, "node_id", nodeIdentity.NodeID())
	}
	instance, err := connector.New(connector.Config{
		RelayURL:          *relayURL,
		Alias:             *alias,
		Version:           version,
		Identity:          nodeIdentity,
		SSHAddress:        *sshAddress,
		Shell:             *shell,
		MaxConcurrentExec: *maxConcurrentExec,
		MaxExecTimeout:    *maxExecTimeout,
		MaxOutputBytes:    *maxOutput,
		MaxStreams:        *maxStreams,
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
	case err := <-connectorErrors:
		return err
	case <-tunnel.Done():
		cancelConnector()
		<-connectorErrors
		if runContext.Err() != nil {
			return nil
		}
		if err := tunnel.Err(); err != nil {
			return fmt.Errorf("SSH node tunnel ended: %w", err)
		}
		return errors.New("SSH node tunnel ended")
	}
}
