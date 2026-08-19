package relay_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/mirmik/ariadne/internal/client"
	"github.com/mirmik/ariadne/internal/connector"
	"github.com/mirmik/ariadne/internal/identity"
	"github.com/mirmik/ariadne/internal/relay"
	"github.com/mirmik/ariadne/internal/wire"
)

func TestRelayConnectorExecAndSSHStream(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	relayConfig := relay.DefaultConfig()
	relayConfig.Token = "test-token"
	relayConfig.PingInterval = 20 * time.Millisecond
	relayConfig.PingTimeout = time.Second
	relayServer := relay.New(relayConfig, logger)
	defer relayServer.Close()
	httpListener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewUnstartedServer(relayServer.Handler())
	httpServer.Listener = httpListener
	httpServer.Start()
	defer httpServer.Close()
	unauthorizedClient, err := client.New(client.Config{RelayURL: httpServer.URL, Token: "wrong-token", HTTPClient: httpServer.Client()})
	if err != nil {
		t.Fatal(err)
	}
	unauthorizedContext, cancelUnauthorized := context.WithTimeout(context.Background(), time.Second)
	_, unauthorizedErr := unauthorizedClient.Nodes(unauthorizedContext)
	cancelUnauthorized()
	if unauthorizedErr == nil {
		t.Fatal("relay accepted an invalid bearer token")
	}

	echoAddress, closeEcho := startEchoServer(t)
	defer closeEcho()
	nodeIdentity, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	nodeConnector, err := connector.New(connector.Config{
		RelayURL:   httpServer.URL,
		Token:      "test-token",
		Alias:      "phone",
		Identity:   nodeIdentity,
		SSHAddress: echoAddress,
		HTTPClient: httpServer.Client(),
		Logger:     logger,
	}, echoExecutor{})
	if err != nil {
		t.Fatal(err)
	}
	connectorContext, cancelConnector := context.WithCancel(context.Background())
	connectorErrors := make(chan error, 1)
	go func() {
		connectorErrors <- nodeConnector.RunOnce(connectorContext)
	}()
	defer func() {
		cancelConnector()
		select {
		case <-connectorErrors:
		case <-time.After(2 * time.Second):
			t.Error("connector did not stop")
		}
	}()

	apiClient, err := client.New(client.Config{
		RelayURL:   httpServer.URL,
		Token:      "test-token",
		HTTPClient: httpServer.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	waitForNode(t, apiClient, "phone")

	execContext, cancelExec := context.WithTimeout(context.Background(), 2*time.Second)
	result, err := apiClient.Exec(execContext, "phone", wire.ExecRequest{
		Argv:          []string{"echo", "hello"},
		TimeoutMillis: 1000,
	})
	cancelExec()
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 || string(result.Stdout) != "echo\x00hello" {
		t.Fatalf("unexpected exec result: %#v", result)
	}

	streamContext, cancelStream := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelStream()
	stream, err := apiClient.DialStream(streamContext, "phone", "ssh")
	if err != nil {
		t.Fatal(err)
	}
	defer stream.CloseNow()
	time.Sleep(50 * time.Millisecond)
	payload := []byte("an opaque SSH packet")
	if err := stream.Write(streamContext, websocket.MessageBinary, payload); err != nil {
		t.Fatal(err)
	}
	messageType, echoed, err := stream.Read(streamContext)
	if err != nil {
		t.Fatal(err)
	}
	if messageType != websocket.MessageBinary || string(echoed) != string(payload) {
		t.Fatalf("unexpected stream reply: type=%v payload=%q", messageType, echoed)
	}
}

type echoExecutor struct{}

func (echoExecutor) Execute(_ context.Context, request wire.ExecRequest) wire.ExecResult {
	return wire.ExecResult{ExitCode: 0, Stdout: []byte(strings.Join(request.Argv, "\x00"))}
}

func waitForNode(t *testing.T, apiClient *client.Client, alias string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		nodes, err := apiClient.Nodes(ctx)
		cancel()
		if err == nil {
			for _, node := range nodes {
				if node.Alias == alias && node.Online {
					return
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("node %q did not come online", alias)
}

func startEchoServer(t *testing.T) (string, func()) {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serverContext, cancel := context.WithCancel(context.Background())
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer connection.Close()
				buffer := make([]byte, 32<<10)
				for {
					count, readErr := connection.Read(buffer)
					if count > 0 {
						if _, writeErr := connection.Write(buffer[:count]); writeErr != nil {
							return
						}
					}
					if readErr != nil {
						return
					}
					select {
					case <-serverContext.Done():
						return
					default:
					}
				}
			}()
		}
	}()
	return listener.Addr().String(), func() {
		cancel()
		if err := listener.Close(); err != nil {
			t.Log(fmt.Sprintf("close echo listener: %v", err))
		}
	}
}
