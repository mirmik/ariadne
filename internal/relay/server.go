package relay

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/mirmik/ariadne/internal/execspec"
	"github.com/mirmik/ariadne/internal/identity"
	"github.com/mirmik/ariadne/internal/managementauth"
	"github.com/mirmik/ariadne/internal/messageconn"
	"github.com/mirmik/ariadne/internal/wire"
	"golang.org/x/crypto/ssh"
)

type Config struct {
	Version              string
	RegistrationTimeout  time.Duration
	StreamOpenTimeout    time.Duration
	DefaultExecTimeout   time.Duration
	MaxExecTimeout       time.Duration
	ExecResultGrace      time.Duration
	PingInterval         time.Duration
	PingTimeout          time.Duration
	MaxPendingHandshakes int
	MaxOnlineNodes       int
	ManagementToken      string
}

func DefaultConfig() Config {
	return Config{
		RegistrationTimeout:  10 * time.Second,
		StreamOpenTimeout:    10 * time.Second,
		DefaultExecTimeout:   30 * time.Second,
		MaxExecTimeout:       10 * time.Minute,
		ExecResultGrace:      5 * time.Second,
		PingInterval:         30 * time.Second,
		PingTimeout:          10 * time.Second,
		MaxPendingHandshakes: 64,
		MaxOnlineNodes:       1024,
	}
}

type Server struct {
	config Config
	logger *slog.Logger

	context    context.Context
	cancel     context.CancelFunc
	handshakes chan struct{}

	mu             sync.RWMutex
	byID           map[string]*nodeSession
	claimsByID     map[string]string
	claimedByAlias map[string]string
}

func New(config Config, logger *slog.Logger) *Server {
	defaults := DefaultConfig()
	if config.Version == "" {
		config.Version = "dev"
	}
	if config.RegistrationTimeout <= 0 {
		config.RegistrationTimeout = defaults.RegistrationTimeout
	}
	if config.StreamOpenTimeout <= 0 {
		config.StreamOpenTimeout = defaults.StreamOpenTimeout
	}
	if config.DefaultExecTimeout <= 0 {
		config.DefaultExecTimeout = defaults.DefaultExecTimeout
	}
	if config.MaxExecTimeout <= 0 {
		config.MaxExecTimeout = defaults.MaxExecTimeout
	}
	if config.DefaultExecTimeout > config.MaxExecTimeout {
		config.DefaultExecTimeout = config.MaxExecTimeout
	}
	if config.ExecResultGrace <= 0 {
		config.ExecResultGrace = defaults.ExecResultGrace
	}
	if config.PingInterval < 0 {
		config.PingInterval = 0
	} else if config.PingInterval == 0 {
		config.PingInterval = defaults.PingInterval
	}
	if config.PingTimeout <= 0 {
		config.PingTimeout = defaults.PingTimeout
	}
	if config.MaxPendingHandshakes <= 0 {
		config.MaxPendingHandshakes = defaults.MaxPendingHandshakes
	}
	if config.MaxOnlineNodes <= 0 {
		config.MaxOnlineNodes = defaults.MaxOnlineNodes
	}
	if logger == nil {
		logger = slog.Default()
	}
	serverContext, cancel := context.WithCancel(context.Background())
	return &Server{
		config:         config,
		logger:         logger,
		context:        serverContext,
		cancel:         cancel,
		handshakes:     make(chan struct{}, config.MaxPendingHandshakes),
		byID:           make(map[string]*nodeSession),
		claimsByID:     make(map[string]string),
		claimedByAlias: make(map[string]string),
	}
}

// NodeHandler exposes only the connector-facing, reactive node plane.
func (server *Server) NodeHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", server.handleHealth)
	mux.HandleFunc("GET /v1/connect", server.handleConnector)
	return securityHeaders(mux)
}

// ManagementHandler exposes operations which may cause actions on nodes. It
// must be bound to loopback or another independently authenticated transport.
func (server *Server) ManagementHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", server.handleHealth)
	mux.HandleFunc("GET /v1/nodes", server.handleNodes)
	mux.HandleFunc("POST /v1/nodes/", server.handleNodeAction)
	mux.HandleFunc("GET /v1/nodes/", server.handleNodeAction)
	return authenticateManagement(server.config.ManagementToken, securityHeaders(mux))
}

func authenticateManagement(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		provided := strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
		if token == "" || !managementauth.Equal(token, provided) {
			response.Header().Set("WWW-Authenticate", "Bearer")
			writeAPIError(response, http.StatusUnauthorized, "management authentication required")
			return
		}
		next.ServeHTTP(response, request)
	})
}

func (server *Server) Close() {
	server.cancel()
	server.mu.RLock()
	sessions := make([]*nodeSession, 0, len(server.byID))
	for _, session := range server.byID {
		sessions = append(sessions, session)
	}
	server.mu.RUnlock()
	for _, session := range sessions {
		session.close(errors.New("relay is shutting down"))
	}
}

func (server *Server) handleHealth(response http.ResponseWriter, request *http.Request) {
	writeJSON(response, http.StatusOK, map[string]string{"status": "ok", "version": server.config.Version})
}

func (server *Server) handleNodes(response http.ResponseWriter, request *http.Request) {
	server.mu.RLock()
	nodes := make([]wire.NodeInfo, 0, len(server.byID))
	for _, session := range server.byID {
		nodes = append(nodes, session.nodeInfo())
	}
	server.mu.RUnlock()
	sort.Slice(nodes, func(left, right int) bool {
		return strings.ToLower(nodes[left].Alias) < strings.ToLower(nodes[right].Alias)
	})
	writeJSON(response, http.StatusOK, wire.NodesResponse{Nodes: nodes})
}

func (server *Server) handleNodeAction(response http.ResponseWriter, request *http.Request) {
	target, action, ok := parseNodeAction(request.URL.Path)
	if !ok {
		writeAPIError(response, http.StatusNotFound, "unknown node endpoint")
		return
	}
	if request.Method == http.MethodPost && action == "claim" {
		server.handleClaim(response, request, target)
		return
	}
	session := server.lookup(target)
	if session == nil {
		writeAPIError(response, http.StatusNotFound, "node is not online")
		return
	}

	switch {
	case request.Method == http.MethodPost && action == "exec":
		server.handleExec(response, request, session)
	case request.Method == http.MethodGet && action == "streams/ssh":
		server.handleStreamProxy(response, request, session, "ssh")
	case request.Method == http.MethodGet && action == "streams/shell":
		server.handleStreamProxy(response, request, session, "shell")
	default:
		writeAPIError(response, http.StatusNotFound, "unknown node endpoint")
	}
}

func (server *Server) handleClaim(response http.ResponseWriter, request *http.Request, nodeID string) {
	request.Body = http.MaxBytesReader(response, request.Body, 16<<10)
	var claimRequest wire.ClaimRequest
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&claimRequest); err != nil {
		writeAPIError(response, http.StatusBadRequest, "invalid claim request: "+err.Error())
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		writeAPIError(response, http.StatusBadRequest, "invalid claim request: unexpected data after JSON value")
		return
	}
	if !wire.ValidAlias(claimRequest.Alias) {
		writeAPIError(response, http.StatusBadRequest, "alias must match [A-Za-z0-9][A-Za-z0-9._-]{0,62}")
		return
	}
	node, err := server.claim(nodeID, claimRequest.Alias)
	if err != nil {
		status := http.StatusConflict
		if errors.Is(err, errNodeNotOnline) {
			status = http.StatusNotFound
		}
		writeAPIError(response, status, err.Error())
		return
	}
	writeJSON(response, http.StatusOK, node)
}

func (server *Server) handleExec(response http.ResponseWriter, request *http.Request, session *nodeSession) {
	request.Body = http.MaxBytesReader(response, request.Body, 64<<10)
	var execRequest wire.ExecRequest
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&execRequest); err != nil {
		writeAPIError(response, http.StatusBadRequest, "invalid exec request: "+err.Error())
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		writeAPIError(response, http.StatusBadRequest, "invalid exec request: unexpected data after JSON value")
		return
	}
	preparedRequest, usedShell, err := execspec.Prepare(execRequest, session.nodeInfo().Platform)
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, err.Error())
		return
	}
	if preparedRequest.TimeoutMillis < 0 || preparedRequest.TimeoutMillis > server.config.MaxExecTimeout.Milliseconds() {
		writeAPIError(response, http.StatusBadRequest, fmt.Sprintf("timeout must be non-negative and not exceed %s", server.config.MaxExecTimeout))
		return
	}
	timeout := server.config.DefaultExecTimeout
	if preparedRequest.TimeoutMillis > 0 {
		timeout = time.Duration(preparedRequest.TimeoutMillis) * time.Millisecond
	}
	preparedRequest.TimeoutMillis = timeout.Milliseconds()

	requestContext, cancel := context.WithTimeout(request.Context(), timeout+server.config.ExecResultGrace)
	defer cancel()
	result, err := session.exec(requestContext, preparedRequest)
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, context.DeadlineExceeded) {
			status = http.StatusGatewayTimeout
		}
		writeAPIError(response, status, err.Error())
		return
	}
	result.Shell = usedShell
	writeJSON(response, http.StatusOK, result)
}

func (server *Server) handleConnector(response http.ResponseWriter, request *http.Request) {
	select {
	case server.handshakes <- struct{}{}:
	default:
		writeAPIError(response, http.StatusServiceUnavailable, "too many connector handshakes")
		return
	}
	connection, err := websocket.Accept(response, request, nil)
	if err != nil {
		server.logger.Warn("connector WebSocket upgrade failed", "error", err)
		<-server.handshakes
		return
	}
	connection.SetReadLimit(wire.MaxControlMessageSize)
	server.serveConnector(messageconn.WebSocket{Conn: connection}, true)
}

// ServeNodeConnection registers and runs one non-HTTP node transport, such as
// a QUIC control connection. Ownership of connection is transferred here.
func (server *Server) ServeNodeConnection(connection messageconn.Conn) {
	server.serveConnector(connection, false)
}

func (server *Server) serveConnector(connection messageconn.Conn, handshakeSlotHeld bool) {
	if !handshakeSlotHeld {
		select {
		case server.handshakes <- struct{}{}:
			handshakeSlotHeld = true
		default:
			server.writeProtocolError(connection, "capacity", "too many connector handshakes")
			connection.CloseNow()
			return
		}
	}
	defer func() {
		if handshakeSlotHeld {
			<-server.handshakes
		}
	}()
	defer connection.CloseNow()

	handshakeContext, cancel := context.WithTimeout(server.context, server.config.RegistrationTimeout)
	defer cancel()
	hello, publicKey, err := readAndValidateHello(handshakeContext, connection)
	if err != nil {
		server.writeProtocolError(connection, "invalid_hello", err.Error())
		return
	}
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		server.writeProtocolError(connection, "internal_error", "could not create registration challenge")
		return
	}
	if err := writeControl(handshakeContext, connection, wire.MessageChallenge, "", wire.Challenge{
		Nonce: base64.RawStdEncoding.EncodeToString(nonce),
	}); err != nil {
		return
	}
	if err := readAndVerifyRegistration(handshakeContext, connection, hello, publicKey, nonce); err != nil {
		server.writeProtocolError(connection, "invalid_signature", err.Error())
		return
	}
	<-server.handshakes
	handshakeSlotHeld = false

	session := newNodeSession(server, connection, wire.NodeInfo{
		ID:               hello.NodeID,
		Alias:            hello.Alias,
		SSHHostKey:       hello.SSHHostKey,
		Platform:         hello.Platform,
		Architecture:     hello.Architecture,
		ConnectorVersion: hello.ConnectorVersion,
		ConnectedAt:      time.Now().UTC(),
		Online:           true,
	})
	replaced, err := server.register(session)
	if err != nil {
		server.writeProtocolError(connection, "capacity", err.Error())
		return
	}
	if replaced != nil {
		replaced.close(errors.New("superseded by a new connection from the same node"))
	}
	defer server.unregister(session)

	sessionInfo := session.nodeInfo()
	if err := writeControl(server.context, connection, wire.MessageRegistered, "", wire.Registered{Node: sessionInfo}); err != nil {
		return
	}
	server.logger.Info("node connected", "node_id", sessionInfo.ID, "alias", sessionInfo.Alias, "platform", sessionInfo.Platform)
	if err := session.run(); err != nil && !errors.Is(err, context.Canceled) {
		sessionInfo = session.nodeInfo()
		server.logger.Info("node disconnected", "node_id", sessionInfo.ID, "alias", sessionInfo.Alias, "error", err)
	}
}

var errNodeNotOnline = errors.New("node is not online")

func (server *Server) lookup(target string) *nodeSession {
	server.mu.RLock()
	defer server.mu.RUnlock()
	if session := server.byID[target]; session != nil {
		return session
	}
	if nodeID := server.claimedByAlias[strings.ToLower(target)]; nodeID != "" {
		return server.byID[nodeID]
	}
	return nil
}

func (server *Server) register(session *nodeSession) (*nodeSession, error) {
	server.mu.Lock()
	defer server.mu.Unlock()
	info := session.nodeInfo()
	replaced := server.byID[info.ID]
	if replaced == nil && len(server.byID) >= server.config.MaxOnlineNodes {
		return nil, errors.New("relay is at online node capacity")
	}
	if alias := server.claimsByID[info.ID]; alias != "" {
		session.setClaimedAlias(alias)
	}
	server.byID[info.ID] = session
	return replaced, nil
}

func (server *Server) unregister(session *nodeSession) {
	server.mu.Lock()
	info := session.nodeInfo()
	if server.byID[info.ID] == session {
		delete(server.byID, info.ID)
	}
	server.mu.Unlock()
	session.close(errors.New("connector disconnected"))
}

func (server *Server) claim(nodeID, alias string) (wire.NodeInfo, error) {
	server.mu.Lock()
	defer server.mu.Unlock()
	session := server.byID[nodeID]
	if session == nil {
		return wire.NodeInfo{}, errNodeNotOnline
	}
	aliasKey := strings.ToLower(alias)
	if owner := server.claimedByAlias[aliasKey]; owner != "" && owner != nodeID {
		return wire.NodeInfo{}, fmt.Errorf("alias %q is already claimed by another node", alias)
	}
	if previous := server.claimsByID[nodeID]; previous != "" {
		delete(server.claimedByAlias, strings.ToLower(previous))
	}
	server.claimsByID[nodeID] = alias
	server.claimedByAlias[aliasKey] = nodeID
	session.setClaimedAlias(alias)
	return session.nodeInfo(), nil
}

func readAndValidateHello(ctx context.Context, connection messageconn.Conn) (wire.Hello, ed25519.PublicKey, error) {
	envelope, err := readControl(ctx, connection)
	if err != nil {
		return wire.Hello{}, nil, err
	}
	if envelope.Type != wire.MessageHello {
		return wire.Hello{}, nil, fmt.Errorf("expected %s, got %s", wire.MessageHello, envelope.Type)
	}
	hello, err := wire.DecodePayload[wire.Hello](envelope)
	if err != nil {
		return wire.Hello{}, nil, err
	}
	if !wire.ValidAlias(hello.Alias) {
		return wire.Hello{}, nil, errors.New("alias must match [A-Za-z0-9][A-Za-z0-9._-]{0,62}")
	}
	if hello.Platform == "" || len(hello.Platform) > 32 || hello.Architecture == "" || len(hello.Architecture) > 32 {
		return wire.Hello{}, nil, errors.New("platform and architecture must contain 1 to 32 characters")
	}
	if len(hello.ConnectorVersion) > 64 {
		return wire.Hello{}, nil, errors.New("connector version is too long")
	}
	publicKey, err := identity.ParsePublicKey(hello.PublicKey)
	if err != nil {
		return wire.Hello{}, nil, err
	}
	if expected := identity.NodeID(publicKey); hello.NodeID != expected {
		return wire.Hello{}, nil, fmt.Errorf("node ID does not match public key; expected %s", expected)
	}
	if _, err := parseWireSSHKey(hello.SSHHostKey); err != nil {
		return wire.Hello{}, nil, fmt.Errorf("invalid embedded SSH host key: %w", err)
	}
	return hello, publicKey, nil
}

func parseWireSSHKey(encoded string) (ssh.PublicKey, error) {
	if encoded == "" {
		return nil, errors.New("key is required")
	}
	if len(encoded) > 1024 {
		return nil, errors.New("key is too large")
	}
	raw, err := base64.RawStdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, errors.New("key is not valid base64")
	}
	key, err := ssh.ParsePublicKey(raw)
	if err != nil || key.Type() != ssh.KeyAlgoED25519 {
		return nil, errors.New("key must be Ed25519")
	}
	return key, nil
}

func readAndVerifyRegistration(ctx context.Context, connection messageconn.Conn, hello wire.Hello, publicKey ed25519.PublicKey, nonce []byte) error {
	envelope, err := readControl(ctx, connection)
	if err != nil {
		return err
	}
	if envelope.Type != wire.MessageRegister {
		return fmt.Errorf("expected %s, got %s", wire.MessageRegister, envelope.Type)
	}
	registration, err := wire.DecodePayload[wire.Registration](envelope)
	if err != nil {
		return err
	}
	signature, err := identity.ParseSignature(registration.Signature)
	if err != nil {
		return err
	}
	if !ed25519.Verify(publicKey, wire.RegistrationTranscript(nonce, hello), signature) {
		return errors.New("registration signature is not valid")
	}
	return nil
}

func parseNodeAction(path string) (string, string, bool) {
	remainder := strings.TrimPrefix(path, "/v1/nodes/")
	parts := strings.SplitN(remainder, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	target, err := url.PathUnescape(parts[0])
	if err != nil || target == "" {
		return "", "", false
	}
	return target, parts[1], true
}

func (server *Server) writeProtocolError(connection messageconn.Conn, code, message string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = writeControl(ctx, connection, wire.MessageError, "", wire.ErrorPayload{Code: code, Message: message})
}

func readControl(ctx context.Context, connection messageconn.Conn) (wire.Envelope, error) {
	messageType, data, err := connection.Read(ctx)
	if err != nil {
		return wire.Envelope{}, err
	}
	if messageType != websocket.MessageText {
		return wire.Envelope{}, errors.New("expected a text control message")
	}
	return wire.DecodeEnvelope(data)
}

func writeControl(ctx context.Context, connection messageconn.Conn, messageType wire.MessageType, id string, payload any) error {
	data, err := wire.MarshalEnvelope(messageType, id, payload)
	if err != nil {
		return err
	}
	return connection.Write(ctx, websocket.MessageText, data)
}

func writeJSON(response http.ResponseWriter, status int, value any) {
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(value)
}

func writeAPIError(response http.ResponseWriter, status int, message string) {
	writeJSON(response, status, wire.APIError{Error: message})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("X-Content-Type-Options", "nosniff")
		response.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(response, request)
	})
}
