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
	"time"

	"github.com/coder/websocket"
	"github.com/mirmik/ariadne/internal/connector"
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

func configureRelayTransport(rawURL, fallbackValue, pin string, logger *slog.Logger) (configuredRelayTransport, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return configuredRelayTransport{}, fmt.Errorf("parse relay URL: %w", err)
	}
	if !strings.EqualFold(parsed.Scheme, "quic") {
		result := configuredRelayTransport{url: rawURL}
		if pin == "" {
			return result, nil
		}
		if parsed.Scheme != "https" && parsed.Scheme != "wss" {
			return configuredRelayTransport{}, errors.New("--relay-cert-pin requires a TLS relay URL")
		}
		tlsConfig, err := transport.ClientTLSConfig(parsed.Hostname(), pin)
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
	tlsConfig, err := transport.ClientTLSConfig(parsed.Hostname(), pin)
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
		fallbackTLSConfig, err := transport.ClientTLSConfig(fallbackParsed.Hostname(), pin)
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
