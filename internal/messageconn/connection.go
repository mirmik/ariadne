package messageconn

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/coder/websocket"
)

const (
	frameText   byte = 1
	frameBinary byte = 2
	framePing   byte = 3
	framePong   byte = 4

	frameHeaderSize = 5
)

// Conn is the message boundary used by the Ariadne wire protocol. WebSocket
// and QUIC expose the same text/binary message semantics to the connector and
// relay state machines.
type Conn interface {
	Read(context.Context) (websocket.MessageType, []byte, error)
	Write(context.Context, websocket.MessageType, []byte) error
	Ping(context.Context) error
	CloseNow()
}

type WebSocket struct {
	Conn *websocket.Conn
}

func (connection WebSocket) Read(ctx context.Context) (websocket.MessageType, []byte, error) {
	return connection.Conn.Read(ctx)
}

func (connection WebSocket) Write(ctx context.Context, messageType websocket.MessageType, data []byte) error {
	return connection.Conn.Write(ctx, messageType, data)
}

func (connection WebSocket) Ping(ctx context.Context) error {
	return connection.Conn.Ping(ctx)
}

func (connection WebSocket) CloseNow() {
	connection.Conn.CloseNow()
}

// Framed carries WebSocket-like messages over one reliable byte stream. It is
// used for the QUIC control connection while preserving wire protocol v1.
type Framed struct {
	stream   io.ReadWriter
	closeFn  func()
	maxBytes uint32

	readMu  sync.Mutex
	writeMu sync.Mutex
	pingMu  sync.Mutex
	pongs   chan struct{}
	once    sync.Once
}

func NewFramed(stream io.ReadWriter, maxBytes uint32, closeFn func()) *Framed {
	return &Framed{
		stream:   stream,
		closeFn:  closeFn,
		maxBytes: maxBytes,
		pongs:    make(chan struct{}, 1),
	}
}

func (connection *Framed) Read(ctx context.Context) (websocket.MessageType, []byte, error) {
	connection.readMu.Lock()
	defer connection.readMu.Unlock()
	stop := context.AfterFunc(ctx, connection.CloseNow)
	defer stop()
	for {
		kind, payload, err := connection.readFrame()
		if err != nil {
			if ctx.Err() != nil {
				return 0, nil, ctx.Err()
			}
			return 0, nil, err
		}
		switch kind {
		case frameText:
			return websocket.MessageText, payload, nil
		case frameBinary:
			return websocket.MessageBinary, payload, nil
		case framePing:
			if len(payload) != 0 {
				return 0, nil, errors.New("QUIC ping frame has a payload")
			}
			if err := connection.writeFrame(framePong, nil); err != nil {
				return 0, nil, err
			}
		case framePong:
			if len(payload) != 0 {
				return 0, nil, errors.New("QUIC pong frame has a payload")
			}
			select {
			case connection.pongs <- struct{}{}:
			default:
			}
		default:
			return 0, nil, fmt.Errorf("unknown QUIC message frame type %d", kind)
		}
	}
}

func (connection *Framed) Write(ctx context.Context, messageType websocket.MessageType, data []byte) error {
	var kind byte
	switch messageType {
	case websocket.MessageText:
		kind = frameText
	case websocket.MessageBinary:
		kind = frameBinary
	default:
		return fmt.Errorf("unsupported message type %d", messageType)
	}
	stop := context.AfterFunc(ctx, connection.CloseNow)
	defer stop()
	if err := connection.writeFrame(kind, data); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	return nil
}

func (connection *Framed) Ping(ctx context.Context) error {
	connection.pingMu.Lock()
	defer connection.pingMu.Unlock()
	stop := context.AfterFunc(ctx, connection.CloseNow)
	defer stop()
	select {
	case <-connection.pongs:
	default:
	}
	if err := connection.writeFrame(framePing, nil); err != nil {
		return err
	}
	select {
	case <-connection.pongs:
		return nil
	case <-ctx.Done():
		connection.CloseNow()
		return ctx.Err()
	}
}

func (connection *Framed) CloseNow() {
	connection.once.Do(func() {
		if connection.closeFn != nil {
			connection.closeFn()
		}
	})
}

func (connection *Framed) readFrame() (byte, []byte, error) {
	header := make([]byte, frameHeaderSize)
	if _, err := io.ReadFull(connection.stream, header); err != nil {
		return 0, nil, err
	}
	length := binary.BigEndian.Uint32(header[1:])
	if length > connection.maxBytes {
		return 0, nil, fmt.Errorf("QUIC message exceeds %d-byte limit", connection.maxBytes)
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(connection.stream, payload); err != nil {
		return 0, nil, err
	}
	return header[0], payload, nil
}

func (connection *Framed) writeFrame(kind byte, payload []byte) error {
	if uint64(len(payload)) > uint64(connection.maxBytes) {
		return fmt.Errorf("QUIC message exceeds %d-byte limit", connection.maxBytes)
	}
	header := make([]byte, frameHeaderSize)
	header[0] = kind
	binary.BigEndian.PutUint32(header[1:], uint32(len(payload)))
	connection.writeMu.Lock()
	defer connection.writeMu.Unlock()
	if err := writeAll(connection.stream, header); err != nil {
		return err
	}
	return writeAll(connection.stream, payload)
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
