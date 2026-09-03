package relay

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/mirmik/ariadne/internal/identity"
	"github.com/mirmik/ariadne/internal/messageconn"
	"github.com/mirmik/ariadne/internal/pairing"
	"github.com/mirmik/ariadne/internal/wire"
)

const relayPairingTestPin = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestServeConnectorCommissionsRelayIdentity(t *testing.T) {
	config := DefaultConfig()
	config.RegistryPath = filepath.Join(t.TempDir(), "registry.json")
	config.RelayCertificatePin = relayPairingTestPin
	server, err := New(config, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	opening, err := server.pairing.Open(time.Now(), time.Minute, 3)
	if err != nil {
		t.Fatal(err)
	}

	serverSocket, clientSocket := net.Pipe()
	serverConnection := messageconn.NewFramed(serverSocket, wire.MaxControlMessageSize, func() { _ = serverSocket.Close() })
	clientConnection := messageconn.NewFramed(clientSocket, wire.MaxControlMessageSize, func() { _ = clientSocket.Close() })
	defer clientConnection.CloseNow()
	go server.ServeNodeConnection(serverConnection)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	nodeIdentity, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	pin, err := pairing.PairRelay(ctx, clientConnection, opening.Code, nodeIdentity)
	if err != nil {
		t.Fatal(err)
	}
	if pin != relayPairingTestPin {
		t.Fatalf("paired pin=%q, want %q", pin, relayPairingTestPin)
	}
}

func TestManagementOpensPairingWithoutExposingCodeOnNodePlane(t *testing.T) {
	config := DefaultConfig()
	config.RegistryPath = filepath.Join(t.TempDir(), "registry.json")
	config.ManagementToken = "test-token"
	config.RelayCertificatePin = relayPairingTestPin
	server, err := New(config, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	request := httptest.NewRequest(http.MethodPost, "/v1/pairing", nil)
	request.Header.Set("Authorization", "Bearer test-token")
	response := httptest.NewRecorder()
	server.ManagementHandler().ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("management pairing status=%d body=%s", response.Code, response.Body.String())
	}
	var opening wire.PairingOpenResponse
	if err := json.NewDecoder(response.Body).Decode(&opening); err != nil {
		t.Fatal(err)
	}
	if len(opening.Code) != 8 || opening.RemainingAttempts != pairing.DefaultAttempts {
		t.Fatalf("unexpected pairing opening: %#v", opening)
	}

	nodeRequest := httptest.NewRequest(http.MethodPost, "/v1/pairing", nil)
	nodeResponse := httptest.NewRecorder()
	server.NodeHandler().ServeHTTP(nodeResponse, nodeRequest)
	if nodeResponse.Code != http.StatusNotFound {
		t.Fatalf("node plane exposed pairing opener: status=%d", nodeResponse.Code)
	}
}
