package sshtunnel

import (
	"context"
	"testing"
)

func TestStartRejectsNonLoopbackRemote(t *testing.T) {
	_, err := Start(context.Background(), Config{
		Destination:   "breakglass@example",
		RemoteAddress: "example.com:47471",
	})
	if err == nil {
		t.Fatal("SSH tunnel accepted a non-loopback remote destination")
	}
}

func TestStartRequiresDestination(t *testing.T) {
	_, err := Start(context.Background(), Config{RemoteAddress: "127.0.0.1:47471"})
	if err == nil {
		t.Fatal("SSH tunnel accepted an empty SSH destination")
	}
}
