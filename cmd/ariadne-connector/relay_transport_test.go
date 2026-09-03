package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/mirmik/ariadne/internal/identity"
	"github.com/mirmik/ariadne/internal/knownrelay"
	"github.com/mirmik/ariadne/internal/messageconn"
	"github.com/mirmik/ariadne/internal/relay"
	"github.com/mirmik/ariadne/internal/transport"
	"github.com/mirmik/ariadne/internal/wire"
)

type testMessageConn struct{}

func (*testMessageConn) Read(context.Context) (websocket.MessageType, []byte, error) {
	return 0, nil, errors.New("not implemented")
}

func (*testMessageConn) Write(context.Context, websocket.MessageType, []byte) error {
	return errors.New("not implemented")
}

func (*testMessageConn) Ping(context.Context) error { return errors.New("not implemented") }

func (*testMessageConn) CloseNow() {}

func TestConfigureQUICRelayUsesAutomaticWSSFallback(t *testing.T) {
	configured, err := configureRelayTransport(
		"quic://relay.example:48123",
		"auto",
		"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"",
		false,
		"",
		nil,
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
		"",
		nil,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if configured.url != "quic://95.165.81.109:14771" {
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
		{input: "95.165.81.109", want: "quic://95.165.81.109:14771"},
		{input: "relay.example", want: "quic://relay.example:14771"},
		{input: "relay.example:48123", want: "quic://relay.example:48123"},
		{input: "2001:db8::1", want: "quic://[2001:db8::1]:14771"},
		{input: "[2001:db8::1]", want: "quic://[2001:db8::1]:14771"},
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

func TestQUICRelayEndpointsUseImplicitPortCandidates(t *testing.T) {
	tests := []struct {
		input string
		want  []string
	}{
		{
			input: "relay.example",
			want: []string{
				"quic://relay.example:14771",
				"quic://relay.example:23771",
				"quic://relay.example:47471",
			},
		},
		{
			input: "quic://relay.example",
			want: []string{
				"quic://relay.example:14771",
				"quic://relay.example:23771",
				"quic://relay.example:47471",
			},
		},
		{
			input: "[2001:db8::1]",
			want: []string{
				"quic://[2001:db8::1]:14771",
				"quic://[2001:db8::1]:23771",
				"quic://[2001:db8::1]:47471",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.input, func(t *testing.T) {
			normalized, err := normalizeRelayURL(test.input)
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := url.Parse(normalized)
			if err != nil {
				t.Fatal(err)
			}
			endpoints := quicRelayEndpoints(test.input, parsed)
			got := make([]string, len(endpoints))
			for index, endpoint := range endpoints {
				got[index] = endpoint.String()
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("endpoints=%v, want %v", got, test.want)
			}
		})
	}
}

func TestQUICRelayEndpointsRespectExplicitPort(t *testing.T) {
	for _, input := range []string{"relay.example:48123", "quic://relay.example:48123", "[2001:db8::1]:48123"} {
		t.Run(input, func(t *testing.T) {
			normalized, err := normalizeRelayURL(input)
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := url.Parse(normalized)
			if err != nil {
				t.Fatal(err)
			}
			endpoints := quicRelayEndpoints(input, parsed)
			if len(endpoints) != 1 || endpoints[0].Port() != "48123" {
				t.Fatalf("endpoints=%v", endpoints)
			}
		})
	}
}

func TestDialRelayCandidatesReturnsFirstSuccessAndCancelsOthers(t *testing.T) {
	loserCanceled := make(chan struct{})
	winner := &testMessageConn{}
	got, winnerIndex, err := dialRelayCandidates(context.Background(), []relayDialCandidate{
		{
			endpoint: "relay.example:14771",
			dial: func(ctx context.Context) (messageconn.Conn, error) {
				<-ctx.Done()
				close(loserCanceled)
				return nil, ctx.Err()
			},
		},
		{
			endpoint: "relay.example:23771",
			dial: func(context.Context) (messageconn.Conn, error) {
				return winner, nil
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != winner {
		t.Fatal("candidate race returned the wrong connection")
	}
	if winnerIndex != 1 {
		t.Fatalf("winner index=%d, want 1", winnerIndex)
	}
	select {
	case <-loserCanceled:
	case <-time.After(time.Second):
		t.Fatal("losing candidate was not canceled")
	}
}

func TestDialRelayCandidatesReportsEveryFailure(t *testing.T) {
	_, winnerIndex, err := dialRelayCandidates(context.Background(), []relayDialCandidate{
		{endpoint: "relay.example:14771", dial: func(context.Context) (messageconn.Conn, error) { return nil, errors.New("first") }},
		{endpoint: "relay.example:23771", dial: func(context.Context) (messageconn.Conn, error) { return nil, errors.New("second") }},
	})
	if err == nil || !strings.Contains(err.Error(), "relay.example:14771") || !strings.Contains(err.Error(), "relay.example:23771") {
		t.Fatalf("candidate error=%v", err)
	}
	if winnerIndex != -1 {
		t.Fatalf("winner index=%d, want -1", winnerIndex)
	}
}

func TestStickyRelayCandidateDialerReusesSelectedEndpoint(t *testing.T) {
	var firstCalls atomic.Int32
	var secondCalls atomic.Int32
	firstDone := make(chan struct{})
	winner := &testMessageConn{}
	dial := stickyRelayCandidateDialer([]relayDialCandidate{
		{
			endpoint: "relay.example:14771",
			dial: func(context.Context) (messageconn.Conn, error) {
				firstCalls.Add(1)
				close(firstDone)
				return nil, errors.New("unavailable")
			},
		},
		{
			endpoint: "relay.example:23771",
			dial: func(context.Context) (messageconn.Conn, error) {
				secondCalls.Add(1)
				return winner, nil
			},
		},
	}, nil)

	connection, err := dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if connection != winner {
		t.Fatal("sticky dialer returned the wrong connection")
	}
	select {
	case <-firstDone:
	case <-time.After(time.Second):
		t.Fatal("first discovery candidate did not finish")
	}
	connection, err = dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if connection != winner {
		t.Fatal("sticky dialer returned the wrong connection")
	}
	if firstCalls.Load() != 1 || secondCalls.Load() != 2 {
		t.Fatalf("candidate calls=(%d,%d), want (1,2)", firstCalls.Load(), secondCalls.Load())
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
		"",
		nil,
		nil,
	)
	if err == nil {
		t.Fatal("certificate pin was accepted for plaintext relay")
	}
}

func TestRelayCertificateTrustRequiresPairingAndRejectsChangedCertificate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "known_relays")
	unknownTrust, err := newRelayCertificateTrust("", path, false, "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	relayURL, err := url.Parse("quic://Relay.Example:47471")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := unknownTrust.tlsConfig(relayURL, defaultNodePort); err == nil || !strings.Contains(err.Error(), "not paired") {
		t.Fatalf("unknown relay error=%v", err)
	}
	store, err := knownrelay.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	firstCertificate := []byte("first certificate")
	if _, err := store.TrustPin("relay.example:47471", transport.FormatCertificatePin(firstCertificate), false); err != nil {
		t.Fatal(err)
	}
	trust, err := newRelayCertificateTrust("", path, false, "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	tlsConfig, _, err := trust.tlsConfig(relayURL, defaultNodePort)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyLeaf(tlsConfig, firstCertificate); err != nil {
		t.Fatal(err)
	}
	if err := verifyLeaf(tlsConfig, firstCertificate); err != nil {
		t.Fatal(err)
	}
	secondCertificate := []byte("second certificate")
	err = verifyLeaf(tlsConfig, secondCertificate)
	if err == nil || !strings.Contains(err.Error(), "pin mismatch") {
		t.Fatalf("changed certificate error=%v", err)
	}

	replacementTrust, err := newRelayCertificateTrust("", path, true, "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	replacementTLSConfig, _, err := replacementTrust.tlsConfig(relayURL, defaultNodePort)
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
	trust, err := newRelayCertificateTrust(transport.FormatCertificatePin(certificate), "", false, "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	relayURL, _ := url.Parse("quic://relay.example:47471")
	tlsConfig, _, err := trust.tlsConfig(relayURL, defaultNodePort)
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

func TestPairingCodeIsUsedAtMostOncePerEndpoint(t *testing.T) {
	nodeIdentity, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	trust, err := newRelayCertificateTrust("", filepath.Join(t.TempDir(), "known_relays"), false, "12345678", nodeIdentity, nil)
	if err != nil {
		t.Fatal(err)
	}
	trust.setPairingAttemptLimit(2)
	code, gotIdentity, err := trust.beginPairingAttempt("relay.example:14771")
	if err != nil {
		t.Fatal(err)
	}
	if code != "12345678" || gotIdentity != nodeIdentity {
		t.Fatalf("first pairing attempt=(%q,%p), want code and connector identity", code, gotIdentity)
	}
	if _, _, err := trust.beginPairingAttempt("relay.example:14771"); err == nil || !strings.Contains(err.Error(), "already attempted") {
		t.Fatalf("repeated endpoint pairing error=%v", err)
	}
	if _, _, err := trust.beginPairingAttempt("relay.example:23771"); err != nil {
		t.Fatalf("second endpoint pairing attempt failed: %v", err)
	}
	if trust.pairingEnabled() {
		t.Fatal("pairing code remained available after the local attempt budget was exhausted")
	}
	if _, _, err := trust.beginPairingAttempt("relay.example:47471"); err == nil || !strings.Contains(err.Error(), "no longer available") {
		t.Fatalf("exhausted pairing code error=%v", err)
	}
}

func TestWSSPairingStoresAuthenticatedPinAndRedials(t *testing.T) {
	certificateSource := httptest.NewTLSServer(http.NotFoundHandler())
	certificate := certificateSource.TLS.Certificates[0]
	certificateSource.Close()
	relayPin := transport.FormatCertificatePin(certificate.Certificate[0])
	relayConfig := relay.DefaultConfig()
	relayConfig.RegistryPath = filepath.Join(t.TempDir(), "registry.json")
	relayConfig.ManagementToken = "test-token"
	relayConfig.RelayCertificatePin = relayPin
	relayServer, err := relay.New(relayConfig, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer relayServer.Close()

	request := httptest.NewRequest(http.MethodPost, "/v1/pairing", nil)
	request.Header.Set("Authorization", "Bearer test-token")
	response := httptest.NewRecorder()
	relayServer.ManagementHandler().ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("open pairing status=%d body=%s", response.Code, response.Body.String())
	}
	var opening wire.PairingOpenResponse
	if err := json.NewDecoder(response.Body).Decode(&opening); err != nil {
		t.Fatal(err)
	}

	nodeServer := httptest.NewUnstartedServer(relayServer.NodeHandler())
	nodeServer.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}}
	nodeServer.StartTLS()
	defer nodeServer.Close()
	knownRelaysPath := filepath.Join(t.TempDir(), "known_relays")
	nodeIdentity, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	configured, err := configureRelayTransport(nodeServer.URL, "none", "", knownRelaysPath, false, opening.Code, nodeIdentity, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	connection, err := configured.dial(ctx)
	if err != nil {
		t.Fatal(err)
	}
	connection.CloseNow()

	parsed, err := url.Parse(nodeServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := canonicalRelayEndpoint(parsed, "443")
	if err != nil {
		t.Fatal(err)
	}
	store, err := knownrelay.Open(knownRelaysPath)
	if err != nil {
		t.Fatal(err)
	}
	storedPin, found, err := store.Pin(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	if !found || storedPin != relayPin {
		t.Fatalf("stored pin=(%q,%v), want (%q,true)", storedPin, found, relayPin)
	}
}
