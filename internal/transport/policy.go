package transport

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

func ValidateRelayURL(rawURL string, allowPlaintext bool) error {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("parse relay URL: %w", err)
	}
	if parsed.Scheme == "https" || parsed.Scheme == "wss" {
		return nil
	}
	if parsed.Scheme != "http" && parsed.Scheme != "ws" {
		return errors.New("relay URL scheme must be http, https, ws, or wss")
	}
	if allowPlaintext || IsLoopbackHost(parsed.Hostname()) {
		return nil
	}
	return errors.New("plaintext relay URL is allowed only on loopback; use TLS or --allow-insecure-relay")
}

func ValidateListenAddress(address string, tlsEnabled, allowPlaintext bool) error {
	if tlsEnabled || allowPlaintext {
		return nil
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("parse listen address: %w", err)
	}
	if IsLoopbackHost(host) {
		return nil
	}
	return errors.New("plaintext relay may listen only on loopback; configure TLS or --allow-insecure-listen")
}

func IsLoopbackHost(host string) bool {
	host = strings.TrimSpace(strings.Trim(host, "[]"))
	if strings.EqualFold(host, "localhost") {
		return true
	}
	address := net.ParseIP(host)
	return address != nil && address.IsLoopback()
}
