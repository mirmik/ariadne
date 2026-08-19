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
	relayURL := flags.String("relay", "http://127.0.0.1:8088", "relay base URL")
	alias := flags.String("alias", "", "stable human-readable node alias")
	identityPath := flags.String("identity", defaultIdentityPath, "persistent Ed25519 identity file")
	sshAddress := flags.String("ssh-address", "127.0.0.1:8022", "optional local sshd TCP address used by ari proxy")
	shell := flags.String("shell", "", "shell executable for zero-config ari shell (defaults to SHELL, then sh)")
	token := flags.String("token", os.Getenv("ARIADNE_TOKEN"), "shared bearer token (prefer ARIADNE_TOKEN)")
	insecureNoAuth := flags.Bool("insecure-no-auth", false, "connect without authentication")
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
	if *token == "" && !*insecureNoAuth {
		return errors.New("ARIADNE_TOKEN is required unless --insecure-no-auth is explicitly set")
	}
	if err := transport.ValidateRelayURL(*relayURL, *allowInsecureRelay); err != nil {
		return err
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	nodeIdentity, created, err := identity.LoadOrCreate(*identityPath)
	if err != nil {
		return err
	}
	if created {
		logger.Info("created node identity", "path", *identityPath, "node_id", nodeIdentity.NodeID())
	}
	instance, err := connector.New(connector.Config{
		RelayURL:          *relayURL,
		Token:             *token,
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
	runContext, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return instance.Run(runContext)
}
