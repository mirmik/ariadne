package pairing

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/coder/websocket"
	"github.com/mirmik/ariadne/internal/identity"
	"github.com/mirmik/ariadne/internal/messageconn"
	"github.com/mirmik/ariadne/internal/wire"
)

func PairRelay(ctx context.Context, connection messageconn.Conn, code string, nodeIdentity *identity.Identity) (string, error) {
	if nodeIdentity == nil {
		return "", errors.New("connector identity is required for relay pairing")
	}
	nodeID := nodeIdentity.NodeID()
	session, ke1, err := NewClient(code, nodeID)
	if err != nil {
		return "", err
	}
	defer session.Clear()
	if err := writeControl(ctx, connection, wire.MessagePairRequest, wire.PairingRequest{
		NodeID:    nodeID,
		PublicKey: nodeIdentity.EncodedPublicKey(),
		KE1:       base64.RawStdEncoding.EncodeToString(ke1),
		Signature: nodeIdentity.Sign(wire.PairingIdentityTranscript(nodeID, nodeIdentity.EncodedPublicKey(), ke1)),
	}); err != nil {
		return "", fmt.Errorf("send pairing request: %w", err)
	}
	responseEnvelope, err := readControl(ctx, connection)
	if err != nil {
		return "", fmt.Errorf("read pairing response: %w", err)
	}
	if err := protocolError(responseEnvelope); err != nil {
		return "", err
	}
	if responseEnvelope.Type != wire.MessagePairResponse {
		return "", fmt.Errorf("expected %s, got %s", wire.MessagePairResponse, responseEnvelope.Type)
	}
	response, err := wire.DecodePayload[wire.PairingResponse](responseEnvelope)
	if err != nil {
		return "", err
	}
	ke2, err := base64.RawStdEncoding.DecodeString(response.KE2)
	if err != nil {
		return "", errors.New("relay sent invalid pairing KE2")
	}
	ke3, secret, err := session.Finish(ke2)
	if err != nil {
		return "", err
	}
	defer clear(secret)
	if err := writeControl(ctx, connection, wire.MessagePairConfirm, wire.PairingConfirm{
		KE3: base64.RawStdEncoding.EncodeToString(ke3),
	}); err != nil {
		return "", fmt.Errorf("send pairing confirmation: %w", err)
	}
	completeEnvelope, err := readControl(ctx, connection)
	if err != nil {
		return "", fmt.Errorf("read pairing completion: %w", err)
	}
	if err := protocolError(completeEnvelope); err != nil {
		return "", err
	}
	if completeEnvelope.Type != wire.MessagePairComplete {
		return "", fmt.Errorf("expected %s, got %s", wire.MessagePairComplete, completeEnvelope.Type)
	}
	complete, err := wire.DecodePayload[wire.PairingComplete](completeEnvelope)
	if err != nil {
		return "", err
	}
	if err := VerifyBinding(secret, nodeID, complete.RelayCertificatePin, complete.ConfirmationMAC); err != nil {
		return "", err
	}
	return complete.RelayCertificatePin, nil
}

func readControl(ctx context.Context, connection messageconn.Conn) (wire.Envelope, error) {
	messageType, data, err := connection.Read(ctx)
	if err != nil {
		return wire.Envelope{}, err
	}
	if messageType != websocket.MessageText {
		return wire.Envelope{}, errors.New("expected a text pairing message")
	}
	return wire.DecodeEnvelope(data)
}

func writeControl(ctx context.Context, connection messageconn.Conn, messageType wire.MessageType, payload any) error {
	data, err := wire.MarshalEnvelope(messageType, "", payload)
	if err != nil {
		return err
	}
	return connection.Write(ctx, websocket.MessageText, data)
}

func protocolError(envelope wire.Envelope) error {
	if envelope.Type != wire.MessageError {
		return nil
	}
	payload, err := wire.DecodePayload[wire.ErrorPayload](envelope)
	if err != nil {
		return err
	}
	return fmt.Errorf("relay pairing failed (%s): %s", payload.Code, payload.Message)
}
