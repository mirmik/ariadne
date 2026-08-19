package connector

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"

	"github.com/mirmik/ariadne/internal/wire"
)

const localStreamQueueDepth = 32

type localStream struct {
	id      string
	session *session
	context context.Context
	cancel  context.CancelFunc
	inbound chan []byte
	done    chan struct{}

	connMu sync.Mutex
	conn   net.Conn
	once   sync.Once
}

func newLocalStream(id string, session *session) *localStream {
	streamContext, cancel := context.WithCancel(session.context)
	return &localStream{
		id:      id,
		session: session,
		context: streamContext,
		cancel:  cancel,
		inbound: make(chan []byte, localStreamQueueDepth),
		done:    make(chan struct{}),
	}
}

func (session *session) openStream(request wire.StreamOpen) error {
	if _, err := wire.EncodeStreamFrame(request.StreamID, nil); err != nil {
		return session.sendStreamError(request.StreamID, "invalid stream ID")
	}
	if request.Protocol != "ssh" {
		return session.sendStreamError(request.StreamID, "unsupported stream protocol")
	}

	stream := newLocalStream(request.StreamID, session)
	session.streamsMu.Lock()
	if _, exists := session.streams[request.StreamID]; exists {
		session.streamsMu.Unlock()
		stream.cancel()
		return session.sendStreamError(request.StreamID, "stream ID is already open")
	}
	if len(session.streams) >= session.connector.config.MaxStreams {
		session.streamsMu.Unlock()
		stream.cancel()
		return session.sendStreamError(request.StreamID, "connector is at stream capacity")
	}
	session.streams[request.StreamID] = stream
	session.streamsMu.Unlock()
	go stream.run()
	return nil
}

func (stream *localStream) run() {
	dialer := net.Dialer{Timeout: stream.session.connector.config.DialTimeout}
	connection, err := dialer.DialContext(stream.context, "tcp", stream.session.connector.config.SSHAddress)
	if err != nil {
		select {
		case <-stream.done:
			return
		default:
		}
		_ = stream.session.sendStreamError(stream.id, "connect to local sshd: "+err.Error())
		stream.finish(false)
		return
	}
	stream.connMu.Lock()
	stream.conn = connection
	stream.connMu.Unlock()
	select {
	case <-stream.done:
		_ = connection.Close()
		return
	default:
	}

	if err := stream.session.sendControl(wire.MessageStreamOpened, "", wire.StreamState{StreamID: stream.id}); err != nil {
		stream.finish(false)
		return
	}
	errorChannel := make(chan error, 2)
	go func() {
		errorChannel <- stream.readLocal()
	}()
	go func() {
		errorChannel <- stream.writeLocal()
	}()
	err = <-errorChannel
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, context.Canceled) {
		stream.session.connector.config.Logger.Debug("local SSH stream ended", "stream_id", stream.id, "error", err)
	}
	stream.finish(true)
}

func (stream *localStream) readLocal() error {
	buffer := make([]byte, 32<<10)
	for {
		count, err := stream.connection().Read(buffer)
		if count > 0 {
			if sendErr := stream.session.sendFrame(stream.id, buffer[:count]); sendErr != nil {
				return sendErr
			}
		}
		if err != nil {
			return err
		}
	}
}

func (stream *localStream) writeLocal() error {
	for {
		select {
		case payload := <-stream.inbound:
			if err := writeAll(stream.connection(), payload); err != nil {
				return err
			}
		case <-stream.done:
			return io.EOF
		case <-stream.context.Done():
			return stream.context.Err()
		}
	}
}

func (stream *localStream) deliver(payload []byte) {
	copyOfPayload := append([]byte(nil), payload...)
	select {
	case stream.inbound <- copyOfPayload:
	case <-stream.done:
	default:
		stream.session.connector.config.Logger.Warn("closing SSH stream because local receiver is too slow", "stream_id", stream.id)
		stream.finish(true)
	}
}

func (stream *localStream) finish(notifyRelay bool) {
	stream.once.Do(func() {
		stream.cancel()
		close(stream.done)
		stream.connMu.Lock()
		if stream.conn != nil {
			_ = stream.conn.Close()
		}
		stream.connMu.Unlock()
		stream.session.removeStream(stream)
		if notifyRelay {
			if err := stream.session.sendControl(wire.MessageStreamClose, "", wire.StreamState{StreamID: stream.id}); err != nil && stream.session.context.Err() == nil {
				stream.session.connector.config.Logger.Debug("could not notify relay about closed stream", "stream_id", stream.id, "error", err)
			}
		}
	})
}

func (stream *localStream) connection() net.Conn {
	stream.connMu.Lock()
	defer stream.connMu.Unlock()
	return stream.conn
}

func (session *session) sendStreamError(streamID, message string) error {
	if streamID == "" {
		return fmt.Errorf("stream error without stream ID: %s", message)
	}
	return session.sendControl(wire.MessageStreamError, "", wire.StreamState{StreamID: streamID, Message: message})
}

func writeAll(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		written, err := writer.Write(data)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrUnexpectedEOF
		}
		data = data[written:]
	}
	return nil
}
