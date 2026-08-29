package main

import (
	"net/url"
	"testing"
)

func TestConfigureQUICRelayUsesAutomaticWSSFallback(t *testing.T) {
	configured, err := configureRelayTransport(
		"quic://relay.example:48123",
		"auto",
		"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
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
		nil,
	)
	if err == nil {
		t.Fatal("certificate pin was accepted for plaintext relay")
	}
}
