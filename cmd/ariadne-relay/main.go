package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mirmik/ariadne/internal/managementauth"
	"github.com/mirmik/ariadne/internal/noderegistry"
	"github.com/mirmik/ariadne/internal/quictransport"
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
	defaultManagementTokenPath, err := managementauth.DefaultPath()
	if err != nil {
		return err
	}
	defaultRegistryPath, err := noderegistry.DefaultPath()
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("ariadne-relay", flag.ContinueOnError)
	managementListen := flags.String("management-listen", "127.0.0.1:8088", "loopback-only management HTTP listen address")
	managementTLSCertificate := flags.String("management-tls-cert", "", "management-plane TLS certificate path")
	managementTLSKey := flags.String("management-tls-key", "", "management-plane TLS private key path")
	nodeListen := flags.String("node-listen", "127.0.0.1:14771", "connector-facing HTTP listen address")
	nodeLoopbackListen := flags.String("node-loopback-listen", "", "optional additional plaintext loopback node listener for SSH tunnels")
	nodeQUICListen := flags.String("node-quic-listen", "", "optional connector-facing QUIC listen address")
	nodeTLSCertificate := flags.String("node-tls-cert", "", "node-plane TLS certificate path")
	nodeTLSKey := flags.String("node-tls-key", "", "node-plane TLS private key path")
	allowInsecureManagementListen := flags.Bool("allow-insecure-management-listen", false, "allow plaintext management HTTP on a trusted non-loopback network")
	managementTokenPath := flags.String("management-token-file", defaultManagementTokenPath, "management bearer token file (created with mode 0600 if absent)")
	registryPath := flags.String("registry-file", defaultRegistryPath, "persistent node identity and alias registry file")
	pairingTTL := flags.Duration("pairing-ttl", 5*time.Minute, "lifetime of a one-time relay pairing code")
	maxPairingAttempts := flags.Int("max-pairing-attempts", 5, "maximum PAKE attempts per pairing code")
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
	if *nodeQUICListen != "" && *nodeTLSCertificate == "" {
		return errors.New("--node-quic-listen requires --node-tls-cert and --node-tls-key")
	}
	if (*managementTLSCertificate == "") != (*managementTLSKey == "") {
		return errors.New("--management-tls-cert and --management-tls-key must be provided together")
	}
	if *pairingTTL <= 0 {
		return errors.New("--pairing-ttl must be positive")
	}
	if *maxPairingAttempts <= 0 {
		return errors.New("--max-pairing-attempts must be positive")
	}
	if err := transport.ValidateListenAddress(
		*managementListen,
		*managementTLSCertificate != "",
		*allowInsecureManagementListen,
	); err != nil {
		return fmt.Errorf("management listener: %w", err)
	}
	if err := transport.ValidateListenAddress(*nodeListen, *nodeTLSCertificate != "", *allowInsecureNodeListen); err != nil {
		return fmt.Errorf("node listener: %w", err)
	}
	if *nodeLoopbackListen != "" {
		if err := transport.ValidateListenAddress(*nodeLoopbackListen, false, false); err != nil {
			return fmt.Errorf("node loopback listener: %w", err)
		}
		if *nodeLoopbackListen == *nodeListen {
			return errors.New("node and node loopback listeners must use different addresses")
		}
	}
	if *managementListen == *nodeListen {
		return errors.New("management and node listeners must use different addresses")
	}
	if *nodeLoopbackListen != "" && *managementListen == *nodeLoopbackListen {
		return errors.New("management and node loopback listeners must use different addresses")
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	managementToken, createdToken, err := managementauth.LoadOrCreate(*managementTokenPath)
	if err != nil {
		return err
	}
	if createdToken {
		logger.Info("created management token", "path", *managementTokenPath)
	}
	if *allowInsecureManagementListen {
		logger.Warn("management bearer token may cross the network without TLS", "address", *managementListen)
	}
	relayConfig := relay.DefaultConfig()
	relayConfig.Version = version
	relayConfig.ManagementToken = managementToken
	relayConfig.RegistryPath = *registryPath
	relayConfig.PairingTTL = *pairingTTL
	relayConfig.MaxPairingAttempts = *maxPairingAttempts
	if *nodeTLSCertificate != "" {
		certificate, err := tls.LoadX509KeyPair(*nodeTLSCertificate, *nodeTLSKey)
		if err != nil {
			return fmt.Errorf("load node TLS keypair: %w", err)
		}
		if len(certificate.Certificate) == 0 {
			return errors.New("node TLS certificate chain is empty")
		}
		relayConfig.RelayCertificatePin = transport.FormatCertificatePin(certificate.Certificate[0])
	}
	relayServer, err := relay.New(relayConfig, logger)
	if err != nil {
		return err
	}
	defer relayServer.Close()
	signalContext, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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
	var nodeLoopbackServer *http.Server
	if *nodeLoopbackListen != "" {
		nodeLoopbackServer = &http.Server{
			Addr:              *nodeLoopbackListen,
			Handler:           relayServer.NodeHandler(),
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       2 * time.Minute,
			MaxHeaderBytes:    32 << 10,
		}
	}
	type serverResult struct {
		name string
		err  error
	}
	serverErrors := make(chan serverResult, 4)
	go func() {
		logger.Info(
			"management plane listening",
			"address", *managementListen,
			"tls", *managementTLSCertificate != "",
		)
		if *managementTLSCertificate != "" {
			serverErrors <- serverResult{
				name: "management",
				err: managementServer.ListenAndServeTLS(
					*managementTLSCertificate,
					*managementTLSKey,
				),
			}
			return
		}
		serverErrors <- serverResult{name: "management", err: managementServer.ListenAndServe()}
	}()
	if nodeLoopbackServer != nil {
		go func() {
			logger.Info("node loopback plane listening", "address", *nodeLoopbackListen, "tls", false)
			serverErrors <- serverResult{name: "node loopback", err: nodeLoopbackServer.ListenAndServe()}
		}()
	}
	go func() {
		logger.Info("node plane listening", "address", *nodeListen, "tls", *nodeTLSCertificate != "")
		if *nodeTLSCertificate != "" {
			serverErrors <- serverResult{name: "node", err: nodeServer.ListenAndServeTLS(*nodeTLSCertificate, *nodeTLSKey)}
			return
		}
		serverErrors <- serverResult{name: "node", err: nodeServer.ListenAndServe()}
	}()
	var nodeQUICServer *quictransport.Server
	if *nodeQUICListen != "" {
		nodeQUICServer, err = quictransport.Listen(*nodeQUICListen, *nodeTLSCertificate, *nodeTLSKey)
		if err != nil {
			return err
		}
		go func() {
			logger.Info(
				"node QUIC plane listening",
				"address", nodeQUICServer.Addr(),
				"tls", true,
				"certificate_pin", nodeQUICServer.CertificatePin(),
			)
			serverErrors <- serverResult{
				name: "node QUIC",
				err:  nodeQUICServer.Serve(signalContext, relayServer.ServeNodeConnection),
			}
		}()
	}

	var runError error
	select {
	case result := <-serverErrors:
		if !errors.Is(result.err, http.ErrServerClosed) && !errors.Is(result.err, context.Canceled) {
			runError = fmt.Errorf("%s server: %w", result.name, result.err)
		}
	case <-signalContext.Done():
	}

	relayServer.Close()
	if nodeQUICServer != nil {
		_ = nodeQUICServer.Close()
	}
	shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	shutdownErrors := []error{
		managementServer.Shutdown(shutdownContext),
		nodeServer.Shutdown(shutdownContext),
	}
	if nodeLoopbackServer != nil {
		shutdownErrors = append(shutdownErrors, nodeLoopbackServer.Shutdown(shutdownContext))
	}
	shutdownError := errors.Join(shutdownErrors...)
	if runError != nil || shutdownError != nil {
		return errors.Join(runError, shutdownError)
	}
	return nil
}
