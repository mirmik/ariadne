package relay

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/subtle"
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
	"github.com/mirmik/ariadne/internal/identity"
	"github.com/mirmik/ariadne/internal/wire"
)

type Config struct {
	Version             string
	Token               string
	RegistrationTimeout time.Duration
	StreamOpenTimeout   time.Duration
	DefaultExecTimeout  time.Duration
	MaxExecTimeout      time.Duration
	ExecResultGrace     time.Duration
	PingInterval        time.Duration
	PingTimeout         time.Duration
}

func DefaultConfig() Config {
	return Config{
		RegistrationTimeout: 10 * time.Second,
		StreamOpenTimeout:   10 * time.Second,
		DefaultExecTimeout:  30 * time.Second,
		MaxExecTimeout:      10 * time.Minute,
		ExecResultGrace:     5 * time.Second,
		PingInterval:        30 * time.Second,
		PingTimeout:         10 * time.Second,
	}
}

type Server struct {
	config Config
	logger *slog.Logger

	context context.Context
	cancel  context.CancelFunc

	mu      sync.RWMutex
	byID    map[string]*nodeSession
	byAlias map[string]*nodeSession
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
	if logger == nil {
		logger = slog.Default()
	}
	serverContext, cancel := context.WithCancel(context.Background())
	return &Server{
		config:  config,
		logger:  logger,
		context: serverContext,
		cancel:  cancel,
		byID:    make(map[string]*nodeSession),
		byAlias: make(map[string]*nodeSession),
	}
}

func (server *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", server.handleHealth)
	mux.Handle("GET /v1/nodes", server.authorize(http.HandlerFunc(server.handleNodes)))
	mux.Handle("POST /v1/nodes/", server.authorize(http.HandlerFunc(server.handleNodeAction)))
	mux.Handle("GET /v1/nodes/", server.authorize(http.HandlerFunc(server.handleNodeAction)))
	mux.Handle("GET /v1/connect", server.authorize(http.HandlerFunc(server.handleConnector)))
	return securityHeaders(mux)
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
		nodes = append(nodes, session.info)
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
	default:
		writeAPIError(response, http.StatusNotFound, "unknown node endpoint")
	}
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
	if len(execRequest.Argv) == 0 || execRequest.Argv[0] == "" {
		writeAPIError(response, http.StatusBadRequest, "argv must contain a non-empty executable")
		return
	}
	if execRequest.TimeoutMillis < 0 || execRequest.TimeoutMillis > server.config.MaxExecTimeout.Milliseconds() {
		writeAPIError(response, http.StatusBadRequest, fmt.Sprintf("timeout must be non-negative and not exceed %s", server.config.MaxExecTimeout))
		return
	}
	timeout := server.config.DefaultExecTimeout
	if execRequest.TimeoutMillis > 0 {
		timeout = time.Duration(execRequest.TimeoutMillis) * time.Millisecond
	}
	execRequest.TimeoutMillis = timeout.Milliseconds()

	requestContext, cancel := context.WithTimeout(request.Context(), timeout+server.config.ExecResultGrace)
	defer cancel()
	result, err := session.exec(requestContext, execRequest)
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, context.DeadlineExceeded) {
			status = http.StatusGatewayTimeout
		}
		writeAPIError(response, status, err.Error())
		return
	}
	writeJSON(response, http.StatusOK, result)
}

func (server *Server) handleConnector(response http.ResponseWriter, request *http.Request) {
	connection, err := websocket.Accept(response, request, nil)
	if err != nil {
		server.logger.Warn("connector WebSocket upgrade failed", "error", err)
		return
	}
	connection.SetReadLimit(wire.MaxControlMessageSize)
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

	session := newNodeSession(server, connection, wire.NodeInfo{
		ID:               hello.NodeID,
		Alias:            hello.Alias,
		Platform:         hello.Platform,
		Architecture:     hello.Architecture,
		ConnectorVersion: hello.ConnectorVersion,
		ConnectedAt:      time.Now().UTC(),
		Online:           true,
	})
	replaced, err := server.register(session)
	if err != nil {
		server.writeProtocolError(connection, "alias_conflict", err.Error())
		return
	}
	if replaced != nil {
		replaced.close(errors.New("superseded by a new connection from the same node"))
	}
	defer server.unregister(session)

	if err := writeControl(server.context, connection, wire.MessageRegistered, "", wire.Registered{Node: session.info}); err != nil {
		return
	}
	server.logger.Info("node connected", "node_id", session.info.ID, "alias", session.info.Alias, "platform", session.info.Platform)
	if err := session.run(); err != nil && !errors.Is(err, context.Canceled) {
		server.logger.Info("node disconnected", "node_id", session.info.ID, "alias", session.info.Alias, "error", err)
	}
}

func (server *Server) authorize(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if server.config.Token == "" {
			next.ServeHTTP(response, request)
			return
		}
		expected := "Bearer " + server.config.Token
		provided := request.Header.Get("Authorization")
		if subtle.ConstantTimeCompare([]byte(expected), []byte(provided)) != 1 {
			response.Header().Set("WWW-Authenticate", "Bearer")
			writeAPIError(response, http.StatusUnauthorized, "authentication required")
			return
		}
		next.ServeHTTP(response, request)
	})
}

func (server *Server) lookup(target string) *nodeSession {
	server.mu.RLock()
	defer server.mu.RUnlock()
	if session := server.byID[target]; session != nil {
		return session
	}
	return server.byAlias[strings.ToLower(target)]
}

func (server *Server) register(session *nodeSession) (*nodeSession, error) {
	server.mu.Lock()
	defer server.mu.Unlock()
	aliasKey := strings.ToLower(session.info.Alias)
	if owner := server.byAlias[aliasKey]; owner != nil && owner.info.ID != session.info.ID {
		return nil, fmt.Errorf("alias %q is already used by another node", session.info.Alias)
	}
	replaced := server.byID[session.info.ID]
	if replaced != nil {
		delete(server.byAlias, strings.ToLower(replaced.info.Alias))
	}
	server.byID[session.info.ID] = session
	server.byAlias[aliasKey] = session
	return replaced, nil
}

func (server *Server) unregister(session *nodeSession) {
	server.mu.Lock()
	if server.byID[session.info.ID] == session {
		delete(server.byID, session.info.ID)
		delete(server.byAlias, strings.ToLower(session.info.Alias))
	}
	server.mu.Unlock()
	session.close(errors.New("connector disconnected"))
}

func readAndValidateHello(ctx context.Context, connection *websocket.Conn) (wire.Hello, ed25519.PublicKey, error) {
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
	return hello, publicKey, nil
}

func readAndVerifyRegistration(ctx context.Context, connection *websocket.Conn, hello wire.Hello, publicKey ed25519.PublicKey, nonce []byte) error {
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

func (server *Server) writeProtocolError(connection *websocket.Conn, code, message string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = writeControl(ctx, connection, wire.MessageError, "", wire.ErrorPayload{Code: code, Message: message})
}

func readControl(ctx context.Context, connection *websocket.Conn) (wire.Envelope, error) {
	messageType, data, err := connection.Read(ctx)
	if err != nil {
		return wire.Envelope{}, err
	}
	if messageType != websocket.MessageText {
		return wire.Envelope{}, errors.New("expected a text control message")
	}
	return wire.DecodeEnvelope(data)
}

func writeControl(ctx context.Context, connection *websocket.Conn, messageType wire.MessageType, id string, payload any) error {
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
