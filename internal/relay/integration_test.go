package relay_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/mirmik/ariadne/internal/client"
	"github.com/mirmik/ariadne/internal/connector"
	"github.com/mirmik/ariadne/internal/identity"
	"github.com/mirmik/ariadne/internal/relay"
	"github.com/mirmik/ariadne/internal/wire"
	"golang.org/x/crypto/ssh"
)

func TestRelayConnectorExecAndSSHStream(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	relayConfig := relay.DefaultConfig()
	relayConfig.PingInterval = 20 * time.Millisecond
	relayConfig.PingTimeout = time.Second
	relayConfig.ManagementToken = testManagementToken
	relayServer := relay.New(relayConfig, logger)
	defer relayServer.Close()
	nodeServer := startHTTPTestServer(t, relayServer.NodeHandler())
	defer nodeServer.Close()
	managementServer := startHTTPTestServer(t, relayServer.ManagementHandler())
	defer managementServer.Close()
	assertPlaneIsolation(t, nodeServer, managementServer)

	echoAddress, closeEcho := startEchoServer(t)
	defer closeEcho()
	nodeIdentity, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	nodeConnector, err := connector.New(connector.Config{
		RelayURL:   nodeServer.URL,
		Alias:      "phone",
		Identity:   nodeIdentity,
		SSHAddress: echoAddress,
		Shell:      "/bin/sh",
		ShellEnvironment: []string{
			"HOME=" + t.TempDir(),
			"PATH=/usr/bin:/bin",
			"SHELL=/bin/sh",
			"ARIADNE_PRIVATE=must-not-leak",
		},
		HTTPClient: nodeServer.Client(),
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
		RelayURL:        managementServer.URL,
		ManagementToken: testManagementToken,
		HTTPClient:      managementServer.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	node := waitForNode(t, apiClient, "phone")
	claimed, err := apiClient.Claim(context.Background(), node.ID, "phone")
	if err != nil {
		t.Fatal(err)
	}
	if !claimed.AliasClaimed || claimed.Alias != "phone" {
		t.Fatalf("unexpected claim result: %#v", claimed)
	}

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

	authorizedSigner := newSSHSigner(t)
	wrongSigner := newSSHSigner(t)
	mismatchContext, cancelMismatch := context.WithTimeout(context.Background(), 3*time.Second)
	_, _, mismatchCleanup, mismatchErr := dialEmbeddedSSH(mismatchContext, apiClient, "phone", authorizedSigner, wrongSigner)
	cancelMismatch()
	mismatchCleanup()
	if mismatchErr == nil {
		t.Fatal("embedded SSH stream accepted a different one-time session key")
	}

	shellContext, cancelShell := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelShell()
	sshClient, peer, shellCleanup, err := dialEmbeddedSSH(shellContext, apiClient, "phone", authorizedSigner, authorizedSigner)
	if err != nil {
		t.Fatal(err)
	}
	defer shellCleanup()
	expectedHostSigner, err := ssh.NewSignerFromKey(nodeIdentity.SSHHostSigner())
	if err != nil {
		t.Fatal(err)
	}
	expectedHostKey := base64.RawStdEncoding.EncodeToString(expectedHostSigner.PublicKey().Marshal())
	if peer.NodeID != nodeIdentity.NodeID() || peer.SSHHostKey != expectedHostKey {
		t.Fatalf("shell peer is not bound to connector identity: %#v", peer)
	}

	shellSession, err := sshClient.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer shellSession.Close()
	stdinReader, stdinWriter := io.Pipe()
	defer stdinWriter.Close()
	output := &synchronizedBuffer{}
	shellSession.Stdin = stdinReader
	shellSession.Stdout = output
	shellSession.Stderr = output
	if err := shellSession.RequestPty("xterm-256color", 24, 80, ssh.TerminalModes{ssh.ECHO: 1}); err != nil {
		t.Fatal(err)
	}
	if err := shellSession.Shell(); err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(stdinWriter, "stty size\nprintf 'PRIVATE=<%s>\\n' \"$ARIADNE_PRIVATE\"\n")
	waitForOutput(t, output, "24 80")
	if err := shellSession.WindowChange(40, 100); err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(stdinWriter, "stty size\nexit 7\n")
	waitErr := shellSession.Wait()
	var exitError *ssh.ExitError
	if !errors.As(waitErr, &exitError) || exitError.ExitStatus() != 7 {
		t.Fatalf("unexpected embedded shell exit: %v", waitErr)
	}
	if !strings.Contains(output.String(), "40 100") {
		t.Fatalf("PTY resize was not observed; output=%q", output.String())
	}
	if strings.Contains(output.String(), "must-not-leak") || !strings.Contains(output.String(), "PRIVATE=<>") {
		t.Fatalf("connector private environment leaked into shell; output=%q", output.String())
	}

	nonPTYSigner := newSSHSigner(t)
	nonPTYClient, _, nonPTYCleanup, err := dialEmbeddedSSH(shellContext, apiClient, "phone", nonPTYSigner, nonPTYSigner)
	if err != nil {
		t.Fatal(err)
	}
	defer nonPTYCleanup()
	nonPTYSession, err := nonPTYClient.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer nonPTYSession.Close()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	nonPTYSession.Stdin = strings.NewReader("printf 'plain-stdout'; printf 'plain-stderr' >&2; exit 3\n")
	nonPTYSession.Stdout = &stdout
	nonPTYSession.Stderr = &stderr
	if err := nonPTYSession.Shell(); err != nil {
		t.Fatal(err)
	}
	waitErr = nonPTYSession.Wait()
	if !errors.As(waitErr, &exitError) || exitError.ExitStatus() != 3 {
		t.Fatalf("unexpected non-PTY shell exit: %v", waitErr)
	}
	if stdout.String() != "plain-stdout" || stderr.String() != "plain-stderr" {
		t.Fatalf("non-PTY streams changed: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func startHTTPTestServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.Listener = listener
	server.Start()
	return server
}

func assertPlaneIsolation(t *testing.T, nodeServer, managementServer *httptest.Server) {
	t.Helper()
	response, err := nodeServer.Client().Get(nodeServer.URL + "/v1/nodes")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("node plane exposed management API: status=%d", response.StatusCode)
	}
	response, err = managementServer.Client().Get(managementServer.URL + "/v1/nodes")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("management plane accepted an unauthenticated request: status=%d", response.StatusCode)
	}
	request, err := http.NewRequest(http.MethodGet, managementServer.URL+"/v1/connect", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+testManagementToken)
	response, err = managementServer.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("management plane exposed connector endpoint: status=%d", response.StatusCode)
	}
}

const testManagementToken = "QkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkI"

type echoExecutor struct{}

func (echoExecutor) Execute(_ context.Context, request wire.ExecRequest) wire.ExecResult {
	return wire.ExecResult{ExitCode: 0, Stdout: []byte(strings.Join(request.Argv, "\x00"))}
}

func waitForNode(t *testing.T, apiClient *client.Client, alias string) wire.NodeInfo {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		nodes, err := apiClient.Nodes(ctx)
		cancel()
		if err == nil {
			for _, node := range nodes {
				if node.Alias == alias && node.Online {
					return node
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("node %q did not come online", alias)
	return wire.NodeInfo{}
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

func newSSHSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

func dialEmbeddedSSH(ctx context.Context, apiClient *client.Client, target string, advertisedSigner, authenticationSigner ssh.Signer) (*ssh.Client, client.StreamPeer, func(), error) {
	encodedClientKey := base64.RawStdEncoding.EncodeToString(advertisedSigner.PublicKey().Marshal())
	websocketConnection, peer, err := apiClient.DialShellStream(ctx, target, encodedClientKey)
	if err != nil {
		return nil, client.StreamPeer{}, func() {}, err
	}
	networkConnection := websocket.NetConn(ctx, websocketConnection, websocket.MessageBinary)
	cleanup := func() {
		_ = networkConnection.Close()
		websocketConnection.CloseNow()
	}
	rawHostKey, err := base64.RawStdEncoding.DecodeString(peer.SSHHostKey)
	if err != nil {
		cleanup()
		return nil, peer, func() {}, err
	}
	hostKey, err := ssh.ParsePublicKey(rawHostKey)
	if err != nil {
		cleanup()
		return nil, peer, func() {}, err
	}
	connection, channels, requests, err := ssh.NewClientConn(networkConnection, peer.NodeID, &ssh.ClientConfig{
		User:            "ariadne",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(authenticationSigner)},
		HostKeyCallback: ssh.FixedHostKey(hostKey),
	})
	if err != nil {
		cleanup()
		return nil, peer, func() {}, err
	}
	sshClient := ssh.NewClient(connection, channels, requests)
	return sshClient, peer, func() {
		_ = sshClient.Close()
		cleanup()
	}, nil
}

type synchronizedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (buffer *synchronizedBuffer) Write(data []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.Write(data)
}

func (buffer *synchronizedBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.String()
}

func waitForOutput(t *testing.T, output *synchronizedBuffer, expected string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(output.String(), expected) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("shell output did not contain %q; output=%q", expected, output.String())
}
