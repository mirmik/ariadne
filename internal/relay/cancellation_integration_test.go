package relay

import (
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/mirmik/ariadne/internal/connector"
	"github.com/mirmik/ariadne/internal/identity"
	"github.com/mirmik/ariadne/internal/messageconn"
	"github.com/mirmik/ariadne/internal/wire"
	"golang.org/x/crypto/ssh"
)

func TestRevokeDisconnectsWithoutCancelingBackgroundJob(t *testing.T) {
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("requires POSIX shell")
	}
	server, err := New(DefaultConfig(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	id, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	left, right := net.Pipe()
	serverConn := messageconn.NewFramed(left, wire.MaxControlMessageSize, func() { _ = left.Close() })
	clientConn := messageconn.NewFramed(right, wire.MaxControlMessageSize, func() { _ = right.Close() })
	defer clientConn.CloseNow()
	node, err := connector.New(connector.Config{Identity: id, Alias: "node", Logger: slog.New(slog.DiscardHandler), Dial: func(context.Context) (messageconn.Conn, error) { return clientConn, nil }}, nil)
	if err != nil {
		t.Fatal(err)
	}
	go server.ServeNodeConnection(serverConn)
	connectorDone := make(chan error, 1)
	go func() { connectorDone <- node.RunOnce(ctx) }()
	var session *nodeSession
	for session == nil {
		session = server.lookup(id.NodeID())
		if ctx.Err() != nil {
			t.Fatal(ctx.Err())
		}
		if session == nil {
			time.Sleep(time.Millisecond)
		}
	}
	dir := t.TempDir()
	trigger, finished := filepath.Join(dir, "continue"), filepath.Join(dir, "finished")
	response, err := session.job(ctx, wire.JobRequest{Action: wire.JobActionStart, Exec: &wire.ExecRequest{Argv: []string{shell, "-c", `while [ ! -f "$1" ]; do sleep 0.01; done; printf done > "$2"`, "review-job", trigger, finished}, TimeoutMillis: 3000}})
	if err != nil || response.Job == nil {
		t.Fatalf("start job: %#v %v", response, err)
	}
	if err := server.revoke(id.NodeID()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-connectorDone:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if server.lookup(id.NodeID()) != nil {
		t.Fatal("revoked node stayed online")
	}
	if err := os.WriteFile(trigger, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for {
		if data, err := os.ReadFile(finished); err == nil && string(data) == "done" {
			break
		}
		if ctx.Err() != nil {
			t.Fatal("revocation canceled the background job")
		}
		time.Sleep(time.Millisecond)
	}
}

type delayedCancellationExecutor struct {
	started chan struct{}
	release chan struct{}
}

func (executor *delayedCancellationExecutor) Execute(ctx context.Context, request wire.ExecRequest) wire.ExecResult {
	if request.Argv[0] != "slow" {
		return wire.ExecResult{ExitCode: 0}
	}
	close(executor.started)
	<-ctx.Done()
	<-executor.release
	return wire.ExecResult{ExitCode: -1, Error: "delayed cancellation result"}
}

type cancellationObserver struct {
	messageconn.Conn
	seen chan struct{}
	once sync.Once
}

func (conn *cancellationObserver) Read(ctx context.Context) (websocket.MessageType, []byte, error) {
	kind, data, err := conn.Conn.Read(ctx)
	if err == nil && kind == websocket.MessageText {
		if envelope, decodeErr := wire.DecodeEnvelope(data); decodeErr == nil && envelope.Type == wire.MessageExecResult {
			result, _ := wire.DecodePayload[wire.ExecResult](envelope)
			if result.Error == "delayed cancellation result" {
				conn.once.Do(func() { close(conn.seen) })
			}
		}
	}
	return kind, data, err
}

func TestCanceledExecPreservesSiblingStream(t *testing.T) {
	server, err := New(DefaultConfig(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	id, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	left, right := net.Pipe()
	serverConn := &cancellationObserver{Conn: messageconn.NewFramed(left, wire.MaxControlMessageSize, func() { _ = left.Close() }), seen: make(chan struct{})}
	clientConn := messageconn.NewFramed(right, wire.MaxControlMessageSize, func() { _ = right.Close() })
	defer clientConn.CloseNow()
	executor := &delayedCancellationExecutor{started: make(chan struct{}), release: make(chan struct{})}
	var release sync.Once
	defer release.Do(func() { close(executor.release) })
	node, err := connector.New(connector.Config{Identity: id, Alias: "node", Logger: slog.New(slog.DiscardHandler), Dial: func(context.Context) (messageconn.Conn, error) { return clientConn, nil }}, executor)
	if err != nil {
		t.Fatal(err)
	}
	go server.ServeNodeConnection(serverConn)
	go node.RunOnce(ctx)
	var session *nodeSession
	for session == nil {
		session = server.lookup(id.NodeID())
		if ctx.Err() != nil {
			t.Fatal(ctx.Err())
		}
		if session == nil {
			time.Sleep(time.Millisecond)
		}
	}
	streamID, err := randomID()
	if err != nil {
		t.Fatal(err)
	}
	stream := newRelayStream(streamID, session)
	if err := session.addStream(stream); err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(id.SSHHostSigner())
	if err != nil {
		t.Fatal(err)
	}
	if err := session.sendControl(wire.MessageStreamOpen, "", wire.StreamOpen{StreamID: streamID, Protocol: "shell", SSHClientPublicKey: base64.RawStdEncoding.EncodeToString(signer.PublicKey().Marshal())}); err != nil {
		t.Fatal(err)
	}
	if err := stream.waitUntilOpened(ctx); err != nil {
		t.Fatal(err)
	}
	execCtx, cancelExec := context.WithCancel(ctx)
	defer cancelExec()
	execDone := make(chan error, 1)
	go func() {
		_, err := session.exec(execCtx, wire.ExecRequest{Argv: []string{"slow"}, TimeoutMillis: 10000})
		execDone <- err
	}()
	select {
	case <-executor.started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	cancelExec()
	select {
	case err := <-execDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// exec has returned and retired its pending ID before the connector responds.
	release.Do(func() { close(executor.release) })
	select {
	case <-serverConn.seen:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if _, err := session.exec(ctx, wire.ExecRequest{Argv: []string{"probe"}, TimeoutMillis: 10000}); err != nil {
		t.Fatalf("late result disconnected node: %v", err)
	}
	select {
	case <-stream.done:
		t.Fatal("late exec result closed sibling stream")
	default:
	}
}
