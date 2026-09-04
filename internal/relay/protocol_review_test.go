package relay

import (
	"context"
	"crypto/ed25519"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/mirmik/ariadne/internal/connector"
	"github.com/mirmik/ariadne/internal/identity"
	"github.com/mirmik/ariadne/internal/messageconn"
	"github.com/mirmik/ariadne/internal/noderegistry"
	"github.com/mirmik/ariadne/internal/wire"
)

func TestProtocolReviewClaimedAliasReconnect(t *testing.T) {
	server, err := New(DefaultConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	id, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.registry.Observe(noderegistry.Observation{NodeID: id.NodeID(), PublicKey: id.EncodedPublicKey(), ReportedAlias: "original"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := server.registry.Claim(id.NodeID(), "grandma", time.Now()); err != nil {
		t.Fatal(err)
	}
	left, right := net.Pipe()
	serverConn := messageconn.NewFramed(left, wire.MaxControlMessageSize, func() { _ = left.Close() })
	clientConn := messageconn.NewFramed(right, wire.MaxControlMessageSize, func() { _ = right.Close() })
	defer clientConn.CloseNow()
	node, err := connector.New(connector.Config{Identity: id, Alias: "original", Dial: func(context.Context) (messageconn.Conn, error) { return clientConn, nil }}, nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); server.ServeNodeConnection(serverConn) }()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	err = node.RunOnce(ctx)
	clientConn.CloseNow()
	<-done
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("claimed alias should keep node connected until test deadline, got %v", err)
	}
}

type reviewConnection struct {
	mu       sync.Mutex
	messages []wire.Envelope
}

func (*reviewConnection) Read(context.Context) (websocket.MessageType, []byte, error) {
	return 0, nil, errors.New("unused")
}
func (conn *reviewConnection) Write(_ context.Context, _ websocket.MessageType, data []byte) error {
	envelope, err := wire.DecodeEnvelope(data)
	if err != nil {
		return err
	}
	conn.mu.Lock()
	conn.messages = append(conn.messages, envelope)
	conn.mu.Unlock()
	return nil
}
func (*reviewConnection) Ping(context.Context) error { return nil }
func (*reviewConnection) CloseNow()                  {}

func TestProtocolReviewLateResponsesDoNotDisconnectNode(t *testing.T) {
	for _, action := range []string{"exec", "job"} {
		t.Run(action, func(t *testing.T) {
			conn := &reviewConnection{}
			session := newNodeSession(nil, conn, wire.NodeInfo{})
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if action == "exec" {
				_, err := session.exec(ctx, wire.ExecRequest{Argv: []string{"true"}})
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("exec error = %v", err)
				}
			} else {
				_, err := session.job(ctx, wire.JobRequest{Action: wire.JobActionList})
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("job error = %v", err)
				}
			}
			conn.mu.Lock()
			requestID := conn.messages[0].ID
			conn.mu.Unlock()
			kind := wire.MessageExecResult
			var payload any = wire.ExecResult{ExitCode: -1, Error: "command canceled"}
			if action == "job" {
				kind, payload = wire.MessageJobResponse, wire.JobResponse{}
			}
			if err := session.handleControl(testEnvelope(t, kind, requestID, payload)); err != nil {
				t.Fatalf("legitimate late response becomes fatal protocol error: %v", err)
			}
		})
	}
}

func TestProtocolReviewRegistrationTranscriptTampering(t *testing.T) {
	id, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, 32)
	hello := wire.Hello{NodeID: id.NodeID(), PublicKey: id.EncodedPublicKey(), Alias: "node", SSHHostKey: "host-key", Platform: "linux", Architecture: "amd64", ConnectorVersion: "review", Capabilities: []string{wire.CapabilityFileTransfer}}
	signature, err := identity.ParseSignature(id.Sign(wire.RegistrationTranscript(nonce, hello)))
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"nonce", "node_id", "alias", "public_key", "ssh_host_key", "platform", "architecture", "version", "capabilities"} {
		t.Run(field, func(t *testing.T) {
			changed := hello
			challenge := append([]byte(nil), nonce...)
			switch field {
			case "nonce":
				challenge[0]++
			case "node_id":
				changed.NodeID += "x"
			case "alias":
				changed.Alias += "x"
			case "public_key":
				changed.PublicKey += "x"
			case "ssh_host_key":
				changed.SSHHostKey += "x"
			case "platform":
				changed.Platform += "x"
			case "architecture":
				changed.Architecture += "x"
			case "version":
				changed.ConnectorVersion += "x"
			case "capabilities":
				changed.Capabilities = []string{wire.CapabilityBackgroundJobs}
			}
			if ed25519.Verify(id.PublicKey(), wire.RegistrationTranscript(challenge, changed), signature) {
				t.Fatalf("signature still valid after modifying %s", field)
			}
		})
	}
}
