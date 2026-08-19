package connector

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/mirmik/ariadne/internal/wire"
	"golang.org/x/crypto/ssh"
)

const localStreamQueueDepth = 32

type localStream struct {
	id            string
	protocol      string
	authorizedKey ssh.PublicKey
	session       *session
	context       context.Context
	cancel        context.CancelFunc
	inbound       chan []byte
	done          chan struct{}

	connMu sync.Mutex
	conn   net.Conn
	once   sync.Once

	readMu     sync.Mutex
	readBuffer []byte
}

func newLocalStream(request wire.StreamOpen, session *session, authorizedKey ssh.PublicKey) *localStream {
	streamContext, cancel := context.WithCancel(session.context)
	return &localStream{
		id:            request.StreamID,
		protocol:      request.Protocol,
		authorizedKey: authorizedKey,
		session:       session,
		context:       streamContext,
		cancel:        cancel,
		inbound:       make(chan []byte, localStreamQueueDepth),
		done:          make(chan struct{}),
	}
}

func (session *session) openStream(request wire.StreamOpen) error {
	if _, err := wire.EncodeStreamFrame(request.StreamID, nil); err != nil {
		return session.sendStreamError(request.StreamID, "invalid stream ID")
	}
	var authorizedKey ssh.PublicKey
	switch request.Protocol {
	case "ssh":
		if request.SSHClientPublicKey != "" {
			return session.sendStreamError(request.StreamID, "external SSH streams do not accept a session key")
		}
	case "shell":
		var err error
		authorizedKey, err = parseSSHStreamKey(request.SSHClientPublicKey)
		if err != nil {
			return session.sendStreamError(request.StreamID, err.Error())
		}
	default:
		return session.sendStreamError(request.StreamID, "unsupported stream protocol")
	}

	stream := newLocalStream(request, session, authorizedKey)
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
	var err error
	switch stream.protocol {
	case "ssh":
		err = stream.runExternalSSH()
	case "shell":
		err = stream.runEmbeddedShell()
	default:
		err = errors.New("unsupported stream protocol")
	}
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, context.Canceled) {
		stream.session.connector.config.Logger.Debug("local stream ended", "stream_id", stream.id, "protocol", stream.protocol, "error", err)
	}
	stream.finish(true)
}

func (stream *localStream) runExternalSSH() error {
	dialer := net.Dialer{Timeout: stream.session.connector.config.DialTimeout}
	connection, err := dialer.DialContext(stream.context, "tcp", stream.session.connector.config.SSHAddress)
	if err != nil {
		select {
		case <-stream.done:
			return io.EOF
		default:
		}
		_ = stream.session.sendStreamError(stream.id, "connect to local sshd: "+err.Error())
		return err
	}
	stream.connMu.Lock()
	stream.conn = connection
	stream.connMu.Unlock()
	select {
	case <-stream.done:
		_ = connection.Close()
		return io.EOF
	default:
	}

	if err := stream.session.sendControl(wire.MessageStreamOpened, "", wire.StreamState{StreamID: stream.id}); err != nil {
		return err
	}
	errorChannel := make(chan error, 2)
	go func() {
		errorChannel <- stream.readLocal()
	}()
	go func() {
		errorChannel <- stream.writeLocal()
	}()
	return <-errorChannel
}

func (stream *localStream) runEmbeddedShell() error {
	if err := stream.session.sendControl(wire.MessageStreamOpened, "", wire.StreamState{StreamID: stream.id}); err != nil {
		return err
	}
	return stream.session.connector.sshServer.serve(stream.context, &streamConnection{stream: stream}, stream.authorizedKey)
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

func parseSSHStreamKey(encoded string) (ssh.PublicKey, error) {
	if encoded == "" {
		return nil, errors.New("embedded SSH stream requires a session public key")
	}
	if len(encoded) > 1024 {
		return nil, errors.New("SSH session public key is too large")
	}
	raw, err := base64.RawStdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, errors.New("SSH session public key is not valid base64")
	}
	key, err := ssh.ParsePublicKey(raw)
	if err != nil || key.Type() != ssh.KeyAlgoED25519 {
		return nil, errors.New("SSH session public key must be Ed25519")
	}
	return key, nil
}

type streamConnection struct {
	stream *localStream
}

func (connection *streamConnection) Read(destination []byte) (int, error) {
	if len(destination) == 0 {
		return 0, nil
	}
	stream := connection.stream
	stream.readMu.Lock()
	defer stream.readMu.Unlock()
	for len(stream.readBuffer) == 0 {
		select {
		case payload := <-stream.inbound:
			stream.readBuffer = payload
		case <-stream.done:
			return 0, io.EOF
		case <-stream.context.Done():
			return 0, stream.context.Err()
		}
	}
	count := copy(destination, stream.readBuffer)
	stream.readBuffer = stream.readBuffer[count:]
	return count, nil
}

func (connection *streamConnection) Write(data []byte) (int, error) {
	written := 0
	for written < len(data) {
		end := written + wire.MaxStreamPayloadSize
		if end > len(data) {
			end = len(data)
		}
		if err := connection.stream.session.sendFrame(connection.stream.id, data[written:end]); err != nil {
			return written, err
		}
		written = end
	}
	return written, nil
}

func (connection *streamConnection) Close() error {
	connection.stream.finish(true)
	return nil
}

func (connection *streamConnection) LocalAddr() net.Addr {
	return streamAddress("ariadne-connector")
}

func (connection *streamConnection) RemoteAddr() net.Addr {
	return streamAddress("ariadne-client")
}

func (connection *streamConnection) SetDeadline(time.Time) error      { return nil }
func (connection *streamConnection) SetReadDeadline(time.Time) error  { return nil }
func (connection *streamConnection) SetWriteDeadline(time.Time) error { return nil }

type streamAddress string

func (address streamAddress) Network() string { return "ariadne" }
func (address streamAddress) String() string  { return string(address) }

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
