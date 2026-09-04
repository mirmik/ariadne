package pairing

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// Regression tests from the protocol review; see docs/protocol-review-2026-09-04.md.
func TestProtocolReviewPairingFinishRejectsExpiredWindow(t *testing.T) {
	var window Window
	opening, err := window.Open(time.Now(), time.Minute, 5)
	if err != nil {
		t.Fatal(err)
	}
	client, ke1, err := NewClient(opening.Code, "node-1")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Clear()
	ke2, server, err := window.Begin(time.Now(), "node-1", ke1)
	if err != nil {
		t.Fatal(err)
	}
	ke3, secret, err := client.Finish(ke2)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(secret)
	// Move only the deadline, avoiding a timing-sensitive sleep. This is the
	// state reached when KE1 arrived in time but KE3 arrived after expiry.
	window.mu.Lock()
	window.active.expiresAt = time.Now().Add(-time.Second)
	window.mu.Unlock()
	if _, err := server.Finish(ke3, testRelayPin); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired pairing Finish error = %v, want ErrExpired", err)
	}
}

func TestPairingConsumeDeadlineBoundary(t *testing.T) {
	deadline := time.Unix(12345, 0)
	for _, offset := range []time.Duration{-time.Nanosecond, 0, time.Nanosecond} {
		window := Window{generation: 1, active: &windowState{expiresAt: deadline}}
		err := window.consume(1, deadline.Add(offset))
		if offset < 0 && err != nil {
			t.Fatalf("before deadline: %v", err)
		}
		if offset >= 0 && !errors.Is(err, ErrExpired) {
			t.Fatalf("offset %v: %v", offset, err)
		}
		if window.active != nil {
			t.Fatal("consumed or expired window retained")
		}
	}
}

func TestProtocolReviewPairingReplacedWindowRejectsOldFinish(t *testing.T) {
	var window Window
	opening, err := window.Open(time.Now(), time.Minute, 5)
	if err != nil {
		t.Fatal(err)
	}
	client, ke1, err := NewClient(opening.Code, "node-1")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Clear()
	ke2, server, err := window.Begin(time.Now(), "node-1", ke1)
	if err != nil {
		t.Fatal(err)
	}
	ke3, secret, err := client.Finish(ke2)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(secret)
	if _, err := window.Open(time.Now(), time.Minute, 5); err != nil {
		t.Fatal(err)
	}
	if _, err := server.Finish(ke3, testRelayPin); !errors.Is(err, ErrConsumed) {
		t.Fatalf("old session completion = %v, want ErrConsumed", err)
	}
}

func TestProtocolReviewPairingConcurrentFinishHasOneWinner(t *testing.T) {
	var window Window
	opening, err := window.Open(time.Now(), time.Minute, 5)
	if err != nil {
		t.Fatal(err)
	}
	var sessions []*ServerSession
	var confirmations [][]byte
	for range 2 {
		client, ke1, err := NewClient(opening.Code, "node-1")
		if err != nil {
			t.Fatal(err)
		}
		defer client.Clear()
		ke2, server, err := window.Begin(time.Now(), "node-1", ke1)
		if err != nil {
			t.Fatal(err)
		}
		ke3, secret, err := client.Finish(ke2)
		if err != nil {
			t.Fatal(err)
		}
		clear(secret)
		sessions = append(sessions, server)
		confirmations = append(confirmations, ke3)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for i := range sessions {
		wg.Go(func() {
			<-start
			_, err := sessions[i].Finish(confirmations[i], testRelayPin)
			results <- err
		})
	}
	close(start)
	wg.Wait()
	close(results)
	winners := 0
	for err := range results {
		if err == nil {
			winners++
		} else if !errors.Is(err, ErrConsumed) {
			t.Fatal(err)
		}
	}
	if winners != 1 {
		t.Fatalf("winners = %d, want 1", winners)
	}
}

func TestProtocolReviewPairingKE3CannotCrossSessions(t *testing.T) {
	var window Window
	opening, err := window.Open(time.Now(), time.Minute, 5)
	if err != nil {
		t.Fatal(err)
	}
	client, ke1, err := NewClient(opening.Code, "node-1")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Clear()
	ke2, _, err := window.Begin(time.Now(), "node-1", ke1)
	if err != nil {
		t.Fatal(err)
	}
	ke3, secret, err := client.Finish(ke2)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(secret)
	_, other, err := window.Begin(time.Now(), "node-1", ke1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Finish(ke3, testRelayPin); err == nil {
		t.Fatal("KE3 from another handshake was accepted")
	}
}
