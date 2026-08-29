package messageconn

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestFramedMessagesAndPing(t *testing.T) {
	leftSocket, rightSocket := net.Pipe()
	left := NewFramed(leftSocket, 1024, func() { _ = leftSocket.Close() })
	right := NewFramed(rightSocket, 1024, func() { _ = rightSocket.Close() })
	defer left.CloseNow()
	defer right.CloseNow()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	leftRead := make(chan error, 1)
	go func() {
		messageType, payload, err := left.Read(ctx)
		if err == nil && (messageType != websocket.MessageBinary || string(payload) != "reply") {
			t.Errorf("left received type=%v payload=%q", messageType, payload)
		}
		leftRead <- err
	}()
	rightRead := make(chan error, 1)
	go func() {
		messageType, payload, err := right.Read(ctx)
		if err == nil && (messageType != websocket.MessageText || string(payload) != "hello") {
			t.Errorf("right received type=%v payload=%q", messageType, payload)
		}
		rightRead <- err
	}()

	if err := left.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	if err := left.Write(ctx, websocket.MessageText, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := right.Write(ctx, websocket.MessageBinary, []byte("reply")); err != nil {
		t.Fatal(err)
	}
	if err := <-leftRead; err != nil {
		t.Fatal(err)
	}
	if err := <-rightRead; err != nil {
		t.Fatal(err)
	}
}

func TestFramedRejectsOversizedMessage(t *testing.T) {
	leftSocket, rightSocket := net.Pipe()
	left := NewFramed(leftSocket, 4, func() { _ = leftSocket.Close() })
	right := NewFramed(rightSocket, 4, func() { _ = rightSocket.Close() })
	defer left.CloseNow()
	defer right.CloseNow()
	if err := left.Write(context.Background(), websocket.MessageText, []byte("12345")); err == nil {
		t.Fatal("oversized message was accepted")
	}
}
