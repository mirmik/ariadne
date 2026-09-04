package connector

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/mirmik/ariadne/internal/filetransfer"
	"github.com/mirmik/ariadne/internal/wire"
)

func (stream *localStream) runFileUpload() error {
	metadata := stream.file
	writer, err := filetransfer.NewAtomicWriter(metadata.Path, metadata.Overwrite, os.FileMode(metadata.Mode))
	if err != nil {
		_ = stream.session.sendStreamError(stream.id, err.Error())
		return err
	}
	defer writer.Abort()
	if err := stream.sendOpened(); err != nil {
		return err
	}

	hash := sha256.New()
	var size int64
	for {
		select {
		case payload := <-stream.inbound:
			frameType, content, decodeErr := wire.DecodeFileFrame(payload)
			if decodeErr != nil {
				return stream.finishFileTransfer(wire.FileTransferResult{Size: size, Error: decodeErr.Error()})
			}
			switch frameType {
			case wire.FileFrameData:
				size += int64(len(content))
				if size > stream.session.connector.config.MaxFileBytes {
					return stream.finishFileTransfer(wire.FileTransferResult{Size: size, Error: "file exceeds connector size limit"})
				}
				if err := writeAll(writer, content); err != nil {
					return stream.finishFileTransfer(wire.FileTransferResult{Size: size, Error: "write upload: " + err.Error()})
				}
				_, _ = hash.Write(content)
			case wire.FileFrameComplete:
				expected, err := wire.DecodeFileTransferResult(content)
				if err != nil {
					return stream.finishFileTransfer(wire.FileTransferResult{Size: size, Error: err.Error()})
				}
				actualHash := hex.EncodeToString(hash.Sum(nil))
				if expected.Size != size || !strings.EqualFold(expected.SHA256, actualHash) {
					return stream.finishFileTransfer(wire.FileTransferResult{Size: size, SHA256: actualHash, Error: "upload size or SHA-256 mismatch"})
				}
				if err := writer.Commit(); err != nil {
					return stream.finishFileTransfer(wire.FileTransferResult{Size: size, SHA256: actualHash, Error: err.Error()})
				}
				return stream.finishFileTransfer(wire.FileTransferResult{Size: size, SHA256: actualHash})
			default:
				return stream.finishFileTransfer(wire.FileTransferResult{Size: size, Error: fmt.Sprintf("unexpected upload frame type %d", frameType)})
			}
		case <-stream.done:
			return io.EOF
		case <-stream.context.Done():
			return stream.context.Err()
		}
	}
}

func (stream *localStream) runFileDownload() error {
	file, err := os.Open(stream.file.Path)
	if err != nil {
		_ = stream.session.sendStreamError(stream.id, "open download: "+err.Error())
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		_ = stream.session.sendStreamError(stream.id, "stat download: "+err.Error())
		return err
	}
	if !info.Mode().IsRegular() {
		err = errors.New("download path is not a regular file")
		_ = stream.session.sendStreamError(stream.id, err.Error())
		return err
	}
	if info.Size() > stream.session.connector.config.MaxFileBytes {
		err = errors.New("file exceeds connector size limit")
		_ = stream.session.sendStreamError(stream.id, err.Error())
		return err
	}
	if err := stream.sendOpened(); err != nil {
		return err
	}

	hash := sha256.New()
	buffer := make([]byte, 32<<10)
	var size int64
	for {
		count, readErr := file.Read(buffer)
		if count > 0 {
			if int64(count) > stream.session.connector.config.MaxFileBytes-size {
				return stream.finishFileTransfer(wire.FileTransferResult{Size: size, Error: "file exceeds connector size limit"})
			}
			size += int64(count)
			_, _ = hash.Write(buffer[:count])
			frame, encodeErr := wire.EncodeFileData(buffer[:count])
			if encodeErr != nil {
				return encodeErr
			}
			if err := stream.session.sendFrame(stream.id, frame); err != nil {
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			return stream.finishFileTransfer(wire.FileTransferResult{
				Size:   size,
				SHA256: hex.EncodeToString(hash.Sum(nil)),
				Mode:   uint32(info.Mode().Perm()),
			})
		}
		if readErr != nil {
			return stream.finishFileTransfer(wire.FileTransferResult{Size: size, Error: "read download: " + readErr.Error()})
		}
	}
}

func (stream *localStream) sendOpened() error {
	return stream.session.sendControl(wire.MessageStreamOpened, "", wire.StreamState{StreamID: stream.id})
}

func (stream *localStream) finishFileTransfer(result wire.FileTransferResult) error {
	frame, err := wire.EncodeFileResult(result)
	if err != nil {
		return err
	}
	if err := stream.session.sendFrame(stream.id, frame); err != nil {
		return err
	}
	select {
	case <-stream.done:
		return io.EOF
	case <-stream.context.Done():
		return stream.context.Err()
	}
}
