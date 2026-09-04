package relay

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mirmik/ariadne/internal/noderegistry"
	"github.com/mirmik/ariadne/internal/wire"
)

func TestNodeSessionRejectsUnsolicitedMessages(t *testing.T) {
	session := &nodeSession{
		pending: make(map[string]chan wire.ExecResult),
		streams: make(map[string]*relayStream),
	}

	execResult := testEnvelope(t, wire.MessageExecResult, "unknown-request", wire.ExecResult{ExitCode: 0})
	if err := session.handleControl(execResult); err == nil || !strings.Contains(err.Error(), "unsolicited exec result") {
		t.Fatalf("unsolicited exec result error=%v", err)
	}

	streamState := testEnvelope(t, wire.MessageStreamOpened, "", wire.StreamState{StreamID: "00112233445566778899aabbccddeeff"})
	if err := session.handleControl(streamState); err == nil || !strings.Contains(err.Error(), "unknown stream") {
		t.Fatalf("unknown stream state error=%v", err)
	}
}

func TestReportedAliasesRequireManagementClaim(t *testing.T) {
	server, err := New(DefaultConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer server.cancel()
	first := &nodeSession{info: wire.NodeInfo{ID: "node-one", Alias: "phone"}}
	second := &nodeSession{info: wire.NodeInfo{ID: "node-two", Alias: "PHONE"}}
	if _, err := server.register(first, "public-key-one"); err != nil {
		t.Fatal(err)
	}
	if _, err := server.register(second, "public-key-two"); err != nil {
		t.Fatal(err)
	}

	if session := server.lookup("phone"); session != nil {
		t.Fatalf("unclaimed reported alias resolved to session=%v", session)
	}
	if session := server.lookup("node-one"); session != first {
		t.Fatalf("node ID lookup session=%v", session)
	}
	claimed, err := server.claim("node-one", "phone")
	if err != nil {
		t.Fatal(err)
	}
	if !claimed.AliasClaimed || server.lookup("PHONE") != first {
		t.Fatalf("claimed alias did not resolve: %#v", claimed)
	}
	if _, err := server.claim("node-two", "phone"); err == nil {
		t.Fatal("a second node claimed an existing alias")
	}
}

func TestAnonymousNodeCapacity(t *testing.T) {
	config := DefaultConfig()
	config.MaxOnlineNodes = 1
	server, err := New(config, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer server.cancel()
	if _, err := server.register(&nodeSession{info: wire.NodeInfo{ID: "node-one", Alias: "one"}}, "public-key-one"); err != nil {
		t.Fatal(err)
	}
	if _, err := server.register(&nodeSession{info: wire.NodeInfo{ID: "node-two", Alias: "two"}}, "public-key-two"); err == nil {
		t.Fatal("relay accepted an anonymous node above its capacity")
	}
}

func TestPersistentClaimsSurviveRelayRestartAndPreventTakeover(t *testing.T) {
	config := DefaultConfig()
	config.RegistryPath = filepath.Join(t.TempDir(), "node-registry.json")
	firstServer, err := New(config, nil)
	if err != nil {
		t.Fatal(err)
	}
	first := &nodeSession{info: wire.NodeInfo{ID: "node-one", Alias: "temporary", Platform: "linux"}}
	if _, err := firstServer.register(first, "public-key-one"); err != nil {
		t.Fatal(err)
	}
	if _, err := firstServer.claim("node-one", "phone"); err != nil {
		t.Fatal(err)
	}
	firstServer.cancel()

	secondServer, err := New(config, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer secondServer.cancel()
	reconnected := &nodeSession{info: wire.NodeInfo{ID: "node-one", Alias: "new-report", Platform: "linux"}}
	if _, err := secondServer.register(reconnected, "public-key-one"); err != nil {
		t.Fatal(err)
	}
	if info := reconnected.nodeInfo(); !info.AliasClaimed || info.Alias != "phone" {
		t.Fatalf("claim was not restored after restart: %#v", info)
	}
	other := &nodeSession{info: wire.NodeInfo{ID: "node-two", Alias: "phone", Platform: "linux"}}
	if _, err := secondServer.register(other, "public-key-two"); err != nil {
		t.Fatal(err)
	}
	if _, err := secondServer.claim("node-two", "PHONE"); err == nil {
		t.Fatal("offline persistent alias was taken over")
	}
	secondServer.mu.Lock()
	delete(secondServer.byID, "node-one")
	secondServer.mu.Unlock()
	if err := secondServer.revoke("node-one"); err != nil {
		t.Fatal(err)
	}
	if _, err := secondServer.register(&nodeSession{info: wire.NodeInfo{ID: "node-one", Alias: "phone"}}, "public-key-one"); !errors.Is(err, noderegistry.ErrRevoked) {
		t.Fatalf("revoked identity reconnect error=%v", err)
	}
}

func testEnvelope(t *testing.T, messageType wire.MessageType, id string, payload any) wire.Envelope {
	t.Helper()
	encoded, err := wire.MarshalEnvelope(messageType, id, payload)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := wire.DecodeEnvelope(encoded)
	if err != nil {
		t.Fatal(err)
	}
	return envelope
}
