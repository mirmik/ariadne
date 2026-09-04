package pairing

import (
	"errors"
	"testing"
	"time"
)

const testRelayPin = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestPairingEstablishesAuthenticatedRelayPinAndIsOneTime(t *testing.T) {
	now := time.Now()
	var window Window
	opening, err := window.Open(now, time.Minute, 3)
	if err != nil {
		t.Fatal(err)
	}
	client, ke1, err := NewClient(opening.Code, "node-1")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Clear()
	ke2, serverSession, err := window.Begin(now, "node-1", ke1)
	if err != nil {
		t.Fatal(err)
	}
	ke3, secret, err := client.Finish(ke2)
	if err != nil {
		t.Fatal(err)
	}
	confirmation, err := serverSession.Finish(ke3, testRelayPin)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyBinding(secret, "node-1", testRelayPin, confirmation); err != nil {
		t.Fatal(err)
	}
	if _, _, err := window.Begin(now, "node-1", ke1); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("consumed window error = %v, want ErrUnavailable", err)
	}
}

func TestPairingRejectsWrongCodeAndExpires(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	var window Window
	opening, err := window.Open(now, time.Minute, 2)
	if err != nil {
		t.Fatal(err)
	}
	wrong := "0000-0000"
	if wrong == opening.Code {
		wrong = "9999-9999"
	}
	client, ke1, err := NewClient(wrong, "node-1")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Clear()
	ke2, _, err := window.Begin(now, "node-1", ke1)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := client.Finish(ke2); err == nil {
		t.Fatal("wrong pairing code was accepted")
	}

	client2, ke1, err := NewClient(opening.Code, "node-1")
	if err != nil {
		t.Fatal(err)
	}
	defer client2.Clear()
	if _, _, err := window.Begin(now.Add(time.Minute), "node-1", ke1); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired window error = %v, want ErrExpired", err)
	}
}

func TestPairingBindingCoversNodeAndPin(t *testing.T) {
	secret := []byte("01234567890123456789012345678901")
	mac := bindingMAC(secret, "node-1", testRelayPin)
	if err := VerifyBinding(secret, "node-2", testRelayPin, mac); err == nil {
		t.Fatal("binding accepted for another node")
	}
	otherPin := "sha256:1123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if err := VerifyBinding(secret, "node-1", otherPin, mac); err == nil {
		t.Fatal("binding accepted for another relay pin")
	}
}

func TestPairingCodeHasNoSeparatorAndAcceptsOptionalHyphen(t *testing.T) {
	var window Window
	opening, err := window.Open(time.Now(), time.Minute, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(opening.Code) != 8 {
		t.Fatalf("pairing code %q has length %d, want 8", opening.Code, len(opening.Code))
	}
	for _, character := range opening.Code {
		if character < '0' || character > '9' {
			t.Fatalf("pairing code %q contains a non-digit", opening.Code)
		}
	}
	hyphenated := opening.Code[:4] + "-" + opening.Code[4:]
	client, _, err := NewClient(hyphenated, "node-1")
	if err != nil {
		t.Fatalf("hyphenated pairing code was rejected: %v", err)
	}
	client.Clear()
}

func TestPairingLimitsOnlineGuesses(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	var window Window
	opening, err := window.Open(now, time.Minute, 1)
	if err != nil {
		t.Fatal(err)
	}
	wrong := "0000-0000"
	if wrong == opening.Code {
		wrong = "9999-9999"
	}
	wrongClient, wrongKE1, err := NewClient(wrong, "node-1")
	if err != nil {
		t.Fatal(err)
	}
	defer wrongClient.Clear()
	if _, _, err := window.Begin(now, "node-1", wrongKE1); err != nil {
		t.Fatal(err)
	}
	correctClient, correctKE1, err := NewClient(opening.Code, "node-1")
	if err != nil {
		t.Fatal(err)
	}
	defer correctClient.Clear()
	if _, _, err := window.Begin(now, "node-1", correctKE1); !errors.Is(err, ErrAttempts) {
		t.Fatalf("attempt limit error=%v, want ErrAttempts", err)
	}
}
