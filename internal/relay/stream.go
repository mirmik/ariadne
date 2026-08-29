package relay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/mirmik/ariadne/internal/wire"
)

const streamQueueDepth = 32

type relayStream struct {
	id      string
	session *nodeSession

	opened   chan error
	openOnce sync.Once
	data     chan []byte
	done     chan struct{}

	finishOnce sync.Once
	errMu      sync.RWMutex
	err        error
}

func newRelayStream(id string, session *nodeSession) *relayStream {
	return &relayStream{
		id:      id,
		session: session,
		opened:  make(chan error, 1),
		data:    make(chan []byte, streamQueueDepth),
		done:    make(chan struct{}),
	}
}

func (stream *relayStream) markOpened() {
	stream.openOnce.Do(func() {
		stream.opened <- nil
	})
}

func (stream *relayStream) waitUntilOpened(ctx context.Context) error {
	select {
	case err := <-stream.opened:
		return err
	case <-stream.done:
		return stream.finishedError()
	case <-ctx.Done():
		return ctx.Err()
	case <-stream.session.done:
		return stream.session.closedError()
	}
}

func (stream *relayStream) deliver(payload []byte) {
	copyOfPayload := append([]byte(nil), payload...)
	select {
	case stream.data <- copyOfPayload:
	case <-stream.done:
	default:
		stream.finish(errors.New("stream receiver is too slow"))
	}
}

func (stream *relayStream) finish(reason error) {
	stream.finishOnce.Do(func() {
		if reason == nil {
			reason = io.EOF
		}
		stream.errMu.Lock()
		stream.err = reason
		stream.errMu.Unlock()
		stream.openOnce.Do(func() {
			stream.opened <- reason
		})
		close(stream.done)
	})
}

func (stream *relayStream) finishedError() error {
	stream.errMu.RLock()
	defer stream.errMu.RUnlock()
	if stream.err != nil {
		return stream.err
	}
	return io.EOF
}

func (server *Server) handleStreamProxy(response http.ResponseWriter, request *http.Request, session *nodeSession, protocol string) {
	sshClientPublicKey := ""
	var fileOpen *wire.FileTransferOpen
	if protocol == "shell" {
		sshClientPublicKey = request.Header.Get(wire.HeaderSSHClientKey)
		if _, err := parseWireSSHKey(sshClientPublicKey); err != nil {
			writeAPIError(response, http.StatusBadRequest, "invalid SSH session public key: "+err.Error())
			return
		}
	} else if request.Header.Get(wire.HeaderSSHClientKey) != "" {
		writeAPIError(response, http.StatusBadRequest, "SSH session public key is only valid for shell streams")
		return
	}
	sessionInfo := session.nodeInfo()
	if protocol == "file-upload" || protocol == "file-download" {
		if !wire.HasCapability(sessionInfo.Capabilities, wire.CapabilityFileTransfer) {
			writeAPIError(response, http.StatusConflict, "connector does not support file transfer; update ariadne-connector")
			return
		}
		var err error
		fileOpen, err = parseFileStreamOpen(request, protocol)
		if err != nil {
			writeAPIError(response, http.StatusBadRequest, err.Error())
			return
		}
	}
	response.Header().Set(wire.HeaderNodeID, sessionInfo.ID)
	response.Header().Set(wire.HeaderSSHHostKey, sessionInfo.SSHHostKey)
	clientConnection, err := websocket.Accept(response, request, nil)
	if err != nil {
		server.logger.Warn("client stream WebSocket upgrade failed", "error", err)
		return
	}
	clientConnection.SetReadLimit(wire.MaxStreamPayloadSize)
	defer clientConnection.CloseNow()

	streamID, err := randomID()
	if err != nil {
		_ = clientConnection.Close(websocket.StatusInternalError, "could not allocate stream")
		return
	}
	stream := newRelayStream(streamID, session)
	if err := session.addStream(stream); err != nil {
		_ = clientConnection.Close(websocket.StatusGoingAway, "node disconnected")
		return
	}
	defer func() {
		stream.finish(io.EOF)
		session.removeStream(stream)
		closeContext, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = session.sendControlWithContext(closeContext, wire.MessageStreamClose, "", wire.StreamState{StreamID: stream.id})
	}()

	if err := session.sendControl(wire.MessageStreamOpen, "", wire.StreamOpen{
		StreamID:           stream.id,
		Protocol:           protocol,
		SSHClientPublicKey: sshClientPublicKey,
		File:               fileOpen,
	}); err != nil {
		_ = clientConnection.Close(websocket.StatusGoingAway, "node disconnected")
		return
	}
	openContext, cancelOpen := context.WithTimeout(server.context, server.config.StreamOpenTimeout)
	err = stream.waitUntilOpened(openContext)
	cancelOpen()
	if err != nil {
		_ = clientConnection.Close(websocket.StatusPolicyViolation, closeReason(err))
		return
	}

	server.logger.Debug("stream opened", "node_id", sessionInfo.ID, "stream_id", stream.id, "protocol", protocol)
	proxyContext, cancelProxy := context.WithCancel(server.context)
	defer cancelProxy()
	errorChannel := make(chan error, 3)
	go func() {
		errorChannel <- copyClientToNode(proxyContext, clientConnection, stream)
	}()
	go func() {
		errorChannel <- copyNodeToClient(proxyContext, clientConnection, stream)
	}()
	if server.config.PingInterval > 0 {
		go func() {
			errorChannel <- pingClientStream(proxyContext, clientConnection, server.config.PingInterval, server.config.PingTimeout)
		}()
	}

	err = <-errorChannel
	stream.finish(err)
	cancelProxy()
	if err != nil && !isNormalStreamEnd(err) {
		server.logger.Debug("stream closed with error", "node_id", sessionInfo.ID, "stream_id", stream.id, "error", err)
	}
}

func parseFileStreamOpen(request *http.Request, protocol string) (*wire.FileTransferOpen, error) {
	query := request.URL.Query()
	for key, values := range query {
		if key != "path" && key != "overwrite" && key != "mode" {
			return nil, fmt.Errorf("unknown file stream parameter %q", key)
		}
		if len(values) != 1 {
			return nil, fmt.Errorf("file stream parameter %q must appear once", key)
		}
	}
	path := query.Get("path")
	if path == "" || len(path) > 16<<10 {
		return nil, errors.New("file path must contain 1 to 16384 bytes")
	}
	open := &wire.FileTransferOpen{Path: path}
	if protocol == "file-download" {
		if query.Has("overwrite") || query.Has("mode") {
			return nil, errors.New("download stream accepts only path")
		}
		return open, nil
	}
	if query.Has("overwrite") {
		overwrite, err := strconv.ParseBool(query.Get("overwrite"))
		if err != nil {
			return nil, errors.New("overwrite must be true or false")
		}
		open.Overwrite = overwrite
	}
	if query.Has("mode") {
		mode, err := strconv.ParseUint(query.Get("mode"), 8, 32)
		if err != nil || mode&^0o777 != 0 {
			return nil, errors.New("mode must be an octal permission value")
		}
		open.Mode = uint32(mode)
	}
	return open, nil
}

func pingClientStream(ctx context.Context, connection *websocket.Conn, interval, timeout time.Duration) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			pingContext, cancel := context.WithTimeout(ctx, timeout)
			err := connection.Ping(pingContext)
			cancel()
			if err != nil {
				return err
			}
		}
	}
}

func copyClientToNode(ctx context.Context, clientConnection *websocket.Conn, stream *relayStream) error {
	for {
		messageType, payload, err := clientConnection.Read(ctx)
		if err != nil {
			return err
		}
		if messageType != websocket.MessageBinary {
			return errors.New("stream accepts binary WebSocket messages only")
		}
		if len(payload) > wire.MaxStreamPayloadSize {
			return fmt.Errorf("stream payload exceeds %d bytes", wire.MaxStreamPayloadSize)
		}
		if err := stream.session.sendFrame(stream.id, payload); err != nil {
			return err
		}
	}
}

func copyNodeToClient(ctx context.Context, clientConnection *websocket.Conn, stream *relayStream) error {
	for {
		select {
		case payload := <-stream.data:
			writeContext, cancel := context.WithTimeout(ctx, 30*time.Second)
			err := clientConnection.Write(writeContext, websocket.MessageBinary, payload)
			cancel()
			if err != nil {
				return err
			}
		case <-stream.done:
			return stream.finishedError()
		case <-stream.session.done:
			return stream.session.closedError()
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func isNormalStreamEnd(err error) bool {
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
		return true
	}
	status := websocket.CloseStatus(err)
	return status == websocket.StatusNormalClosure || status == websocket.StatusGoingAway || status == websocket.StatusNoStatusRcvd
}

func closeReason(err error) string {
	if err == nil {
		return "stream could not be opened"
	}
	message := err.Error()
	if len(message) > 120 {
		message = message[:120]
	}
	return message
}
