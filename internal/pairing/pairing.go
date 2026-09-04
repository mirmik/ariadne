package pairing

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/bytemare/opaque"
	"github.com/mirmik/ariadne/internal/transport"
)

const (
	DefaultTTL      = 5 * time.Minute
	DefaultAttempts = 5

	protocolContext = "ariadne relay commissioning v1"
	clientIdentity  = "ariadne-connector"
	serverIdentity  = "ariadne-relay"
)

var (
	ErrUnavailable = errors.New("relay pairing is not open")
	ErrExpired     = errors.New("relay pairing window has expired")
	ErrAttempts    = errors.New("relay pairing attempt limit reached")
	ErrConsumed    = errors.New("relay pairing code was already consumed")
)

type Opening struct {
	Code              string
	ExpiresAt         time.Time
	RemainingAttempts int
}

type Window struct {
	mu         sync.Mutex
	generation uint64
	active     *windowState
}

type windowState struct {
	expiresAt         time.Time
	remainingAttempts int
	server            *opaque.Server
	record            *opaque.ClientRecord
}

type ServerSession struct {
	window     *Window
	generation uint64
	server     *opaque.Server
	output     *opaque.ServerOutput
	nodeID     string
}

type ClientSession struct {
	client *opaque.Client
	nodeID string
}

func (window *Window) Open(now time.Time, ttl time.Duration, attempts int) (Opening, error) {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	if attempts <= 0 {
		attempts = DefaultAttempts
	}
	code, err := randomCode()
	if err != nil {
		return Opening{}, err
	}
	server, record, err := registration([]byte(code))
	if err != nil {
		return Opening{}, fmt.Errorf("prepare pairing verifier: %w", err)
	}
	expiresAt := now.Add(ttl)
	window.mu.Lock()
	window.generation++
	window.active = &windowState{
		expiresAt:         expiresAt,
		remainingAttempts: attempts,
		server:            server,
		record:            record,
	}
	window.mu.Unlock()
	return Opening{Code: code, ExpiresAt: expiresAt, RemainingAttempts: attempts}, nil
}

func (window *Window) Begin(now time.Time, nodeID string, encodedKE1 []byte) ([]byte, *ServerSession, error) {
	if strings.TrimSpace(nodeID) == "" {
		return nil, nil, errors.New("pairing node identity is empty")
	}
	window.mu.Lock()
	if window.active == nil {
		window.mu.Unlock()
		return nil, nil, ErrUnavailable
	}
	if !now.Before(window.active.expiresAt) {
		window.active = nil
		window.mu.Unlock()
		return nil, nil, ErrExpired
	}
	if window.active.remainingAttempts <= 0 {
		window.active = nil
		window.mu.Unlock()
		return nil, nil, ErrAttempts
	}
	window.active.remainingAttempts--
	generation := window.generation
	server := window.active.server
	record := window.active.record
	window.mu.Unlock()

	ke1, err := server.Deserialize.KE1(encodedKE1)
	if err != nil {
		return nil, nil, fmt.Errorf("decode pairing KE1: %w", err)
	}
	ke2, output, err := server.GenerateKE2(ke1, record)
	if err != nil {
		return nil, nil, fmt.Errorf("create pairing KE2: %w", err)
	}
	return ke2.Serialize(), &ServerSession{
		window:     window,
		generation: generation,
		server:     server,
		output:     output,
		nodeID:     nodeID,
	}, nil
}

func (session *ServerSession) Finish(encodedKE3 []byte, relayPin string) (string, error) {
	defer clear(session.output.ClientMAC)
	defer clear(session.output.SessionSecret)
	pin, err := transport.NormalizeCertificatePin(relayPin)
	if err != nil {
		return "", fmt.Errorf("normalize relay certificate pin: %w", err)
	}
	ke3, err := session.server.Deserialize.KE3(encodedKE3)
	if err != nil {
		return "", fmt.Errorf("decode pairing KE3: %w", err)
	}
	if err := session.server.LoginFinish(ke3, session.output.ClientMAC); err != nil {
		return "", errors.New("pairing code authentication failed")
	}
	if err := session.window.consume(session.generation, time.Now()); err != nil {
		return "", err
	}
	return bindingMAC(session.output.SessionSecret, session.nodeID, pin), nil
}

func NewClient(code, nodeID string) (*ClientSession, []byte, error) {
	password, err := normalizeCode(code)
	if err != nil {
		return nil, nil, err
	}
	if strings.TrimSpace(nodeID) == "" {
		return nil, nil, errors.New("pairing node identity is empty")
	}
	configuration := configuration()
	client, err := configuration.Client()
	if err != nil {
		return nil, nil, err
	}
	passwordBytes := []byte(password)
	defer clear(passwordBytes)
	ke1, err := client.GenerateKE1(passwordBytes)
	if err != nil {
		client.ClearState()
		return nil, nil, fmt.Errorf("create pairing KE1: %w", err)
	}
	return &ClientSession{client: client, nodeID: nodeID}, ke1.Serialize(), nil
}

func (session *ClientSession) Finish(encodedKE2 []byte) ([]byte, []byte, error) {
	ke2, err := session.client.Deserialize.KE2(encodedKE2)
	if err != nil {
		return nil, nil, fmt.Errorf("decode pairing KE2: %w", err)
	}
	ke3, sessionSecret, _, err := session.client.GenerateKE3(
		ke2,
		[]byte(clientIdentity),
		[]byte(serverIdentity),
	)
	if err != nil {
		return nil, nil, errors.New("pairing code authentication failed")
	}
	return ke3.Serialize(), sessionSecret, nil
}

func (session *ClientSession) Clear() {
	if session != nil && session.client != nil {
		session.client.ClearState()
	}
}

func VerifyBinding(sessionSecret []byte, nodeID, relayPin, encodedMAC string) error {
	pin, err := transport.NormalizeCertificatePin(relayPin)
	if err != nil {
		return err
	}
	provided, err := base64.RawStdEncoding.DecodeString(encodedMAC)
	if err != nil {
		return errors.New("relay sent an invalid pairing confirmation")
	}
	expected, err := base64.RawStdEncoding.DecodeString(bindingMAC(sessionSecret, nodeID, pin))
	if err != nil {
		return err
	}
	if !hmac.Equal(provided, expected) {
		return errors.New("relay pairing confirmation is not authentic")
	}
	return nil
}

func (window *Window) consume(generation uint64, now time.Time) error {
	window.mu.Lock()
	defer window.mu.Unlock()
	if window.active == nil || window.generation != generation {
		return ErrConsumed
	}
	if !now.Before(window.active.expiresAt) {
		window.active = nil
		return ErrExpired
	}
	window.active = nil
	return nil
}

func configuration() *opaque.Configuration {
	configuration := opaque.DefaultConfiguration()
	configuration.Context = []byte(protocolContext)
	return configuration
}

func registration(password []byte) (*opaque.Server, *opaque.ClientRecord, error) {
	defer clear(password)
	configuration := configuration()
	server, err := configuration.Server()
	if err != nil {
		return nil, nil, err
	}
	privateKey, publicKey := configuration.KeyGen()
	if err := server.SetKeyMaterial(&opaque.ServerKeyMaterial{
		Identity:       []byte(serverIdentity),
		PrivateKey:     privateKey,
		PublicKeyBytes: publicKey.Encode(),
		OPRFGlobalSeed: configuration.GenerateOPRFSeed(),
	}); err != nil {
		return nil, nil, err
	}
	client, err := configuration.Client()
	if err != nil {
		return nil, nil, err
	}
	defer client.ClearState()
	request, err := client.RegistrationInit(password)
	if err != nil {
		return nil, nil, err
	}
	credentialID := opaque.RandomBytes(32)
	response, err := server.RegistrationResponse(request, credentialID, nil)
	if err != nil {
		return nil, nil, err
	}
	record, _, err := client.RegistrationFinalize(response, []byte(clientIdentity), []byte(serverIdentity))
	if err != nil {
		return nil, nil, err
	}
	return server, &opaque.ClientRecord{
		CredentialIdentifier: credentialID,
		ClientIdentity:       []byte(clientIdentity),
		RegistrationRecord:   record,
	}, nil
}

func bindingMAC(sessionSecret []byte, nodeID, relayPin string) string {
	mac := hmac.New(sha256.New, sessionSecret)
	_, _ = mac.Write([]byte(protocolContext))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(nodeID))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(relayPin))
	return base64.RawStdEncoding.EncodeToString(mac.Sum(nil))
}

func randomCode() (string, error) {
	maximum := big.NewInt(100_000_000)
	value, err := rand.Int(rand.Reader, maximum)
	if err != nil {
		return "", fmt.Errorf("generate pairing code: %w", err)
	}
	return fmt.Sprintf("%08d", value.Int64()), nil
}

func normalizeCode(code string) (string, error) {
	code = strings.ReplaceAll(strings.TrimSpace(code), "-", "")
	code = strings.ReplaceAll(code, " ", "")
	if len(code) != 8 {
		return "", errors.New("pairing code must contain exactly 8 digits")
	}
	for _, character := range code {
		if character < '0' || character > '9' {
			return "", errors.New("pairing code must contain exactly 8 digits")
		}
	}
	return code, nil
}
