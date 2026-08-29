package main

import (
	"crypto/tls"
	"crypto/x509"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mirmik/ariadne/internal/transport"
)

func TestConfigureQUICRelayUsesAutomaticWSSFallback(t *testing.T) {
	configured, err := configureRelayTransport(
		"quic://relay.example:48123",
		"auto",
		"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"",
		false,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if configured.dial == nil {
		t.Fatal("QUIC relay did not configure a custom dialer")
	}
	if configured.url != "quic://relay.example:48123" {
		t.Fatalf("url=%q", configured.url)
	}
}

func TestConfigureBareRelayUsesQUICDefaults(t *testing.T) {
	configured, err := configureRelayTransport(
		"95.165.81.109",
		"none",
		"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"",
		false,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if configured.url != "quic://95.165.81.109:47471" {
		t.Fatalf("normalized relay URL=%q", configured.url)
	}
	if configured.dial == nil {
		t.Fatal("bare relay did not configure QUIC")
	}
}

func TestNormalizeRelayURL(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{input: "95.165.81.109", want: "quic://95.165.81.109:47471"},
		{input: "relay.example", want: "quic://relay.example:47471"},
		{input: "relay.example:48123", want: "quic://relay.example:48123"},
		{input: "2001:db8::1", want: "quic://[2001:db8::1]:47471"},
		{input: "[2001:db8::1]", want: "quic://[2001:db8::1]:47471"},
		{input: "[2001:db8::1]:48123", want: "quic://[2001:db8::1]:48123"},
		{input: "https://relay.example:47471", want: "https://relay.example:47471"},
	}
	for _, test := range tests {
		t.Run(test.input, func(t *testing.T) {
			got, err := normalizeRelayURL(test.input)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("normalizeRelayURL(%q)=%q, want %q", test.input, got, test.want)
			}
		})
	}
}

func TestNormalizeRelayURLRejectsAmbiguousBareAddress(t *testing.T) {
	for _, value := range []string{"", "relay.example/path", "relay.example:", "user@relay.example"} {
		if _, err := normalizeRelayURL(value); err == nil {
			t.Errorf("normalizeRelayURL(%q) succeeded", value)
		}
	}
}

func TestResolveFallbackURL(t *testing.T) {
	relay, err := url.Parse("quic://relay.example:48123")
	if err != nil {
		t.Fatal(err)
	}
	automatic, err := resolveFallbackURL(relay, "auto")
	if err != nil {
		t.Fatal(err)
	}
	if automatic != "https://relay.example:48123" {
		t.Fatalf("automatic fallback=%q", automatic)
	}
	disabled, err := resolveFallbackURL(relay, "none")
	if err != nil {
		t.Fatal(err)
	}
	if disabled != "" {
		t.Fatalf("disabled fallback=%q", disabled)
	}
}

func TestConfigureRelayRejectsPinOnPlaintext(t *testing.T) {
	_, err := configureRelayTransport(
		"http://127.0.0.1:47471",
		"auto",
		"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"",
		false,
		nil,
	)
	if err == nil {
		t.Fatal("certificate pin was accepted for plaintext relay")
	}
}

func TestRelayCertificateTrustUsesTOFUAndRejectsChangedCertificate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "known_relays")
	trust, err := newRelayCertificateTrust("", path, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	relayURL, err := url.Parse("quic://Relay.Example:47471")
	if err != nil {
		t.Fatal(err)
	}
	tlsConfig, err := trust.tlsConfig(relayURL, defaultNodePort)
	if err != nil {
		t.Fatal(err)
	}
	firstCertificate := []byte("first certificate")
	if err := verifyLeaf(tlsConfig, firstCertificate); err != nil {
		t.Fatal(err)
	}
	if err := verifyLeaf(tlsConfig, firstCertificate); err != nil {
		t.Fatal(err)
	}
	secondCertificate := []byte("second certificate")
	err = verifyLeaf(tlsConfig, secondCertificate)
	if err == nil || !strings.Contains(err.Error(), "--accept-new-relay-certificate") {
		t.Fatalf("changed certificate error=%v", err)
	}

	replacementTrust, err := newRelayCertificateTrust("", path, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	replacementTLSConfig, err := replacementTrust.tlsConfig(relayURL, defaultNodePort)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyLeaf(replacementTLSConfig, secondCertificate); err != nil {
		t.Fatal(err)
	}
	if err := verifyLeaf(replacementTLSConfig, []byte("third certificate")); err == nil || !strings.Contains(err.Error(), "--accept-new-relay-certificate") {
		t.Fatalf("one-shot replacement approval accepted another certificate: %v", err)
	}
}

func TestCanonicalRelayEndpointSharesQUICAndWSSIdentity(t *testing.T) {
	quicURL, _ := url.Parse("quic://Relay.Example:47471")
	wssURL, _ := url.Parse("https://relay.example:47471")
	quicEndpoint, err := canonicalRelayEndpoint(quicURL, defaultNodePort)
	if err != nil {
		t.Fatal(err)
	}
	wssEndpoint, err := canonicalRelayEndpoint(wssURL, "443")
	if err != nil {
		t.Fatal(err)
	}
	if quicEndpoint != wssEndpoint || quicEndpoint != "relay.example:47471" {
		t.Fatalf("QUIC endpoint=%q WSS endpoint=%q", quicEndpoint, wssEndpoint)
	}
}

func verifyLeaf(config *tls.Config, certificateDER []byte) error {
	return config.VerifyConnection(tls.ConnectionState{PeerCertificates: []*x509.Certificate{{Raw: certificateDER}}})
}

func TestExplicitPinStillUsesExactCertificate(t *testing.T) {
	certificate := []byte("relay certificate")
	trust, err := newRelayCertificateTrust(transport.FormatCertificatePin(certificate), "", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	relayURL, _ := url.Parse("quic://relay.example:47471")
	tlsConfig, err := trust.tlsConfig(relayURL, defaultNodePort)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyLeaf(tlsConfig, certificate); err != nil {
		t.Fatal(err)
	}
	if err := verifyLeaf(tlsConfig, []byte("wrong")); err == nil {
		t.Fatal("explicit pin accepted the wrong certificate")
	}
}
