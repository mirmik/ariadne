package connector

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/mirmik/ariadne/internal/messageconn"
	"github.com/mirmik/ariadne/internal/wire"
)

type session struct {
	connector *Connector
	conn      messageconn.Conn
	context   context.Context
	cancel    context.CancelFunc
	lifecycle context.Context

	writeMu sync.Mutex

	execSlots chan struct{}
	execMu    sync.Mutex
	running   map[string]context.CancelFunc

	streamsMu sync.RWMutex
	streams   map[string]*localStream
}

func newSession(parent context.Context, connector *Connector, connection messageconn.Conn) *session {
	sessionContext, cancel := context.WithCancel(parent)
	return &session{
		connector: connector,
		conn:      connection,
		context:   sessionContext,
		cancel:    cancel,
		lifecycle: parent,
		execSlots: make(chan struct{}, connector.config.MaxConcurrentExec),
		running:   make(map[string]context.CancelFunc),
		streams:   make(map[string]*localStream),
	}
}

func (session *session) run() error {
	defer session.close()
	for {
		messageType, data, err := session.conn.Read(session.context)
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
			if stream := session.stream(streamID); stream != nil {
				stream.deliver(payload)
			}
		default:
			return fmt.Errorf("unsupported WebSocket message type %d", messageType)
		}
	}
}

func (session *session) handleControl(envelope wire.Envelope) error {
	switch envelope.Type {
	case wire.MessageExecRequest:
		request, err := wire.DecodePayload[wire.ExecRequest](envelope)
		if err != nil {
			return err
		}
		return session.startExec(envelope.ID, request)

	case wire.MessageExecCancel:
		if envelope.ID == "" {
			return errors.New("exec cancellation has no request ID")
		}
		session.execMu.Lock()
		cancel := session.running[envelope.ID]
		session.execMu.Unlock()
		if cancel != nil {
			cancel()
		}
		return nil

	case wire.MessageJobRequest:
		if envelope.ID == "" {
			return errors.New("job request has no request ID")
		}
		request, err := wire.DecodePayload[wire.JobRequest](envelope)
		if err != nil {
			return err
		}
		response := session.connector.jobs.Handle(session.lifecycle, request)
		return session.sendControl(wire.MessageJobResponse, envelope.ID, response)

	case wire.MessageStreamOpen:
		openRequest, err := wire.DecodePayload[wire.StreamOpen](envelope)
		if err != nil {
			return err
		}
		return session.openStream(openRequest)

	case wire.MessageStreamClose:
		state, err := wire.DecodePayload[wire.StreamState](envelope)
		if err != nil {
			return err
		}
		if stream := session.stream(state.StreamID); stream != nil {
			stream.finish(false)
		}
		return nil

	case wire.MessageError:
		protocolError, err := wire.DecodePayload[wire.ErrorPayload](envelope)
		if err != nil {
			return err
		}
		return fmt.Errorf("relay protocol error %s: %s", protocolError.Code, protocolError.Message)

	default:
		return fmt.Errorf("relay sent unexpected message type %s", envelope.Type)
	}
}

func (session *session) startExec(requestID string, request wire.ExecRequest) error {
	if requestID == "" {
		return errors.New("exec request has no request ID")
	}
	if len(request.Argv) == 0 || request.Argv[0] == "" {
		return session.sendExecResult(requestID, wire.ExecResult{ExitCode: -1, Error: "argv must contain a non-empty executable"})
	}
	if request.TimeoutMillis <= 0 || request.TimeoutMillis > session.connector.config.MaxExecTimeout.Milliseconds() {
		return session.sendExecResult(requestID, wire.ExecResult{
			ExitCode: -1,
			Error:    fmt.Sprintf("timeout must be between 1ms and %s", session.connector.config.MaxExecTimeout),
		})
	}
	timeout := time.Duration(request.TimeoutMillis) * time.Millisecond
	select {
	case session.execSlots <- struct{}{}:
	default:
		return session.sendExecResult(requestID, wire.ExecResult{ExitCode: -1, Error: "connector is at exec capacity"})
	}

	execContext, cancel := context.WithTimeout(session.context, timeout)
	session.execMu.Lock()
	if _, exists := session.running[requestID]; exists {
		session.execMu.Unlock()
		cancel()
		<-session.execSlots
		return errors.New("duplicate exec request ID")
	}
	session.running[requestID] = cancel
	session.execMu.Unlock()

	go func() {
		defer func() {
			cancel()
			session.execMu.Lock()
			delete(session.running, requestID)
			session.execMu.Unlock()
			<-session.execSlots
		}()
		result := session.connector.executor.Execute(execContext, request)
		if err := session.sendExecResult(requestID, result); err != nil && session.context.Err() == nil {
			session.connector.config.Logger.Debug("could not send exec result", "request_id", requestID, "error", err)
		}
	}()
	return nil
}

func (session *session) sendExecResult(requestID string, result wire.ExecResult) error {
	return session.sendControl(wire.MessageExecResult, requestID, result)
}

func (session *session) sendControl(messageType wire.MessageType, id string, payload any) error {
	data, err := wire.MarshalEnvelope(messageType, id, payload)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return session.write(ctx, websocket.MessageText, data)
}

func (session *session) sendFrame(streamID string, payload []byte) error {
	frame, err := wire.EncodeStreamFrame(streamID, payload)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return session.write(ctx, websocket.MessageBinary, frame)
}

func (session *session) write(ctx context.Context, messageType websocket.MessageType, data []byte) error {
	if session.context.Err() != nil {
		return session.context.Err()
	}
	session.writeMu.Lock()
	defer session.writeMu.Unlock()
	return session.conn.Write(ctx, messageType, data)
}

func (session *session) stream(id string) *localStream {
	session.streamsMu.RLock()
	defer session.streamsMu.RUnlock()
	return session.streams[id]
}

func (session *session) removeStream(stream *localStream) {
	session.streamsMu.Lock()
	if session.streams[stream.id] == stream {
		delete(session.streams, stream.id)
	}
	session.streamsMu.Unlock()
}

func (session *session) close() {
	session.cancel()
	session.execMu.Lock()
	for _, cancel := range session.running {
		cancel()
	}
	session.execMu.Unlock()

	session.streamsMu.RLock()
	streams := make([]*localStream, 0, len(session.streams))
	for _, stream := range session.streams {
		streams = append(streams, stream)
	}
	session.streamsMu.RUnlock()
	for _, stream := range streams {
		stream.finish(false)
	}
	session.conn.CloseNow()
}
