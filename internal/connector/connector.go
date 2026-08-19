package connector

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/mirmik/ariadne/internal/identity"
	"github.com/mirmik/ariadne/internal/wire"
	"golang.org/x/crypto/ssh"
)

type Config struct {
	RelayURL          string
	Token             string
	Alias             string
	Version           string
	Identity          *identity.Identity
	SSHAddress        string
	Shell             string
	ShellEnvironment  []string
	MaxConcurrentExec int
	MaxExecTimeout    time.Duration
	MaxOutputBytes    int
	MaxStreams        int
	DialTimeout       time.Duration
	HTTPClient        *http.Client
	Logger            *slog.Logger
	ReconnectInitial  time.Duration
	ReconnectMaximum  time.Duration
}

type Connector struct {
	config     Config
	executor   Executor
	sshServer  *embeddedSSHServer
	sshHostKey string
}

func New(config Config, executor Executor) (*Connector, error) {
	if config.Identity == nil {
		return nil, errors.New("connector identity is required")
	}
	if !wire.ValidAlias(config.Alias) {
		return nil, errors.New("alias must match [A-Za-z0-9][A-Za-z0-9._-]{0,62}")
	}
	normalizedURL, err := ConnectorURL(config.RelayURL)
	if err != nil {
		return nil, err
	}
	config.RelayURL = normalizedURL
	if config.Version == "" {
		config.Version = "dev"
	}
	if config.SSHAddress == "" {
		config.SSHAddress = "127.0.0.1:22"
	}
	if config.MaxConcurrentExec <= 0 {
		config.MaxConcurrentExec = 4
	}
	if config.MaxExecTimeout <= 0 {
		config.MaxExecTimeout = 10 * time.Minute
	}
	if config.MaxOutputBytes <= 0 {
		config.MaxOutputBytes = defaultMaxOutputBytes
	}
	if config.MaxOutputBytes > wire.MaxExecOutputBytes {
		return nil, fmt.Errorf("max output exceeds protocol limit of %d bytes", wire.MaxExecOutputBytes)
	}
	if config.MaxStreams <= 0 {
		config.MaxStreams = 64
	}
	if config.DialTimeout <= 0 {
		config.DialTimeout = 10 * time.Second
	}
	if config.Logger == nil {
		config.Logger = slog.Default()
	}
	if config.ReconnectInitial <= 0 {
		config.ReconnectInitial = time.Second
	}
	if config.ReconnectMaximum <= 0 {
		config.ReconnectMaximum = 30 * time.Second
	}
	if executor == nil {
		executor = LocalExecutor{MaxOutputBytes: config.MaxOutputBytes}
	}
	hostSigner, err := ssh.NewSignerFromKey(config.Identity.SSHHostSigner())
	if err != nil {
		return nil, fmt.Errorf("create embedded SSH host signer: %w", err)
	}
	return &Connector{
		config:     config,
		executor:   executor,
		sshServer:  newEmbeddedSSHServer(hostSigner, config.Shell, config.ShellEnvironment, config.Logger),
		sshHostKey: base64.RawStdEncoding.EncodeToString(hostSigner.PublicKey().Marshal()),
	}, nil
}

func ConnectorURL(base string) (string, error) {
	if strings.TrimSpace(base) == "" {
		return "", errors.New("relay URL is required")
	}
	parsed, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("parse relay URL: %w", err)
	}
	if parsed.Scheme != "ws" && parsed.Scheme != "wss" && parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", errors.New("relay URL scheme must be ws, wss, http, or https")
	}
	if parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("relay URL must contain a host and no credentials, query, or fragment")
	}
	path := strings.TrimSuffix(parsed.Path, "/")
	if path == "" {
		path = "/v1/connect"
	} else if path != "/v1/connect" {
		path += "/v1/connect"
	}
	parsed.Path = path
	return parsed.String(), nil
}

func (connector *Connector) Run(ctx context.Context) error {
	backoff := connector.config.ReconnectInitial
	for {
		sessionStarted := time.Now()
		err := connector.RunOnce(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if time.Since(sessionStarted) >= time.Minute {
			backoff = connector.config.ReconnectInitial
		}
		connector.config.Logger.Warn("connector session ended; reconnecting", "error", err, "after", backoff)
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
		backoff *= 2
		if backoff > connector.config.ReconnectMaximum {
			backoff = connector.config.ReconnectMaximum
		}
	}
}

func (connector *Connector) RunOnce(ctx context.Context) error {
	headers := make(http.Header)
	if connector.config.Token != "" {
		headers.Set("Authorization", "Bearer "+connector.config.Token)
	}
	dialContext, cancelDial := context.WithTimeout(ctx, connector.config.DialTimeout)
	connection, _, err := websocket.Dial(dialContext, connector.config.RelayURL, &websocket.DialOptions{
		HTTPClient:      connector.config.HTTPClient,
		HTTPHeader:      headers,
		CompressionMode: websocket.CompressionDisabled,
	})
	cancelDial()
	if err != nil {
		return fmt.Errorf("connect to relay: %w", err)
	}
	connection.SetReadLimit(wire.MaxControlMessageSize)
	defer connection.CloseNow()

	handshakeContext, cancelHandshake := context.WithTimeout(ctx, 10*time.Second)
	defer cancelHandshake()
	hello := wire.Hello{
		NodeID:           connector.config.Identity.NodeID(),
		Alias:            connector.config.Alias,
		PublicKey:        connector.config.Identity.EncodedPublicKey(),
		SSHHostKey:       connector.sshHostKey,
		Platform:         runtime.GOOS,
		Architecture:     runtime.GOARCH,
		ConnectorVersion: connector.config.Version,
	}
	if err := writeConnectorControl(handshakeContext, connection, wire.MessageHello, "", hello); err != nil {
		return fmt.Errorf("send connector hello: %w", err)
	}
	challengeEnvelope, err := readConnectorControl(handshakeContext, connection)
	if err != nil {
		return fmt.Errorf("read registration challenge: %w", err)
	}
	if err := rejectProtocolError(challengeEnvelope); err != nil {
		return err
	}
	if challengeEnvelope.Type != wire.MessageChallenge {
		return fmt.Errorf("expected %s, got %s", wire.MessageChallenge, challengeEnvelope.Type)
	}
	challenge, err := wire.DecodePayload[wire.Challenge](challengeEnvelope)
	if err != nil {
		return err
	}
	nonce, err := base64.RawStdEncoding.DecodeString(challenge.Nonce)
	if err != nil || len(nonce) != 32 {
		return errors.New("relay sent an invalid registration nonce")
	}
	registration := wire.Registration{
		Signature: connector.config.Identity.Sign(wire.RegistrationTranscript(nonce, hello)),
	}
	if err := writeConnectorControl(handshakeContext, connection, wire.MessageRegister, "", registration); err != nil {
		return fmt.Errorf("send connector registration: %w", err)
	}
	registeredEnvelope, err := readConnectorControl(handshakeContext, connection)
	if err != nil {
		return fmt.Errorf("read registration result: %w", err)
	}
	if err := rejectProtocolError(registeredEnvelope); err != nil {
		return err
	}
	if registeredEnvelope.Type != wire.MessageRegistered {
		return fmt.Errorf("expected %s, got %s", wire.MessageRegistered, registeredEnvelope.Type)
	}
	registered, err := wire.DecodePayload[wire.Registered](registeredEnvelope)
	if err != nil {
		return err
	}
	if registered.Node.ID != hello.NodeID || registered.Node.Alias != hello.Alias || registered.Node.SSHHostKey != hello.SSHHostKey {
		return errors.New("relay registered a different node identity")
	}
	cancelHandshake()

	connector.config.Logger.Info("connected to relay", "node_id", hello.NodeID, "alias", hello.Alias)
	session := newSession(ctx, connector, connection)
	return session.run()
}

func readConnectorControl(ctx context.Context, connection *websocket.Conn) (wire.Envelope, error) {
	messageType, data, err := connection.Read(ctx)
	if err != nil {
		return wire.Envelope{}, err
	}
	if messageType != websocket.MessageText {
		return wire.Envelope{}, errors.New("expected a text control message")
	}
	return wire.DecodeEnvelope(data)
}

func writeConnectorControl(ctx context.Context, connection *websocket.Conn, messageType wire.MessageType, id string, payload any) error {
	data, err := wire.MarshalEnvelope(messageType, id, payload)
	if err != nil {
		return err
	}
	return connection.Write(ctx, websocket.MessageText, data)
}

func rejectProtocolError(envelope wire.Envelope) error {
	if envelope.Type != wire.MessageError {
		return nil
	}
	protocolError, err := wire.DecodePayload[wire.ErrorPayload](envelope)
	if err != nil {
		return err
	}
	return fmt.Errorf("relay rejected connector (%s): %s", protocolError.Code, protocolError.Message)
}
