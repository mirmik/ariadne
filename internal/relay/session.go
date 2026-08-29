package relay

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/mirmik/ariadne/internal/messageconn"
	"github.com/mirmik/ariadne/internal/wire"
)

type nodeSession struct {
	server *Server
	conn   messageconn.Conn
	infoMu sync.RWMutex
	info   wire.NodeInfo

	done      chan struct{}
	closeOnce sync.Once
	closeMu   sync.RWMutex
	closeErr  error

	writeMu sync.Mutex

	pendingMu   sync.Mutex
	pending     map[string]chan wire.ExecResult
	pendingJobs map[string]chan wire.JobResponse

	streamsMu sync.RWMutex
	streams   map[string]*relayStream
	known     map[string]struct{}
}

func (session *nodeSession) nodeInfo() wire.NodeInfo {
	session.infoMu.RLock()
	defer session.infoMu.RUnlock()
	return session.info
}

func (session *nodeSession) setClaimedAlias(alias string) {
	session.infoMu.Lock()
	session.info.Alias = alias
	session.info.AliasClaimed = true
	session.infoMu.Unlock()
}

func newNodeSession(server *Server, connection messageconn.Conn, info wire.NodeInfo) *nodeSession {
	return &nodeSession{
		server:      server,
		conn:        connection,
		info:        info,
		done:        make(chan struct{}),
		pending:     make(map[string]chan wire.ExecResult),
		pendingJobs: make(map[string]chan wire.JobResponse),
		streams:     make(map[string]*relayStream),
		known:       make(map[string]struct{}),
	}
}

func (session *nodeSession) run() error {
	if session.server.config.PingInterval > 0 {
		go session.pingLoop()
	}
	for {
		messageType, data, err := session.conn.Read(session.server.context)
		if err != nil {
			return err
		}
		switch messageType {
		case websocket.MessageText:
			envelope, err := wire.DecodeEnvelope(data)
			if err != nil {
				return err
			}
			if err := session.handleControl(envelope); err != nil {
				return err
			}
		case websocket.MessageBinary:
			streamID, payload, err := wire.DecodeStreamFrame(data)
			if err != nil {
				return err
			}
			stream := session.stream(streamID)
			if stream == nil {
				if session.knownStream(streamID) {
					continue
				}
				return fmt.Errorf("connector sent data for unknown stream %s", streamID)
			}
			stream.deliver(payload)
		default:
			return fmt.Errorf("unsupported WebSocket message type %d", messageType)
		}
	}
}

func (session *nodeSession) handleControl(envelope wire.Envelope) error {
	switch envelope.Type {
	case wire.MessageExecResult:
		if envelope.ID == "" {
			return errors.New("exec result has no request ID")
		}
		result, err := wire.DecodePayload[wire.ExecResult](envelope)
		if err != nil {
			return err
		}
		session.pendingMu.Lock()
		resultChannel := session.pending[envelope.ID]
		if resultChannel != nil {
			delete(session.pending, envelope.ID)
		}
		session.pendingMu.Unlock()
		if resultChannel == nil {
			return fmt.Errorf("connector sent an unsolicited exec result for request %s", envelope.ID)
		}
		select {
		case resultChannel <- result:
		default:
		}
		return nil

	case wire.MessageJobResponse:
		if envelope.ID == "" {
			return errors.New("job response has no request ID")
		}
		response, err := wire.DecodePayload[wire.JobResponse](envelope)
		if err != nil {
			return err
		}
		session.pendingMu.Lock()
		responseChannel := session.pendingJobs[envelope.ID]
		if responseChannel != nil {
			delete(session.pendingJobs, envelope.ID)
		}
		session.pendingMu.Unlock()
		if responseChannel == nil {
			return fmt.Errorf("connector sent an unsolicited job response for request %s", envelope.ID)
		}
		select {
		case responseChannel <- response:
		default:
		}
		return nil

	case wire.MessageStreamOpened, wire.MessageStreamError, wire.MessageStreamClose:
		state, err := wire.DecodePayload[wire.StreamState](envelope)
		if err != nil {
			return err
		}
		stream := session.stream(state.StreamID)
		if stream == nil {
			if session.knownStream(state.StreamID) {
				return nil
			}
			return fmt.Errorf("connector sent state for unknown stream %s", state.StreamID)
		}
		switch envelope.Type {
		case wire.MessageStreamOpened:
			stream.markOpened()
		case wire.MessageStreamError:
			if state.Message == "" {
				state.Message = "connector could not open stream"
			}
			stream.finish(errors.New(state.Message))
		case wire.MessageStreamClose:
			stream.finish(io.EOF)
		}
		return nil

	case wire.MessageError:
		protocolError, err := wire.DecodePayload[wire.ErrorPayload](envelope)
		if err != nil {
			return err
		}
		return fmt.Errorf("connector protocol error %s: %s", protocolError.Code, protocolError.Message)

	default:
		return fmt.Errorf("connector sent unexpected message type %s", envelope.Type)
	}
}

func (session *nodeSession) exec(ctx context.Context, request wire.ExecRequest) (wire.ExecResult, error) {
	requestID, err := randomID()
	if err != nil {
		return wire.ExecResult{}, err
	}
	resultChannel := make(chan wire.ExecResult, 1)
	session.pendingMu.Lock()
	select {
	case <-session.done:
		session.pendingMu.Unlock()
		return wire.ExecResult{}, session.closedError()
	default:
	}
	session.pending[requestID] = resultChannel
	session.pendingMu.Unlock()
	defer func() {
		session.pendingMu.Lock()
		delete(session.pending, requestID)
		session.pendingMu.Unlock()
	}()

	if err := session.sendControl(wire.MessageExecRequest, requestID, request); err != nil {
		return wire.ExecResult{}, fmt.Errorf("send exec request: %w", err)
	}
	select {
	case result := <-resultChannel:
		return result, nil
	case <-session.done:
		return wire.ExecResult{}, session.closedError()
	case <-ctx.Done():
		cancelContext, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = session.sendControlWithContext(cancelContext, wire.MessageExecCancel, requestID, nil)
		return wire.ExecResult{}, ctx.Err()
	}
}

func (session *nodeSession) job(ctx context.Context, request wire.JobRequest) (wire.JobResponse, error) {
	requestID, err := randomID()
	if err != nil {
		return wire.JobResponse{}, err
	}
	responseChannel := make(chan wire.JobResponse, 1)
	session.pendingMu.Lock()
	select {
	case <-session.done:
		session.pendingMu.Unlock()
		return wire.JobResponse{}, session.closedError()
	default:
	}
	session.pendingJobs[requestID] = responseChannel
	session.pendingMu.Unlock()
	defer func() {
		session.pendingMu.Lock()
		delete(session.pendingJobs, requestID)
		session.pendingMu.Unlock()
	}()
	if err := session.sendControl(wire.MessageJobRequest, requestID, request); err != nil {
		return wire.JobResponse{}, fmt.Errorf("send job request: %w", err)
	}
	select {
	case response := <-responseChannel:
		if response.Error != "" {
			return response, errors.New(response.Error)
		}
		return response, nil
	case <-session.done:
		return wire.JobResponse{}, session.closedError()
	case <-ctx.Done():
		return wire.JobResponse{}, ctx.Err()
	}
}

func (session *nodeSession) sendControl(messageType wire.MessageType, id string, payload any) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return session.sendControlWithContext(ctx, messageType, id, payload)
}

func (session *nodeSession) sendControlWithContext(ctx context.Context, messageType wire.MessageType, id string, payload any) error {
	data, err := wire.MarshalEnvelope(messageType, id, payload)
	if err != nil {
		return err
	}
	return session.write(ctx, websocket.MessageText, data)
}

func (session *nodeSession) sendFrame(streamID string, payload []byte) error {
	frame, err := wire.EncodeStreamFrame(streamID, payload)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return session.write(ctx, websocket.MessageBinary, frame)
}

func (session *nodeSession) write(ctx context.Context, messageType websocket.MessageType, data []byte) error {
	select {
	case <-session.done:
		return session.closedError()
	default:
	}
	session.writeMu.Lock()
	defer session.writeMu.Unlock()
	return session.conn.Write(ctx, messageType, data)
}

func (session *nodeSession) addStream(stream *relayStream) error {
	session.streamsMu.Lock()
	defer session.streamsMu.Unlock()
	select {
	case <-session.done:
		return session.closedError()
	default:
	}
	if _, exists := session.streams[stream.id]; exists {
		return errors.New("stream ID collision")
	}
	session.streams[stream.id] = stream
	session.known[stream.id] = struct{}{}
	return nil
}

func (session *nodeSession) removeStream(stream *relayStream) {
	session.streamsMu.Lock()
	if session.streams[stream.id] == stream {
		delete(session.streams, stream.id)
	}
	session.streamsMu.Unlock()
}

func (session *nodeSession) stream(id string) *relayStream {
	session.streamsMu.RLock()
	defer session.streamsMu.RUnlock()
	return session.streams[id]
}

func (session *nodeSession) knownStream(id string) bool {
	session.streamsMu.RLock()
	defer session.streamsMu.RUnlock()
	_, known := session.known[id]
	return known
}

func (session *nodeSession) close(reason error) {
	session.closeOnce.Do(func() {
		if reason == nil {
			reason = errors.New("node connection closed")
		}
		session.closeMu.Lock()
		session.closeErr = reason
		session.closeMu.Unlock()
		close(session.done)
		session.conn.CloseNow()

		session.streamsMu.RLock()
		streams := make([]*relayStream, 0, len(session.streams))
		for _, stream := range session.streams {
			streams = append(streams, stream)
		}
		session.streamsMu.RUnlock()
		for _, stream := range streams {
			stream.finish(reason)
		}
	})
}

func (session *nodeSession) closedError() error {
	session.closeMu.RLock()
	defer session.closeMu.RUnlock()
	if session.closeErr != nil {
		return session.closeErr
	}
	return errors.New("node connection closed")
}

func (session *nodeSession) pingLoop() {
	ticker := time.NewTicker(session.server.config.PingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-session.done:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), session.server.config.PingTimeout)
			err := session.conn.Ping(ctx)
			cancel()
			if err != nil {
				session.close(fmt.Errorf("connector heartbeat failed: %w", err))
				return
			}
		}
	}
}

func randomID() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("create request ID: %w", err)
	}
	return hex.EncodeToString(raw), nil
}
