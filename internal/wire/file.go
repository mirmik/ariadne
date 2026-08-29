package wire

import (
	"encoding/json"
	"errors"
	"fmt"
)

type FileFrameType byte

const (
	FileFrameData     FileFrameType = 1
	FileFrameComplete FileFrameType = 2
	FileFrameResult   FileFrameType = 3
)

func EncodeFileData(data []byte) ([]byte, error) {
	if len(data) > MaxStreamPayloadSize-1 {
		return nil, fmt.Errorf("file data exceeds %d bytes", MaxStreamPayloadSize-1)
	}
	frame := make([]byte, 1+len(data))
	frame[0] = byte(FileFrameData)
	copy(frame[1:], data)
	return frame, nil
}

func EncodeFileComplete(result FileTransferResult) ([]byte, error) {
	return encodeFileJSONFrame(FileFrameComplete, result)
}

func EncodeFileResult(result FileTransferResult) ([]byte, error) {
	return encodeFileJSONFrame(FileFrameResult, result)
}

func DecodeFileFrame(frame []byte) (FileFrameType, []byte, error) {
	if len(frame) == 0 {
		return 0, nil, errors.New("file frame is empty")
	}
	frameType := FileFrameType(frame[0])
	switch frameType {
	case FileFrameData, FileFrameComplete, FileFrameResult:
	default:
		return 0, nil, fmt.Errorf("unknown file frame type %d", frameType)
	}
	return frameType, frame[1:], nil
}

func DecodeFileTransferResult(payload []byte) (FileTransferResult, error) {
	var result FileTransferResult
	if err := json.Unmarshal(payload, &result); err != nil {
		return FileTransferResult{}, fmt.Errorf("decode file transfer result: %w", err)
	}
	return result, nil
}

func encodeFileJSONFrame(frameType FileFrameType, value FileTransferResult) ([]byte, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	if len(payload) > MaxStreamPayloadSize-1 {
		return nil, errors.New("file metadata exceeds stream payload limit")
	}
	return append([]byte{byte(frameType)}, payload...), nil
}
