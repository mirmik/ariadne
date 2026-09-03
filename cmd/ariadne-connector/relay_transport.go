package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/mirmik/ariadne/internal/connector"
	"github.com/mirmik/ariadne/internal/identity"
	"github.com/mirmik/ariadne/internal/knownrelay"
	"github.com/mirmik/ariadne/internal/messageconn"
	"github.com/mirmik/ariadne/internal/pairing"
	"github.com/mirmik/ariadne/internal/quictransport"
	"github.com/mirmik/ariadne/internal/transport"
	"github.com/mirmik/ariadne/internal/wire"
)

const defaultNodePort = "14771"

// A portless QUIC relay address discovers one fixed endpoint from this list.
// The first port is recommended; 47471 remains for existing deployments.
var implicitNodePorts = [...]string{"14771", "23771", "47471"}

type configuredRelayTransport struct {
	url        string
	httpClient *http.Client
	dial       func(context.Context) (messageconn.Conn, error)
}

type relayDialCandidate struct {
	endpoint string
	dial     func(context.Context) (messageconn.Conn, error)
}

type relayDialResult struct {
	connection messageconn.Conn
	err        error
	index      int
}

func configureRelayTransport(rawURL, fallbackValue, pin, knownRelaysPath string, acceptNewCertificate bool, pairingCode string, nodeIdentity *identity.Identity, logger *slog.Logger) (configuredRelayTransport, error) {
	normalizedURL, err := normalizeRelayURL(rawURL)
	if err != nil {
		return configuredRelayTransport{}, err
	}
	parsed, err := url.Parse(normalizedURL)
	if err != nil {
		return configuredRelayTransport{}, fmt.Errorf("parse relay URL: %w", err)
	}
	if !strings.EqualFold(parsed.Scheme, "quic") {
		result := configuredRelayTransport{url: normalizedURL}
		if parsed.Scheme != "https" && parsed.Scheme != "wss" {
			if pin != "" {
				return configuredRelayTransport{}, errors.New("--relay-cert-pin requires a TLS relay URL")
			}
			if acceptNewCertificate {
				return configuredRelayTransport{}, errors.New("--accept-new-relay-certificate requires a TLS relay URL")
			}
			if pairingCode != "" {
				return configuredRelayTransport{}, errors.New("--pairing-code requires a TLS relay URL")
			}
			return result, nil
		}
		trust, err := newRelayCertificateTrust(pin, knownRelaysPath, acceptNewCertificate, pairingCode, nodeIdentity, logger)
		if err != nil {
			return configuredRelayTransport{}, err
		}
		connectorURL, err := connector.ConnectorURL(normalizedURL)
		if err != nil {
			return configuredRelayTransport{}, err
		}
		result.dial = func(ctx context.Context) (messageconn.Conn, error) {
			pairingExpected := trust.pairingEnabled()
			tlsConfig, endpoint, err := trust.tlsConfig(parsed, "443")
			if err != nil {
				return nil, err
			}
			connection, err := dialWebSocketEndpoint(ctx, connectorURL, tlsHTTPClient(tlsConfig))
			if err != nil {
				return nil, err
			}
			if !pairingExpected {
				return connection, nil
			}
			return trust.pairAndRedial(ctx, connection, endpoint, parsed.Hostname(), func(config *tls.Config) (messageconn.Conn, error) {
				return dialWebSocketEndpoint(ctx, connectorURL, tlsHTTPClient(config))
			})
		}
		return result, nil
	}

	if parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return configuredRelayTransport{}, errors.New("QUIC relay URL must contain only a host and optional port")
	}
	trust, err := newRelayCertificateTrust(pin, knownRelaysPath, acceptNewCertificate, pairingCode, nodeIdentity, logger)
	if err != nil {
		return configuredRelayTransport{}, err
	}
	endpoints := quicRelayEndpoints(rawURL, parsed)
	trust.setPairingAttemptLimit(len(endpoints))
	candidates := make([]relayDialCandidate, 0, len(endpoints))
	for index, endpoint := range endpoints {
		candidateFallback := fallbackValue
		if index > 0 && explicitFallback(candidateFallback) {
			candidateFallback = "none"
		}
		fallbackURL, err := resolveFallbackURL(endpoint, candidateFallback)
		if err != nil {
			return configuredRelayTransport{}, err
		}
		var fallbackParsed *url.URL
		if fallbackURL != "" {
			if err := transport.ValidateRelayURL(fallbackURL, false); err != nil {
				return configuredRelayTransport{}, fmt.Errorf("relay fallback: %w", err)
			}
			fallbackParsed, err = url.Parse(fallbackURL)
			if err != nil {
				return configuredRelayTransport{}, fmt.Errorf("parse relay fallback URL: %w", err)
			}
			fallbackURL, err = connector.ConnectorURL(fallbackURL)
			if err != nil {
				return configuredRelayTransport{}, err
			}
		}

		address := endpoint.Host
		candidateEndpoint := endpoint
		candidateFallbackURL := fallbackURL
		candidateFallbackParsed := fallbackParsed
		candidates = append(candidates, relayDialCandidate{
			endpoint: address,
			dial: func(ctx context.Context) (messageconn.Conn, error) {
				pairingExpected := trust.pairingEnabled()
				tlsConfig, canonicalEndpoint, err := trust.tlsConfig(candidateEndpoint, defaultNodePort)
				if err != nil {
					return nil, err
				}
				var fallbackHTTPClient *http.Client
				if candidateFallbackParsed != nil {
					fallbackTLSConfig, fallbackEndpoint, err := trust.tlsConfig(candidateFallbackParsed, "443")
					if err != nil {
						return nil, err
					}
					if pairingExpected && fallbackEndpoint != canonicalEndpoint {
						return nil, errors.New("pairing requires QUIC and WSS fallback to use the same host and port")
					}
					fallbackHTTPClient = tlsHTTPClient(fallbackTLSConfig)
				}
				connection, err := dialRelayEndpoint(ctx, address, tlsConfig, candidateFallbackURL, fallbackHTTPClient, logger)
				if err != nil {
					return nil, err
				}
				if !pairingExpected {
					return connection, nil
				}
				return trust.pairAndRedial(ctx, connection, canonicalEndpoint, candidateEndpoint.Hostname(), func(config *tls.Config) (messageconn.Conn, error) {
					var pinnedFallbackClient *http.Client
					if candidateFallbackParsed != nil {
						pinnedFallbackClient = tlsHTTPClient(config.Clone())
					}
					return dialRelayEndpoint(ctx, address, config, candidateFallbackURL, pinnedFallbackClient, logger)
				})
			},
		})
	}

	return configuredRelayTransport{
		url:  normalizedURL,
		dial: stickyRelayCandidateDialer(candidates, logger),
	}, nil
}

func quicRelayEndpoints(rawURL string, relay *url.URL) []*url.URL {
	ports := []string{relay.Port()}
	if !relayInputHasExplicitPort(rawURL) {
		ports = implicitNodePorts[:]
	}
	endpoints := make([]*url.URL, 0, len(ports))
	for _, port := range ports {
		endpoint := *relay
		endpoint.Host = net.JoinHostPort(relay.Hostname(), port)
		endpoints = append(endpoints, &endpoint)
	}
	return endpoints
}

func relayInputHasExplicitPort(value string) bool {
	value = strings.TrimSpace(value)
	if strings.Contains(value, "://") {
		parsed, err := url.Parse(value)
		return err == nil && parsed.Port() != ""
	}
	if _, port, err := net.SplitHostPort(value); err == nil {
		return port != ""
	}
	return false
}

func explicitFallback(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "auto", "none":
		return false
	default:
		return true
	}
}

func stickyRelayCandidateDialer(candidates []relayDialCandidate, logger *slog.Logger) func(context.Context) (messageconn.Conn, error) {
	var selectedMu sync.Mutex
	selected := -1
	return func(ctx context.Context) (messageconn.Conn, error) {
		selectedMu.Lock()
		selectedCandidate := selected
		selectedMu.Unlock()
		if selectedCandidate >= 0 {
			return candidates[selectedCandidate].dial(ctx)
		}

		connection, winner, err := dialRelayCandidates(ctx, candidates)
		if err != nil {
			return nil, err
		}
		selectedMu.Lock()
		if selected < 0 {
			selected = winner
		}
		selectedMu.Unlock()
		if logger != nil && len(candidates) > 1 {
			logger.Info("selected relay endpoint", "endpoint", candidates[winner].endpoint)
		}
		return connection, nil
	}
}

func dialRelayCandidates(ctx context.Context, candidates []relayDialCandidate) (messageconn.Conn, int, error) {
	if len(candidates) == 0 {
		return nil, -1, errors.New("relay has no connection candidates")
	}
	if len(candidates) == 1 {
		connection, err := candidates[0].dial(ctx)
		return connection, 0, err
	}

	dialContext, cancel := context.WithCancel(ctx)
	results := make(chan relayDialResult, len(candidates))
	for index, candidate := range candidates {
		index := index
		candidate := candidate
		go func() {
			connection, err := candidate.dial(dialContext)
			if err != nil {
				err = fmt.Errorf("%s: %w", candidate.endpoint, err)
			}
			results <- relayDialResult{connection: connection, err: err, index: index}
		}()
	}

	errorsByCandidate := make([]error, len(candidates))
	remaining := len(candidates)
	for remaining > 0 {
		select {
		case result := <-results:
			remaining--
			if result.err == nil {
				cancel()
				go closeRelayDialLosers(results, remaining)
				return result.connection, result.index, nil
			}
			errorsByCandidate[result.index] = result.err
		case <-ctx.Done():
			cancel()
			go closeRelayDialLosers(results, remaining)
			return nil, -1, ctx.Err()
		}
	}
	cancel()
	return nil, -1, errors.Join(errorsByCandidate...)
}

func closeRelayDialLosers(results <-chan relayDialResult, remaining int) {
	for range remaining {
		result := <-results
		if result.connection != nil {
			result.connection.CloseNow()
		}
	}
}

func dialRelayEndpoint(ctx context.Context, address string, tlsConfig *tls.Config, fallbackURL string, fallbackHTTPClient *http.Client, logger *slog.Logger) (messageconn.Conn, error) {
	quicContext, cancel := context.WithTimeout(ctx, 4*time.Second)
	connection, quicErr := quictransport.Dial(quicContext, address, tlsConfig)
	cancel()
	if quicErr == nil {
		return connection, nil
	}
	if fallbackURL == "" {
		return nil, quicErr
	}
	if logger != nil {
		logger.Warn("QUIC relay unavailable; trying WSS fallback", "error", quicErr, "fallback", fallbackURL)
	}
	websocketConnection, websocketErr := dialWebSocketEndpoint(ctx, fallbackURL, fallbackHTTPClient)
	if websocketErr != nil {
		return nil, errors.Join(quicErr, fmt.Errorf("connect WSS fallback: %w", websocketErr))
	}
	return websocketConnection, nil
}

func dialWebSocketEndpoint(ctx context.Context, endpoint string, httpClient *http.Client) (messageconn.Conn, error) {
	connection, _, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{
		HTTPClient:      httpClient,
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return nil, err
	}
	connection.SetReadLimit(wire.MaxControlMessageSize)
	return messageconn.WebSocket{Conn: connection}, nil
}

func normalizeRelayURL(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", errors.New("relay address is empty")
	}
	if strings.Contains(value, "://") {
		return value, nil
	}
	if strings.ContainsAny(value, "/?#@") || strings.ContainsAny(value, " \t\r\n") {
		return "", errors.New("bare relay address must be a host or host:port; use a full URL for other forms")
	}

	addressValue := value
	if strings.HasPrefix(addressValue, "[") && strings.HasSuffix(addressValue, "]") {
		addressValue = strings.TrimSuffix(strings.TrimPrefix(addressValue, "["), "]")
	}
	if address, err := netip.ParseAddr(addressValue); err == nil {
		return "quic://" + net.JoinHostPort(address.String(), defaultNodePort), nil
	}
	if host, port, err := net.SplitHostPort(value); err == nil {
		if host == "" || port == "" {
			return "", errors.New("bare relay address must contain a non-empty host and port")
		}
		return "quic://" + net.JoinHostPort(host, port), nil
	}
	if strings.Contains(value, ":") {
		return "", errors.New("invalid bare relay address; use HOST:PORT or a bracketed IPv6 address")
	}
	return "quic://" + net.JoinHostPort(value, defaultNodePort), nil
}

type relayCertificateTrust struct {
	pin                  string
	store                *knownrelay.Store
	acceptNewCertificate bool
	pairingCode          string
	nodeIdentity         *identity.Identity
	logger               *slog.Logger
	mu                   sync.Mutex
	observedPins         map[string]string
	pairingAttempts      map[string]struct{}
	pairingAttemptLimit  int
	pairingComplete      bool
}

func newRelayCertificateTrust(pin, knownRelaysPath string, acceptNewCertificate bool, pairingCode string, nodeIdentity *identity.Identity, logger *slog.Logger) (*relayCertificateTrust, error) {
	if pin != "" {
		if acceptNewCertificate {
			return nil, errors.New("--relay-cert-pin and --accept-new-relay-certificate cannot be combined")
		}
		if pairingCode != "" {
			return nil, errors.New("--relay-cert-pin and --pairing-code cannot be combined")
		}
		normalizedPin, err := transport.NormalizeCertificatePin(pin)
		if err != nil {
			return nil, err
		}
		return &relayCertificateTrust{pin: normalizedPin, logger: logger}, nil
	}
	if pairingCode != "" && acceptNewCertificate {
		return nil, errors.New("--pairing-code and --accept-new-relay-certificate cannot be combined")
	}
	if pairingCode != "" && nodeIdentity == nil {
		return nil, errors.New("connector identity is required for relay pairing")
	}
	if knownRelaysPath == "" {
		return nil, errors.New("known relays file path is empty")
	}
	store, err := knownrelay.Open(knownRelaysPath)
	if err != nil {
		return nil, err
	}
	return &relayCertificateTrust{
		store:                store,
		acceptNewCertificate: acceptNewCertificate,
		pairingCode:          pairingCode,
		nodeIdentity:         nodeIdentity,
		logger:               logger,
		observedPins:         make(map[string]string),
		pairingAttempts:      make(map[string]struct{}),
		pairingAttemptLimit:  1,
	}, nil
}

func (trust *relayCertificateTrust) pairingEnabled() bool {
	trust.mu.Lock()
	defer trust.mu.Unlock()
	return trust.pairingCode != ""
}

func (trust *relayCertificateTrust) setPairingAttemptLimit(limit int) {
	if limit <= 0 {
		limit = 1
	}
	trust.mu.Lock()
	trust.pairingAttemptLimit = limit
	trust.mu.Unlock()
}

func (trust *relayCertificateTrust) beginPairingAttempt(endpoint string) (string, *identity.Identity, error) {
	trust.mu.Lock()
	defer trust.mu.Unlock()
	if trust.pairingComplete {
		return "", nil, errors.New("relay pairing has already completed")
	}
	if _, attempted := trust.pairingAttempts[endpoint]; attempted {
		return "", nil, fmt.Errorf("pairing with relay %s was already attempted; restart with a fresh pairing code", endpoint)
	}
	if trust.pairingCode == "" {
		return "", nil, errors.New("relay pairing code is no longer available; restart with a fresh pairing code")
	}
	trust.pairingAttempts[endpoint] = struct{}{}
	code := trust.pairingCode
	nodeIdentity := trust.nodeIdentity
	if len(trust.pairingAttempts) >= trust.pairingAttemptLimit {
		trust.pairingCode = ""
	}
	return code, nodeIdentity, nil
}

func (trust *relayCertificateTrust) tlsConfig(relayURL *url.URL, defaultPort string) (*tls.Config, string, error) {
	endpoint, err := canonicalRelayEndpoint(relayURL, defaultPort)
	if err != nil {
		return nil, "", err
	}
	if trust.pin != "" {
		config, err := transport.ClientTLSConfig(relayURL.Hostname(), trust.pin)
		return config, endpoint, err
	}
	if trust.pairingEnabled() {
		config := transport.ClientTLSConfigWithLeafVerifier(relayURL.Hostname(), func(certificateDER []byte) error {
			trust.mu.Lock()
			trust.observedPins[endpoint] = transport.FormatCertificatePin(certificateDER)
			trust.mu.Unlock()
			return nil
		})
		return config, endpoint, nil
	}
	knownPin, found, err := trust.store.Pin(endpoint)
	if err != nil {
		return nil, "", err
	}
	if !found {
		return nil, "", fmt.Errorf("relay %s is not paired; run `ari pair` on the relay and reconnect with --pairing-code CODE", endpoint)
	}
	if !trust.acceptNewCertificate {
		config, err := transport.ClientTLSConfig(relayURL.Hostname(), knownPin)
		return config, endpoint, err
	}
	config := transport.ClientTLSConfigWithLeafVerifier(relayURL.Hostname(), func(certificateDER []byte) error {
		trust.mu.Lock()
		defer trust.mu.Unlock()
		acceptReplacement := trust.acceptNewCertificate
		result, err := trust.store.Verify(endpoint, certificateDER, acceptReplacement)
		if err != nil {
			var changed *knownrelay.CertificateChangedError
			if errors.As(err, &changed) {
				return fmt.Errorf("%w; verify the relay identity, then rerun with --accept-new-relay-certificate", err)
			}
			return err
		}
		trust.acceptNewCertificate = false
		if trust.logger != nil {
			switch result.Decision {
			case knownrelay.Learned:
				trust.logger.Info("trusted relay certificate on first use", "endpoint", endpoint, "certificate_pin", result.Pin, "known_relays_file", trust.store.Path())
			case knownrelay.Replaced:
				trust.logger.Warn("replaced trusted relay certificate after explicit approval", "endpoint", endpoint, "previous_certificate_pin", result.PreviousPin, "certificate_pin", result.Pin, "known_relays_file", trust.store.Path())
			case knownrelay.Known:
				if acceptReplacement {
					trust.logger.Info("relay certificate is unchanged; explicit replacement approval was not needed", "endpoint", endpoint, "certificate_pin", result.Pin)
				}
			}
		}
		return nil
	})
	return config, endpoint, nil
}

func (trust *relayCertificateTrust) pairAndRedial(
	ctx context.Context,
	connection messageconn.Conn,
	endpoint string,
	serverName string,
	redial func(*tls.Config) (messageconn.Conn, error),
) (messageconn.Conn, error) {
	pairingCode, nodeIdentity, err := trust.beginPairingAttempt(endpoint)
	if err != nil {
		connection.CloseNow()
		return nil, err
	}
	pairedPin, err := pairing.PairRelay(ctx, connection, pairingCode, nodeIdentity)
	connection.CloseNow()
	if err != nil {
		return nil, err
	}
	trust.mu.Lock()
	if trust.pairingComplete {
		trust.mu.Unlock()
		return nil, errors.New("another relay pairing attempt completed first")
	}
	result, err := trust.store.TrustPin(endpoint, pairedPin, true)
	if err != nil {
		trust.mu.Unlock()
		return nil, fmt.Errorf("save paired relay identity: %w", err)
	}
	observedPin := trust.observedPins[endpoint]
	trust.pairingCode = ""
	trust.pairingComplete = true
	trust.mu.Unlock()
	if trust.logger != nil {
		trust.logger.Info("paired relay identity", "endpoint", endpoint, "certificate_pin", result.Pin, "known_relays_file", trust.store.Path())
		if observedPin != "" && observedPin != result.Pin {
			trust.logger.Warn("pairing detected an intercepting TLS certificate; reconnecting only to the authenticated relay identity", "endpoint", endpoint, "presented_certificate_pin", observedPin, "authenticated_certificate_pin", result.Pin)
		}
	}
	pinnedConfig, err := transport.ClientTLSConfig(serverName, result.Pin)
	if err != nil {
		return nil, err
	}
	return redial(pinnedConfig)
}

func canonicalRelayEndpoint(relayURL *url.URL, defaultPort string) (string, error) {
	host := strings.ToLower(strings.TrimSuffix(relayURL.Hostname(), "."))
	if host == "" {
		return "", errors.New("relay URL has no host")
	}
	port := relayURL.Port()
	if port == "" {
		port = defaultPort
	}
	if port == "" {
		return "", errors.New("relay URL has no port and no default port")
	}
	return net.JoinHostPort(host, port), nil
}

func tlsHTTPClient(tlsConfig *tls.Config) *http.Client {
	httpTransport := http.DefaultTransport.(*http.Transport).Clone()
	httpTransport.TLSClientConfig = tlsConfig
	return &http.Client{Transport: httpTransport}
}

func resolveFallbackURL(relay *url.URL, value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "auto":
		fallback := *relay
		fallback.Scheme = "https"
		fallback.Path = ""
		return fallback.String(), nil
	case "none":
		return "", nil
	default:
		return value, nil
	}
}
