package wire

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestEnvelopeRoundTrip(t *testing.T) {
	original := ExecRequest{Command: "uname -a | sed -n '1p'", Shell: "posix", Cwd: "/tmp", TimeoutMillis: 5000}
	encoded, err := MarshalEnvelope(MessageExecRequest, "request-1", original)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := DecodeEnvelope(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if envelope.Type != MessageExecRequest || envelope.ID != "request-1" {
		t.Fatalf("unexpected envelope: %#v", envelope)
	}
	decoded, err := DecodePayload[ExecRequest](envelope)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Command != original.Command || decoded.Shell != original.Shell || decoded.Cwd != original.Cwd || decoded.TimeoutMillis != original.TimeoutMillis {
		t.Fatalf("payload changed during round trip: %#v", decoded)
	}
}

func TestDecodeEnvelopeRejectsUnknownAndTrailingFields(t *testing.T) {
	tests := []string{
		`{"version":2,"type":"exec.request","unknown":true}`,
		`{"version":2,"type":"exec.request"} {}`,
		`{"version":99,"type":"exec.request"}`,
	}
	for _, input := range tests {
		if _, err := DecodeEnvelope([]byte(input)); err == nil {
			t.Fatalf("DecodeEnvelope accepted %s", input)
		}
	}
}

func TestStreamFrameRoundTrip(t *testing.T) {
	streamID := "00112233445566778899aabbccddeeff"
	payload := []byte("opaque ssh bytes")
	frame, err := EncodeStreamFrame(streamID, payload)
	if err != nil {
		t.Fatal(err)
	}
	decodedID, decodedPayload, err := DecodeStreamFrame(frame)
	if err != nil {
		t.Fatal(err)
	}
	if decodedID != streamID || !bytes.Equal(decodedPayload, payload) {
		t.Fatalf("decoded frame differs: id=%q payload=%q", decodedID, decodedPayload)
	}
}

func TestDecodePayloadRejectsUnknownFields(t *testing.T) {
	payload, err := json.Marshal(map[string]any{"argv": []string{"true"}, "surprise": true})
	if err != nil {
		t.Fatal(err)
	}
	_, err = DecodePayload[ExecRequest](Envelope{Version: ProtocolVersion, Type: MessageExecRequest, Payload: payload})
	if err == nil {
		t.Fatal("DecodePayload accepted an unknown field")
	}
}

func TestLegacyProtocolRejected(t *testing.T) {
	if _, err := DecodeEnvelope([]byte(`{"version":1,"type":"connector.hello","payload":{}}`)); err == nil || !strings.Contains(err.Error(), "unsupported protocol version 1") {
		t.Fatalf("legacy hello error = %v", err)
	}
	frame, err := EncodeStreamFrame("00112233445566778899aabbccddeeff", nil)
	if err != nil {
		t.Fatal(err)
	}
	frame[0] = 1
	if _, _, err := DecodeStreamFrame(frame); err == nil {
		t.Fatal("accepted legacy stream framing")
	}
}

func TestRegistrationCapabilitiesEncoding(t *testing.T) {
	nonce := bytes.Repeat([]byte{1}, 32)
	hello := Hello{}
	empty := RegistrationTranscript(nonce, hello)
	hello.Capabilities = []string{}
	if !bytes.Equal(empty, RegistrationTranscript(nonce, hello)) {
		t.Fatal("nil and empty capabilities differ")
	}
	hello.Capabilities = []string{"a", "bc"}
	original := RegistrationTranscript(nonce, hello)
	for _, changed := range [][]string{{"bc", "a"}, {"ab", "c"}, {"a"}, {"a", "bc", "d"}} {
		hello.Capabilities = changed
		if bytes.Equal(original, RegistrationTranscript(nonce, hello)) {
			t.Fatalf("capabilities not bound: %v", changed)
		}
	}
}

func TestMaximumExecResultFitsControlMessage(t *testing.T) {
	result := ExecResult{
		ExitCode: 0,
		Stdout:   bytes.Repeat([]byte{0xff}, MaxExecOutputBytes),
		Stderr:   bytes.Repeat([]byte{0x00}, MaxExecOutputBytes),
	}
	encoded, err := MarshalEnvelope(MessageExecResult, "request", result)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > MaxControlMessageSize {
		t.Fatalf("maximum exec result is %d bytes, control limit is %d", len(encoded), MaxControlMessageSize)
	}
	envelope, err := DecodeEnvelope(encoded)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodePayload[ExecResult](envelope)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded.Stdout, result.Stdout) || !bytes.Equal(decoded.Stderr, result.Stderr) {
		t.Fatal("binary command output changed during JSON round trip")
	}
}

func TestRegistrationTranscriptBindsSSHHostKey(t *testing.T) {
	hello := Hello{
		NodeID:           "n_example",
		Alias:            "phone",
		PublicKey:        "identity-key",
		SSHHostKey:       "ssh-host-key-a",
		Platform:         "linux",
		Architecture:     "arm64",
		ConnectorVersion: "dev",
	}
	nonce := bytes.Repeat([]byte{0x42}, 32)
	first := RegistrationTranscript(nonce, hello)
	hello.SSHHostKey = "ssh-host-key-b"
	second := RegistrationTranscript(nonce, hello)
	if bytes.Equal(first, second) {
		t.Fatal("registration transcript does not bind the embedded SSH host key")
	}
}
