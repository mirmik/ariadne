package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/mirmik/ariadne/internal/connector"
	"github.com/mirmik/ariadne/internal/knownrelay"
	"github.com/mirmik/ariadne/internal/messageconn"
	"github.com/mirmik/ariadne/internal/quictransport"
	"github.com/mirmik/ariadne/internal/transport"
	"github.com/mirmik/ariadne/internal/wire"
)

const defaultNodePort = "47471"

type configuredRelayTransport struct {
	url        string
	httpClient *http.Client
	dial       func(context.Context) (messageconn.Conn, error)
}

func configureRelayTransport(rawURL, fallbackValue, pin, knownRelaysPath string, acceptNewCertificate bool, logger *slog.Logger) (configuredRelayTransport, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return configuredRelayTransport{}, fmt.Errorf("parse relay URL: %w", err)
	}
	if !strings.EqualFold(parsed.Scheme, "quic") {
		result := configuredRelayTransport{url: rawURL}
		if parsed.Scheme != "https" && parsed.Scheme != "wss" {
			if pin != "" {
				return configuredRelayTransport{}, errors.New("--relay-cert-pin requires a TLS relay URL")
			}
			if acceptNewCertificate {
				return configuredRelayTransport{}, errors.New("--accept-new-relay-certificate requires a TLS relay URL")
			}
			return result, nil
		}
		trust, err := newRelayCertificateTrust(pin, knownRelaysPath, acceptNewCertificate, logger)
		if err != nil {
			return configuredRelayTransport{}, err
		}
		tlsConfig, err := trust.tlsConfig(parsed, "443")
		if err != nil {
			return configuredRelayTransport{}, err
		}
		result.httpClient = tlsHTTPClient(tlsConfig)
		return result, nil
	}

	if parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return configuredRelayTransport{}, errors.New("QUIC relay URL must contain only a host and optional port")
	}
	address := parsed.Host
	if parsed.Port() == "" {
		address = net.JoinHostPort(parsed.Hostname(), defaultNodePort)
	}
	trust, err := newRelayCertificateTrust(pin, knownRelaysPath, acceptNewCertificate, logger)
	if err != nil {
		return configuredRelayTransport{}, err
	}
	tlsConfig, err := trust.tlsConfig(parsed, defaultNodePort)
	if err != nil {
		return configuredRelayTransport{}, err
	}

	fallbackURL, err := resolveFallbackURL(parsed, fallbackValue)
	if err != nil {
		return configuredRelayTransport{}, err
	}
	var fallbackHTTPClient *http.Client
	if fallbackURL != "" {
		if err := transport.ValidateRelayURL(fallbackURL, false); err != nil {
			return configuredRelayTransport{}, fmt.Errorf("relay fallback: %w", err)
		}
		fallbackParsed, err := url.Parse(fallbackURL)
		if err != nil {
			return configuredRelayTransport{}, fmt.Errorf("parse relay fallback URL: %w", err)
		}
		fallbackTLSConfig, err := trust.tlsConfig(fallbackParsed, "443")
		if err != nil {
			return configuredRelayTransport{}, err
		}
		fallbackHTTPClient = tlsHTTPClient(fallbackTLSConfig)
		fallbackURL, err = connector.ConnectorURL(fallbackURL)
		if err != nil {
			return configuredRelayTransport{}, err
		}
	}

	return configuredRelayTransport{
		url: rawURL,
		dial: func(ctx context.Context) (messageconn.Conn, error) {
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
			websocketConnection, _, websocketErr := websocket.Dial(ctx, fallbackURL, &websocket.DialOptions{
				HTTPClient:      fallbackHTTPClient,
				CompressionMode: websocket.CompressionDisabled,
			})
			if websocketErr != nil {
				return nil, errors.Join(quicErr, fmt.Errorf("connect WSS fallback: %w", websocketErr))
			}
			websocketConnection.SetReadLimit(wire.MaxControlMessageSize)
			return messageconn.WebSocket{Conn: websocketConnection}, nil
		},
	}, nil
}

type relayCertificateTrust struct {
	pin                  string
	store                *knownrelay.Store
	acceptNewCertificate bool
	logger               *slog.Logger
	mu                   sync.Mutex
}

func newRelayCertificateTrust(pin, knownRelaysPath string, acceptNewCertificate bool, logger *slog.Logger) (*relayCertificateTrust, error) {
	if pin != "" {
		if acceptNewCertificate {
			return nil, errors.New("--relay-cert-pin and --accept-new-relay-certificate cannot be combined")
		}
		return &relayCertificateTrust{pin: pin, logger: logger}, nil
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
		logger:               logger,
	}, nil
}

func (trust *relayCertificateTrust) tlsConfig(relayURL *url.URL, defaultPort string) (*tls.Config, error) {
	if trust.pin != "" {
		return transport.ClientTLSConfig(relayURL.Hostname(), trust.pin)
	}
	endpoint, err := canonicalRelayEndpoint(relayURL, defaultPort)
	if err != nil {
		return nil, err
	}
	return transport.ClientTLSConfigWithLeafVerifier(relayURL.Hostname(), func(certificateDER []byte) error {
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
	}), nil
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
