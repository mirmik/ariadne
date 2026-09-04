package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"log"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/mirmik/ariadne/internal/identity"
	"github.com/mirmik/ariadne/internal/knownrelay"
	"github.com/mirmik/ariadne/internal/relay"
	"github.com/mirmik/ariadne/internal/transport"
	"github.com/mirmik/ariadne/internal/wire"
)

func TestProtocolReviewMITMCannotReplacePairedRelay(t *testing.T) {
	for _, tamper := range []bool{false, true} {
		name := "transparent_pake_relay"
		if tamper {
			name = "replace_final_pin"
		}
		t.Run(name, func(t *testing.T) {
			realHTTP := httptest.NewUnstartedServer(http.NotFoundHandler())
			realHTTP.StartTLS()
			defer realHTTP.Close()
			realPin := transport.FormatCertificatePin(realHTTP.TLS.Certificates[0].Certificate[0])
			cfg := relay.DefaultConfig()
			cfg.RelayCertificatePin, cfg.ManagementToken = realPin, "review-token"
			server, err := relay.New(cfg, slog.New(slog.DiscardHandler))
			if err != nil {
				t.Fatal(err)
			}
			defer server.Close()
			realHTTP.Config.Handler = server.NodeHandler()
			request := httptest.NewRequest(http.MethodPost, "/v1/pairing", nil)
			request.Header.Set("Authorization", "Bearer review-token")
			response := httptest.NewRecorder()
			server.ManagementHandler().ServeHTTP(response, request)
			if response.Code != http.StatusCreated {
				t.Fatalf("pairing status %d", response.Code)
			}
			var opening wire.PairingOpenResponse
			if err := json.Unmarshal(response.Body.Bytes(), &opening); err != nil {
				t.Fatal(err)
			}

			public, private, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			template := &x509.Certificate{SerialNumber: big.NewInt(42), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature}
			der, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
			if err != nil {
				t.Fatal(err)
			}
			attackerPin := transport.FormatCertificatePin(der)
			var upgrades atomic.Int32
			proxied := make(chan error, 1)
			attacker := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				err := func() error {
					downstream, err := websocket.Accept(w, r, nil)
					if err != nil {
						return err
					}
					defer downstream.CloseNow()
					upgrades.Add(1)
					ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
					defer cancel()
					upstream, _, err := websocket.Dial(ctx, strings.Replace(realHTTP.URL, "https:", "wss:", 1)+"/v1/connect", &websocket.DialOptions{HTTPClient: realHTTP.Client()})
					if err != nil {
						return err
					}
					defer upstream.CloseNow()
					// Relay KE1/KE2 and KE3/complete. No password or OPAQUE secret
					// is available to this TLS-terminating intermediary.
					for step := range 2 {
						kind, data, err := downstream.Read(ctx)
						if err != nil {
							return err
						}
						if err := upstream.Write(ctx, kind, data); err != nil {
							return err
						}
						kind, data, err = upstream.Read(ctx)
						if err != nil {
							return err
						}
						if tamper && step == 1 {
							envelope, err := wire.DecodeEnvelope(data)
							if err != nil {
								return err
							}
							complete, err := wire.DecodePayload[wire.PairingComplete](envelope)
							if err != nil {
								return err
							}
							complete.RelayCertificatePin = attackerPin
							data, err = wire.MarshalEnvelope(wire.MessagePairComplete, "", complete)
							if err != nil {
								return err
							}
						}
						if err := downstream.Write(ctx, kind, data); err != nil {
							return err
						}
					}
					_, _, _ = downstream.Read(ctx)
					return nil
				}()
				proxied <- err
			}))
			attacker.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: private}}}
			attacker.Config.ErrorLog = log.New(io.Discard, "", 0)
			attacker.StartTLS()
			defer attacker.Close()
			id, err := identity.Generate()
			if err != nil {
				t.Fatal(err)
			}
			storePath := filepath.Join(t.TempDir(), "known_relays")
			configured, err := configureRelayTransport(attacker.URL, "none", "", storePath, false, opening.Code, id, slog.New(slog.DiscardHandler))
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			conn, err := configured.dial(ctx)
			if conn != nil {
				conn.CloseNow()
			}
			if err == nil {
				t.Fatal("attacker endpoint accepted after commissioning")
			}
			if tamper && !strings.Contains(err.Error(), "not authentic") {
				t.Fatalf("expected binding failure, got %v", err)
			}
			if !tamper && !strings.Contains(err.Error(), "pin mismatch") {
				t.Fatalf("expected pinned redial failure, got %v", err)
			}
			select {
			case err := <-proxied:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			store, err := knownrelay.Open(storePath)
			if err != nil {
				t.Fatal(err)
			}
			parsed, _ := url.Parse(attacker.URL)
			endpoint, err := canonicalRelayEndpoint(parsed, "443")
			if err != nil {
				t.Fatal(err)
			}
			pin, found, err := store.Pin(endpoint)
			if err != nil {
				t.Fatal(err)
			}
			if tamper && found {
				t.Fatal("tampered pin persisted")
			}
			if !tamper && (!found || pin != realPin) {
				t.Fatalf("stored pin %q, found %v", pin, found)
			}
			if upgrades.Load() != 1 {
				t.Fatalf("attacker got %d application sessions", upgrades.Load())
			}
		})
	}
}
