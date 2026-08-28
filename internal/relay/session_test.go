package relay

import (
	"strings"
	"testing"

	"github.com/mirmik/ariadne/internal/wire"
)

func TestNodeSessionRejectsUnsolicitedMessages(t *testing.T) {
	session := &nodeSession{
		pending: make(map[string]chan wire.ExecResult),
		streams: make(map[string]*relayStream),
		known:   make(map[string]struct{}),
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
	server := New(DefaultConfig(), nil)
	defer server.cancel()
	first := &nodeSession{info: wire.NodeInfo{ID: "node-one", Alias: "phone"}}
	second := &nodeSession{info: wire.NodeInfo{ID: "node-two", Alias: "PHONE"}}
	if _, err := server.register(first); err != nil {
		t.Fatal(err)
	}
	if _, err := server.register(second); err != nil {
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
	server := New(config, nil)
	defer server.cancel()
	if _, err := server.register(&nodeSession{info: wire.NodeInfo{ID: "node-one", Alias: "one"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := server.register(&nodeSession{info: wire.NodeInfo{ID: "node-two", Alias: "two"}}); err == nil {
		t.Fatal("relay accepted an anonymous node above its capacity")
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
