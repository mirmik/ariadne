package connector

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/coder/websocket"
	"github.com/mirmik/ariadne/internal/wire"
)

type reviewFileConnection struct {
	path   string
	cancel context.CancelFunc
	bytes  int
}

func (*reviewFileConnection) Read(context.Context) (websocket.MessageType, []byte, error) {
	return 0, nil, errors.New("unused")
}
func (conn *reviewFileConnection) Write(_ context.Context, kind websocket.MessageType, data []byte) error {
	if kind == websocket.MessageText {
		envelope, err := wire.DecodeEnvelope(data)
		if err != nil {
			return err
		}
		if envelope.Type == wire.MessageStreamOpened {
			// The initial stat has succeeded with size 1. Grow the same inode
			// before the download loop reads it; only three bytes are used.
			file, err := os.OpenFile(conn.path, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				return err
			}
			_, writeErr := file.WriteString("bc")
			return errors.Join(writeErr, file.Close())
		}
		return nil
	}
	_, payload, err := wire.DecodeStreamFrame(data)
	if err != nil {
		return err
	}
	frameType, content, err := wire.DecodeFileFrame(payload)
	if err != nil {
		return err
	}
	if frameType == wire.FileFrameData {
		conn.bytes += len(content)
	}
	if frameType == wire.FileFrameResult {
		conn.cancel()
	}
	return nil
}
func (*reviewFileConnection) Ping(context.Context) error { return nil }
func (*reviewFileConnection) CloseNow()                  {}

func TestProtocolReviewDownloadEnforcesLimitWhileStreaming(t *testing.T) {
	path := filepath.Join(t.TempDir(), "growing-file")
	if err := os.WriteFile(path, []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conn := &reviewFileConnection{path: path, cancel: cancel}
	connector := &Connector{config: Config{MaxFileBytes: 1}}
	session := newSession(ctx, connector, conn)
	stream := newLocalStream(wire.StreamOpen{StreamID: "00112233445566778899aabbccddeeff", Protocol: "file-download", File: &wire.FileTransferOpen{Path: path}}, session, nil)
	defer stream.cancel()
	err := stream.runFileDownload()
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if conn.bytes > 1 {
		t.Fatalf("download transmitted %d bytes with MaxFileBytes=1", conn.bytes)
	}
}
