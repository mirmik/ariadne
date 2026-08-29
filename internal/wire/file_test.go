package wire

import (
	"bytes"
	"testing"
)

func TestFileFramesRoundTrip(t *testing.T) {
	data := []byte{0, 1, 2, 0xff}
	frame, err := EncodeFileData(data)
	if err != nil {
		t.Fatal(err)
	}
	frameType, payload, err := DecodeFileFrame(frame)
	if err != nil || frameType != FileFrameData || !bytes.Equal(payload, data) {
		t.Fatalf("unexpected data frame: type=%d payload=%x err=%v", frameType, payload, err)
	}

	want := FileTransferResult{Size: 42, SHA256: "abcd"}
	frame, err = EncodeFileResult(want)
	if err != nil {
		t.Fatal(err)
	}
	frameType, payload, err = DecodeFileFrame(frame)
	if err != nil || frameType != FileFrameResult {
		t.Fatalf("unexpected result frame: type=%d err=%v", frameType, err)
	}
	got, err := DecodeFileTransferResult(payload)
	if err != nil || got != want {
		t.Fatalf("result changed: got=%#v err=%v", got, err)
	}
}
