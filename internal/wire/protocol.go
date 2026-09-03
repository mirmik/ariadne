package wire

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"time"
)

const (
	ProtocolVersion       = 1
	MaxControlMessageSize = 4 << 20
	MaxExecOutputBytes    = 1 << 20
	MaxStreamPayloadSize  = 64 << 10
	StreamFrameHeaderSize = 18

	HeaderSSHClientKey = "X-Ariadne-SSH-Client-Key"
	HeaderNodeID       = "X-Ariadne-Node-ID"
	HeaderSSHHostKey   = "X-Ariadne-SSH-Host-Key"

	CapabilityFileTransfer   = "file-transfer.v1"
	CapabilityBackgroundJobs = "background-jobs.v1"
)

type MessageType string

const (
	MessageHello        MessageType = "connector.hello"
	MessagePairRequest  MessageType = "pairing.request"
	MessagePairResponse MessageType = "pairing.response"
	MessagePairConfirm  MessageType = "pairing.confirm"
	MessagePairComplete MessageType = "pairing.complete"
	MessageChallenge    MessageType = "relay.challenge"
	MessageRegister     MessageType = "connector.register"
	MessageRegistered   MessageType = "relay.registered"
	MessageExecRequest  MessageType = "exec.request"
	MessageExecResult   MessageType = "exec.result"
	MessageExecCancel   MessageType = "exec.cancel"
	MessageJobRequest   MessageType = "job.request"
	MessageJobResponse  MessageType = "job.response"
	MessageStreamOpen   MessageType = "stream.open"
	MessageStreamOpened MessageType = "stream.opened"
	MessageStreamError  MessageType = "stream.error"
	MessageStreamClose  MessageType = "stream.close"
	MessageError        MessageType = "error"
)

type Envelope struct {
	Version int             `json:"version"`
	Type    MessageType     `json:"type"`
	ID      string          `json:"id,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

type Hello struct {
	NodeID           string   `json:"node_id"`
	Alias            string   `json:"alias"`
	PublicKey        string   `json:"public_key"`
	SSHHostKey       string   `json:"ssh_host_key"`
	Platform         string   `json:"platform"`
	Architecture     string   `json:"architecture"`
	ConnectorVersion string   `json:"connector_version"`
	Capabilities     []string `json:"capabilities,omitempty"`
}

type PairingRequest struct {
	NodeID    string `json:"node_id"`
	PublicKey string `json:"public_key"`
	KE1       string `json:"ke1"`
	Signature string `json:"signature"`
}

type PairingResponse struct {
	KE2 string `json:"ke2"`
}

type PairingConfirm struct {
	KE3 string `json:"ke3"`
}

type PairingComplete struct {
	RelayCertificatePin string `json:"relay_certificate_pin"`
	ConfirmationMAC     string `json:"confirmation_mac"`
}

type PairingOpenResponse struct {
	Code              string    `json:"code"`
	ExpiresAt         time.Time `json:"expires_at"`
	RemainingAttempts int       `json:"remaining_attempts"`
}

type Challenge struct {
	Nonce string `json:"nonce"`
}

type Registration struct {
	Signature string `json:"signature"`
}

type Registered struct {
	Node NodeInfo `json:"node"`
}

type NodeInfo struct {
	ID               string    `json:"id"`
	Alias            string    `json:"alias"`
	AliasClaimed     bool      `json:"alias_claimed"`
	SSHHostKey       string    `json:"ssh_host_key"`
	Platform         string    `json:"platform"`
	Architecture     string    `json:"architecture"`
	ConnectorVersion string    `json:"connector_version"`
	Capabilities     []string  `json:"capabilities,omitempty"`
	ConnectedAt      time.Time `json:"connected_at"`
	Online           bool      `json:"online"`
}

type NodesResponse struct {
	Nodes []NodeInfo `json:"nodes"`
}

type ClaimRequest struct {
	Alias string `json:"alias"`
}

type ExecRequest struct {
	Command       string   `json:"command,omitempty"`
	Argv          []string `json:"argv,omitempty"`
	Shell         string   `json:"shell,omitempty"`
	Cwd           string   `json:"cwd,omitempty"`
	TimeoutMillis int64    `json:"timeout_ms,omitempty"`
}

type ExecResult struct {
	ExitCode        int    `json:"exit_code"`
	Shell           string `json:"shell,omitempty"`
	Stdout          []byte `json:"stdout,omitempty"`
	Stderr          []byte `json:"stderr,omitempty"`
	Error           string `json:"error,omitempty"`
	TimedOut        bool   `json:"timed_out,omitempty"`
	StdoutTruncated bool   `json:"stdout_truncated,omitempty"`
	StderrTruncated bool   `json:"stderr_truncated,omitempty"`
	DurationMillis  int64  `json:"duration_ms"`
}

type JobAction string

const (
	JobActionStart  JobAction = "start"
	JobActionList   JobAction = "list"
	JobActionStatus JobAction = "status"
	JobActionRead   JobAction = "read"
	JobActionCancel JobAction = "cancel"
	JobActionRemove JobAction = "remove"
)

type JobRequest struct {
	Action       JobAction    `json:"action"`
	JobID        string       `json:"job_id,omitempty"`
	Exec         *ExecRequest `json:"exec,omitempty"`
	Command      string       `json:"command,omitempty"`
	Shell        string       `json:"shell,omitempty"`
	StdoutOffset int64        `json:"stdout_offset,omitempty"`
	StderrOffset int64        `json:"stderr_offset,omitempty"`
	Limit        int          `json:"limit,omitempty"`
}

type JobResponse struct {
	Job    *JobInfo   `json:"job,omitempty"`
	Jobs   []JobInfo  `json:"jobs,omitempty"`
	Output *JobOutput `json:"output,omitempty"`
	Error  string     `json:"error,omitempty"`
}

type JobInfo struct {
	ID              string    `json:"id"`
	State           string    `json:"state"`
	Argv            []string  `json:"argv,omitempty"`
	Command         string    `json:"command,omitempty"`
	Shell           string    `json:"shell,omitempty"`
	Cwd             string    `json:"cwd,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	StartedAt       time.Time `json:"started_at"`
	FinishedAt      time.Time `json:"finished_at,omitempty"`
	ExitCode        int       `json:"exit_code"`
	Error           string    `json:"error,omitempty"`
	StdoutSize      int64     `json:"stdout_size"`
	StderrSize      int64     `json:"stderr_size"`
	StdoutTruncated bool      `json:"stdout_truncated,omitempty"`
	StderrTruncated bool      `json:"stderr_truncated,omitempty"`
}

type JobOutput struct {
	Stdout           []byte `json:"stdout,omitempty"`
	Stderr           []byte `json:"stderr,omitempty"`
	NextStdoutOffset int64  `json:"next_stdout_offset"`
	NextStderrOffset int64  `json:"next_stderr_offset"`
	StdoutEOF        bool   `json:"stdout_eof"`
	StderrEOF        bool   `json:"stderr_eof"`
}

type StreamOpen struct {
	StreamID           string            `json:"stream_id"`
	Protocol           string            `json:"protocol"`
	SSHClientPublicKey string            `json:"ssh_client_public_key,omitempty"`
	File               *FileTransferOpen `json:"file,omitempty"`
}

type FileTransferOpen struct {
	Path      string `json:"path"`
	Overwrite bool   `json:"overwrite,omitempty"`
	Mode      uint32 `json:"mode,omitempty"`
}

type FileTransferResult struct {
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256,omitempty"`
	Mode   uint32 `json:"mode,omitempty"`
	Error  string `json:"error,omitempty"`
}

type StreamState struct {
	StreamID string `json:"stream_id"`
	Message  string `json:"message,omitempty"`
}

type ErrorPayload struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type APIError struct {
	Error string `json:"error"`
}

var aliasPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

func ValidAlias(alias string) bool {
	return aliasPattern.MatchString(alias)
}

func HasCapability(capabilities []string, capability string) bool {
	for _, candidate := range capabilities {
		if candidate == capability {
			return true
		}
	}
	return false
}

func NewEnvelope(messageType MessageType, id string, payload any) (Envelope, error) {
	envelope := Envelope{
		Version: ProtocolVersion,
		Type:    messageType,
		ID:      id,
	}
	if payload == nil {
		return envelope, nil
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return Envelope{}, fmt.Errorf("encode %s payload: %w", messageType, err)
	}
	envelope.Payload = raw
	return envelope, nil
}

func MarshalEnvelope(messageType MessageType, id string, payload any) ([]byte, error) {
	envelope, err := NewEnvelope(messageType, id, payload)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope)
}

func DecodeEnvelope(data []byte) (Envelope, error) {
	if len(data) > MaxControlMessageSize {
		return Envelope{}, fmt.Errorf("control message exceeds %d bytes", MaxControlMessageSize)
	}
	var envelope Envelope
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		return Envelope{}, fmt.Errorf("decode envelope: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return Envelope{}, err
	}
	if envelope.Version != ProtocolVersion {
		return Envelope{}, fmt.Errorf("unsupported protocol version %d", envelope.Version)
	}
	if envelope.Type == "" {
		return Envelope{}, errors.New("message type is required")
	}
	return envelope, nil
}

func DecodePayload[T any](envelope Envelope) (T, error) {
	var payload T
	if len(envelope.Payload) == 0 {
		return payload, errors.New("message payload is required")
	}
	decoder := json.NewDecoder(bytes.NewReader(envelope.Payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		return payload, fmt.Errorf("decode %s payload: %w", envelope.Type, err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return payload, err
	}
	return payload, nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("unexpected data after JSON value")
	}
	return fmt.Errorf("decode trailing JSON: %w", err)
}

func RegistrationTranscript(nonce []byte, hello Hello) []byte {
	transcript := make([]byte, 0, 256)
	transcript = append(transcript, "ariadne/register/v1"...)
	transcript = appendField(transcript, nonce)
	transcript = appendField(transcript, []byte(hello.NodeID))
	transcript = appendField(transcript, []byte(hello.Alias))
	transcript = appendField(transcript, []byte(hello.PublicKey))
	transcript = appendField(transcript, []byte(hello.SSHHostKey))
	transcript = appendField(transcript, []byte(hello.Platform))
	transcript = appendField(transcript, []byte(hello.Architecture))
	transcript = appendField(transcript, []byte(hello.ConnectorVersion))
	return transcript
}

func PairingIdentityTranscript(nodeID, publicKey string, ke1 []byte) []byte {
	transcript := make([]byte, 0, 192)
	transcript = append(transcript, "ariadne/pairing-identity/v1"...)
	transcript = appendField(transcript, []byte(nodeID))
	transcript = appendField(transcript, []byte(publicKey))
	transcript = appendField(transcript, ke1)
	return transcript
}

func appendField(destination, value []byte) []byte {
	destination = binary.BigEndian.AppendUint32(destination, uint32(len(value)))
	return append(destination, value...)
}

func EncodeStreamFrame(streamID string, payload []byte) ([]byte, error) {
	if len(payload) > MaxStreamPayloadSize {
		return nil, fmt.Errorf("stream payload exceeds %d bytes", MaxStreamPayloadSize)
	}
	rawID, err := hex.DecodeString(streamID)
	if err != nil || len(rawID) != 16 {
		return nil, errors.New("stream ID must be 16 bytes encoded as hexadecimal")
	}
	frame := make([]byte, StreamFrameHeaderSize+len(payload))
	frame[0] = ProtocolVersion
	frame[1] = 0
	copy(frame[2:StreamFrameHeaderSize], rawID)
	copy(frame[StreamFrameHeaderSize:], payload)
	return frame, nil
}

func DecodeStreamFrame(frame []byte) (string, []byte, error) {
	if len(frame) < StreamFrameHeaderSize {
		return "", nil, errors.New("stream frame is shorter than its header")
	}
	if int(frame[0]) != ProtocolVersion {
		return "", nil, fmt.Errorf("unsupported stream frame version %d", frame[0])
	}
	if frame[1] != 0 {
		return "", nil, fmt.Errorf("unsupported stream frame flags 0x%x", frame[1])
	}
	payload := frame[StreamFrameHeaderSize:]
	if len(payload) > MaxStreamPayloadSize {
		return "", nil, fmt.Errorf("stream payload exceeds %d bytes", MaxStreamPayloadSize)
	}
	return hex.EncodeToString(frame[2:StreamFrameHeaderSize]), payload, nil
}
