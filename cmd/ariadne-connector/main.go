package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/mirmik/ariadne/internal/cliargs"
	"github.com/mirmik/ariadne/internal/connector"
	"github.com/mirmik/ariadne/internal/identity"
	"github.com/mirmik/ariadne/internal/knownrelay"
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
	defaultKnownRelaysPath, err := knownrelay.DefaultPath()
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("ariadne-connector", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	relayURL := flags.String("relay", "http://127.0.0.1:47471", "node-plane relay address or URL (bare host defaults to quic://HOST:47471)")
	relaySSH := flags.String("relay-ssh", "", "reach node plane through OpenSSH (user@host or user@host:port)")
	relayCertificatePin := flags.String("relay-cert-pin", "", "optional pre-provisioned SHA-256 pin of the relay TLS leaf certificate")
	knownRelaysPath := flags.String("known-relays-file", defaultKnownRelaysPath, "TOFU relay certificate store")
	acceptNewRelayCertificate := flags.Bool("accept-new-relay-certificate", false, "explicitly accept one changed relay certificate in the TOFU store")
	relayFallback := flags.String("relay-fallback", "auto", "WSS fallback for quic:// relay: auto, none, or an HTTPS/WSS URL")
	alias := flags.String("alias", "", "reported human-readable label (defaults to the local hostname)")
	identityPath := flags.String("identity", defaultIdentityPath, "persistent Ed25519 identity file")
	sshAddress := flags.String("ssh-address", "127.0.0.1:8022", "optional local sshd TCP address used by ari proxy")
	shell := flags.String("shell", "", "shell executable for zero-config ari shell (defaults to SHELL, then sh)")
	allowInsecureRelay := flags.Bool("allow-insecure-relay", false, "allow plaintext relay outside loopback")
	maxConcurrentExec := flags.Int("max-concurrent-exec", 4, "maximum concurrent exec requests")
	maxExecTimeout := flags.Duration("max-exec-timeout", 10*time.Minute, "maximum exec duration")
	maxOutput := flags.Int("max-output", 1<<20, "maximum captured bytes for each output stream")
	maxFileSize := flags.Int64("max-file-size", 1<<30, "maximum upload or download size in bytes")
	maxStreams := flags.Int("max-streams", 64, "maximum simultaneous shell and SSH proxy streams")
	maxConcurrentJobs := flags.Int("max-concurrent-jobs", 4, "maximum concurrent background jobs")
	maxJobOutput := flags.Int64("max-job-output", 16<<20, "maximum spooled bytes for each job output stream")
	maxRetainedJobs := flags.Int("max-retained-jobs", 64, "maximum retained completed background jobs")
	maxJobTimeout := flags.Duration("max-job-timeout", 24*time.Hour, "maximum background job timeout")
	jobRetention := flags.Duration("job-retention", 24*time.Hour, "completed background job retention")
	verbose := flags.Bool("verbose", false, "enable debug logs")
	showVersion := flags.Bool("version", false, "print version and exit")
	flags.Usage = func() { printConnectorUsage(flags) }
	if err := flags.Parse(cliargs.Current()); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *showVersion {
		fmt.Println(version)
		return nil
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", flags.Args())
	}
	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	connectorAlias, hostnameDefault, err := resolveConnectorAlias(*alias, os.Hostname)
	if err != nil {
		return err
	}
	if hostnameDefault {
		logger.Info("using hostname as connector alias", "alias", connectorAlias)
	}
	runContext, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var tunnel *sshtunnel.Tunnel
	if *relaySSH != "" {
		if *relayCertificatePin != "" {
			return errors.New("--relay-cert-pin is not used with --relay-ssh")
		}
		if *acceptNewRelayCertificate {
			return errors.New("--accept-new-relay-certificate is not used with --relay-ssh")
		}
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
	relayTransport, err := configureRelayTransport(
		*relayURL,
		*relayFallback,
		*relayCertificatePin,
		*knownRelaysPath,
		*acceptNewRelayCertificate,
		logger,
	)
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
	nodeIdentity, created, err := identity.LoadOrCreate(*identityPath)
	if err != nil {
		return err
	}
	if created {
		logger.Info("created node identity", "path", *identityPath, "node_id", nodeIdentity.NodeID())
	}
	instance, err := connector.New(connector.Config{
		RelayURL:          relayTransport.url,
		Alias:             connectorAlias,
		Version:           version,
		Identity:          nodeIdentity,
		SSHAddress:        *sshAddress,
		Shell:             *shell,
		MaxConcurrentExec: *maxConcurrentExec,
		MaxExecTimeout:    *maxExecTimeout,
		MaxOutputBytes:    *maxOutput,
		MaxFileBytes:      *maxFileSize,
		MaxStreams:        *maxStreams,
		MaxConcurrentJobs: *maxConcurrentJobs,
		MaxJobOutputBytes: *maxJobOutput,
		MaxRetainedJobs:   *maxRetainedJobs,
		MaxJobTimeout:     *maxJobTimeout,
		JobRetention:      *jobRetention,
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

func resolveConnectorAlias(explicit string, hostname func() (string, error)) (string, bool, error) {
	if explicit != "" {
		return explicit, false, nil
	}
	name, err := hostname()
	if err != nil {
		return "", false, fmt.Errorf("determine hostname for default alias: %w; pass --alias explicitly", err)
	}
	alias := normalizeHostnameAlias(name)
	if alias == "" {
		return "", false, fmt.Errorf("hostname %q cannot form a connector alias; pass --alias explicitly", name)
	}
	return alias, true, nil
}

func normalizeHostnameAlias(hostname string) string {
	const maximumAliasLength = 63
	trimmed := strings.TrimSpace(hostname)
	alias := make([]byte, 0, min(len(trimmed), maximumAliasLength))
	for index := 0; index < len(trimmed) && len(alias) < maximumAliasLength; index++ {
		character := trimmed[index]
		alphanumeric := character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9'
		if alphanumeric {
			alias = append(alias, character)
			continue
		}
		if len(alias) == 0 {
			continue
		}
		if character == '.' || character == '_' || character == '-' {
			alias = append(alias, character)
			continue
		}
		if alias[len(alias)-1] != '-' {
			alias = append(alias, '-')
		}
	}
	return strings.TrimRight(string(alias), "._-")
}

func printConnectorUsage(flags *flag.FlagSet) {
	output := flags.Output()
	fmt.Fprintln(output, `Usage:
  ariadne-connector --relay HOST [options]
  ariadne-connector --relay-ssh USER@HOST[:PORT] [options]

Ariadne connector keeps an outgoing node connection to the relay and exposes
shell, structured exec, file transfer, and connector-owned background jobs
only through requests received from the management plane.
When --alias is omitted, the connector reports a normalized local hostname.

Relay addresses:
  HOST                 QUIC on the default port: quic://HOST:47471
  HOST:PORT            QUIC on an explicit port
  quic://HOST[:PORT]   Explicit QUIC; WSS fallback is automatic by default
  https://HOST[:PORT]  WSS only
  http://HOST[:PORT]   Plaintext; allowed on loopback unless explicitly enabled

Relay trust (direct QUIC/WSS):
  On first use, the relay certificate fingerprint is stored in known_relays.
  Later connections require the same fingerprint. A changed certificate is
  rejected with both fingerprints in the error. After verifying the relay,
  rerun once with --accept-new-relay-certificate to replace the stored value.
  --relay-cert-pin bypasses TOFU and requires an exact pre-provisioned pin.

Examples:
  ariadne-connector --relay relay.example
  ariadne-connector --relay relay.example --alias workstation
  ariadne-connector --relay relay.example:48123 --alias workstation
  ariadne-connector --relay https://relay.example:47471 --alias workstation
  ariadne-connector --relay relay.example --accept-new-relay-certificate --alias workstation
  ariadne-connector --relay-ssh breakglass@relay.example:22061 --alias workstation

Options:`)
	flags.PrintDefaults()
}
