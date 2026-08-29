package relay_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/mirmik/ariadne/internal/client"
	"github.com/mirmik/ariadne/internal/connector"
	"github.com/mirmik/ariadne/internal/identity"
	"github.com/mirmik/ariadne/internal/messageconn"
	"github.com/mirmik/ariadne/internal/quictransport"
	"github.com/mirmik/ariadne/internal/relay"
	"github.com/mirmik/ariadne/internal/transport"
	"github.com/mirmik/ariadne/internal/wire"
	"golang.org/x/crypto/ssh"
)

func TestRelayConnectorExecAndSSHStream(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	relayConfig := relay.DefaultConfig()
	relayConfig.PingInterval = 20 * time.Millisecond
	relayConfig.PingTimeout = time.Second
	relayConfig.ManagementToken = testManagementToken
	relayServer, err := relay.New(relayConfig, logger)
	if err != nil {
		t.Fatal(err)
	}
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
			"ARIADNE_INHERITED=test-value",
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
	if !wire.HasCapability(node.Capabilities, wire.CapabilityFileTransfer) {
		t.Fatalf("connector did not advertise file transfer: %#v", node.Capabilities)
	}
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

	commandContext, cancelCommand := context.WithTimeout(context.Background(), 2*time.Second)
	commandResult, err := apiClient.Exec(commandContext, "phone", wire.ExecRequest{
		Command:       "printf 'hello from shell' | tr a-z A-Z",
		TimeoutMillis: 1000,
	})
	cancelCommand()
	if err != nil {
		t.Fatal(err)
	}
	if commandResult.ExitCode != 0 || commandResult.Shell != "sh" || string(commandResult.Stdout) != "sh\x00-lc\x00printf 'hello from shell' | tr a-z A-Z" {
		t.Fatalf("unexpected shell command result: %#v", commandResult)
	}

	filePayload := append([]byte("ariadne-file\x00"), bytes.Repeat([]byte{0xff, 0x42}, 40<<10)...)
	uploadSource := filepath.Join(t.TempDir(), "source.bin")
	if err := os.WriteFile(uploadSource, filePayload, 0o640); err != nil {
		t.Fatal(err)
	}
	remotePath := filepath.Join(t.TempDir(), "remote.bin")
	transferContext, cancelTransfer := context.WithTimeout(context.Background(), 5*time.Second)
	uploadResult, err := apiClient.UploadFile(transferContext, "phone", uploadSource, remotePath, false)
	cancelTransfer()
	if err != nil || uploadResult.Size != int64(len(filePayload)) || uploadResult.SHA256 == "" {
		t.Fatalf("upload failed: result=%#v err=%v", uploadResult, err)
	}
	remotePayload, err := os.ReadFile(remotePath)
	if err != nil || !bytes.Equal(remotePayload, filePayload) {
		t.Fatalf("remote upload differs: size=%d err=%v", len(remotePayload), err)
	}

	downloadPath := filepath.Join(t.TempDir(), "download.bin")
	transferContext, cancelTransfer = context.WithTimeout(context.Background(), 5*time.Second)
	downloadResult, err := apiClient.DownloadFile(transferContext, "phone", remotePath, downloadPath, false)
	cancelTransfer()
	if err != nil || downloadResult.Size != uploadResult.Size || downloadResult.SHA256 != uploadResult.SHA256 {
		t.Fatalf("download failed: result=%#v err=%v", downloadResult, err)
	}
	downloadPayload, err := os.ReadFile(downloadPath)
	if err != nil || !bytes.Equal(downloadPayload, filePayload) {
		t.Fatalf("download differs: size=%d err=%v", len(downloadPayload), err)
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
	_, _ = io.WriteString(stdinWriter, "stty size\nprintf 'INHERITED=<%s>\\n' \"$ARIADNE_INHERITED\"\n")
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
	if !strings.Contains(output.String(), "INHERITED=<test-value>") {
		t.Fatalf("connector environment was not inherited by shell; output=%q", output.String())
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

	if err := apiClient.Revoke(context.Background(), node.ID); err != nil {
		t.Fatal(err)
	}
	waitForNoNodes(t, apiClient)
	reconnectContext, cancelReconnect := context.WithTimeout(context.Background(), 2*time.Second)
	reconnectErr := nodeConnector.RunOnce(reconnectContext)
	cancelReconnect()
	if reconnectErr == nil || !strings.Contains(reconnectErr.Error(), "revoked") {
		t.Fatalf("revoked node reconnected: %v", reconnectErr)
	}
}

func TestRelayConnectorExecAndStreamOverQUIC(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	relayConfig := relay.DefaultConfig()
	relayConfig.ManagementToken = testManagementToken
	relayServer, err := relay.New(relayConfig, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer relayServer.Close()
	managementServer := startHTTPTestServer(t, relayServer.ManagementHandler())
	defer managementServer.Close()

	certificatePath, keyPath, certificateDER := quicTestCertificate(t)
	quicServer, err := quictransport.Listen("127.0.0.1:0", certificatePath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	quicContext, cancelQUIC := context.WithCancel(context.Background())
	quicErrors := make(chan error, 1)
	go func() {
		quicErrors <- quicServer.Serve(quicContext, relayServer.ServeNodeConnection)
	}()
	defer func() {
		cancelQUIC()
		_ = quicServer.Close()
		<-quicErrors
	}()

	tlsConfig, err := transport.ClientTLSConfig("127.0.0.1", transport.FormatCertificatePin(certificateDER))
	if err != nil {
		t.Fatal(err)
	}
	echoAddress, closeEcho := startEchoServer(t)
	defer closeEcho()
	nodeIdentity, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	nodeConnector, err := connector.New(connector.Config{
		Alias:      "quic-node",
		Identity:   nodeIdentity,
		SSHAddress: echoAddress,
		Logger:     logger,
		Dial: func(ctx context.Context) (messageconn.Conn, error) {
			return quictransport.Dial(ctx, quicServer.Addr(), tlsConfig)
		},
	}, echoExecutor{})
	if err != nil {
		t.Fatal(err)
	}
	connectorContext, cancelConnector := context.WithCancel(context.Background())
	connectorErrors := make(chan error, 1)
	go func() { connectorErrors <- nodeConnector.RunOnce(connectorContext) }()
	defer func() {
		cancelConnector()
		select {
		case <-connectorErrors:
		case <-time.After(2 * time.Second):
			t.Error("QUIC connector did not stop")
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
	node := waitForNode(t, apiClient, "quic-node")
	if _, err := apiClient.Claim(context.Background(), node.ID, "quic-node"); err != nil {
		t.Fatal(err)
	}
	result, err := apiClient.Exec(context.Background(), "quic-node", wire.ExecRequest{
		Argv:          []string{"echo", "over-quic"},
		TimeoutMillis: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 || string(result.Stdout) != "echo\x00over-quic" {
		t.Fatalf("unexpected QUIC exec result: %#v", result)
	}

	streamContext, cancelStream := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelStream()
	stream, err := apiClient.DialStream(streamContext, "quic-node", "ssh")
	if err != nil {
		t.Fatal(err)
	}
	defer stream.CloseNow()
	payload := []byte("opaque payload over QUIC")
	if err := stream.Write(streamContext, websocket.MessageBinary, payload); err != nil {
		t.Fatal(err)
	}
	messageType, echoed, err := stream.Read(streamContext)
	if err != nil {
		t.Fatal(err)
	}
	if messageType != websocket.MessageBinary || string(echoed) != string(payload) {
		t.Fatalf("unexpected QUIC stream reply: type=%v payload=%q", messageType, echoed)
	}
}

func TestQUICConnectorReconnectsAfterTransportRestart(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	relayConfig := relay.DefaultConfig()
	relayConfig.ManagementToken = testManagementToken
	relayServer, err := relay.New(relayConfig, logger)
	if err != nil {
		t.Fatal(err)
	}
	managementServer := startHTTPTestServer(t, relayServer.ManagementHandler())

	certificatePath, keyPath, certificateDER := quicTestCertificate(t)
	firstQUICServer, err := quictransport.Listen("127.0.0.1:0", certificatePath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	address := firstQUICServer.Addr()
	firstContext, cancelFirst := context.WithCancel(context.Background())
	firstErrors := make(chan error, 1)
	go func() {
		firstErrors <- firstQUICServer.Serve(firstContext, relayServer.ServeNodeConnection)
	}()

	tlsConfig, err := transport.ClientTLSConfig("127.0.0.1", transport.FormatCertificatePin(certificateDER))
	if err != nil {
		t.Fatal(err)
	}
	nodeIdentity, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	nodeConnector, err := connector.New(connector.Config{
		Alias:            "reconnecting-node",
		Identity:         nodeIdentity,
		Logger:           logger,
		ReconnectInitial: 10 * time.Millisecond,
		ReconnectMaximum: 50 * time.Millisecond,
		DialTimeout:      500 * time.Millisecond,
		Dial: func(ctx context.Context) (messageconn.Conn, error) {
			return quictransport.Dial(ctx, address, tlsConfig)
		},
	}, echoExecutor{})
	if err != nil {
		t.Fatal(err)
	}
	connectorContext, cancelConnector := context.WithCancel(context.Background())
	connectorErrors := make(chan error, 1)
	go func() { connectorErrors <- nodeConnector.Run(connectorContext) }()

	apiClient, err := client.New(client.Config{
		RelayURL:        managementServer.URL,
		ManagementToken: testManagementToken,
		HTTPClient:      managementServer.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	firstNode := waitForNode(t, apiClient, "reconnecting-node")
	if _, err := apiClient.Claim(context.Background(), firstNode.ID, "reconnecting-node"); err != nil {
		t.Fatal(err)
	}
	if !wire.HasCapability(firstNode.Capabilities, wire.CapabilityBackgroundJobs) {
		t.Fatalf("connector did not advertise background jobs: %#v", firstNode.Capabilities)
	}
	job, err := apiClient.StartJob(context.Background(), "reconnecting-node", wire.ExecRequest{
		Argv: []string{"/bin/sh", "-c", "printf 'before\\n'; sleep 0.4; printf 'after\\n'"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if job.ID == "" || job.State != "running" {
		t.Fatalf("unexpected started job: %#v", job)
	}

	cancelFirst()
	if err := firstQUICServer.Close(); err != nil {
		t.Fatal(err)
	}
	<-firstErrors
	waitForNoNodes(t, apiClient)

	secondQUICServer, err := quictransport.Listen(address, certificatePath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	secondContext, cancelSecond := context.WithCancel(context.Background())
	secondErrors := make(chan error, 1)
	go func() {
		secondErrors <- secondQUICServer.Serve(secondContext, relayServer.ServeNodeConnection)
	}()
	defer func() {
		cancelConnector()
		select {
		case <-connectorErrors:
		case <-time.After(2 * time.Second):
			t.Error("reconnecting connector did not stop")
		}
		cancelSecond()
		_ = secondQUICServer.Close()
		<-secondErrors
		managementServer.Close()
		relayServer.Close()
	}()

	secondNode := waitForNode(t, apiClient, "reconnecting-node")
	if secondNode.ID != firstNode.ID || !secondNode.AliasClaimed {
		t.Fatalf("reconnected node lost identity or claim: before=%#v after=%#v", firstNode, secondNode)
	}
	job = waitForJob(t, apiClient, "reconnecting-node", job.ID)
	if job.State != "succeeded" || job.ExitCode != 0 {
		t.Fatalf("job did not survive transport restart: %#v", job)
	}
	job, output, err := apiClient.ReadJob(context.Background(), "reconnecting-node", job.ID, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if string(output.Stdout) != "before\nafter\n" || !output.StdoutEOF || !output.StderrEOF {
		t.Fatalf("unexpected retained job output: job=%#v output=%#v", job, output)
	}
	result, err := apiClient.Exec(context.Background(), "reconnecting-node", wire.ExecRequest{
		Argv:          []string{"echo", "after-reconnect"},
		TimeoutMillis: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(result.Stdout) != "echo\x00after-reconnect" {
		t.Fatalf("unexpected post-reconnect result: %#v", result)
	}
}

func waitForJob(t *testing.T, apiClient *client.Client, target, jobID string) wire.JobInfo {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		job, err := apiClient.JobStatus(context.Background(), target, jobID)
		if err == nil && job.State != "running" {
			return job
		}
		time.Sleep(10 * time.Millisecond)
	}
	job, err := apiClient.JobStatus(context.Background(), target, jobID)
	t.Fatalf("job did not finish: job=%#v err=%v", job, err)
	return wire.JobInfo{}
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

func quicTestCertificate(t *testing.T) (string, string, []byte) {
	t.Helper()
	tlsServer := httptest.NewTLSServer(http.NotFoundHandler())
	certificate := tlsServer.TLS.Certificates[0]
	tlsServer.Close()
	privateDER, err := x509.MarshalPKCS8PrivateKey(certificate.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	certificatePath := filepath.Join(directory, "relay.crt")
	keyPath := filepath.Join(directory, "relay.key")
	if err := os.WriteFile(certificatePath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Certificate[0]}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certificatePath, keyPath, certificate.Certificate[0]
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

func waitForNoNodes(t *testing.T, apiClient *client.Client) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		nodes, err := apiClient.Nodes(ctx)
		cancel()
		if err == nil && len(nodes) == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("relay still reports nodes after QUIC transport shutdown")
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
