package sshtunnel

import (
	"context"
	"os"
	"os/exec"
	"reflect"
	"testing"
	"time"
)

func TestParseDestination(t *testing.T) {
	tests := []struct {
		input       string
		destination string
		port        string
	}{
		{input: "relay", destination: "relay"},
		{input: "breakout@relay", destination: "breakout@relay"},
		{input: "breakout@95.165.81.109:22061", destination: "breakout@95.165.81.109", port: "22061"},
		{input: "relay:22", destination: "relay", port: "22"},
		{input: "breakout@[2001:db8::1]:22061", destination: "breakout@2001:db8::1", port: "22061"},
		{input: "2001:db8::1", destination: "2001:db8::1"},
	}
	for _, test := range tests {
		destination, port, err := parseDestination(test.input)
		if err != nil {
			t.Errorf("parseDestination(%q): %v", test.input, err)
			continue
		}
		if destination != test.destination || port != test.port {
			t.Errorf("parseDestination(%q) = (%q, %q), want (%q, %q)", test.input, destination, port, test.destination, test.port)
		}
	}
}

func TestParseDestinationRejectsInvalidValues(t *testing.T) {
	for _, input := range []string{"", " user@host", "user@", "@host", "host:", "host:not-a-port", "host:0", "host:65536", "user@[2001:db8::1", "-oProxyCommand=bad", "-o@host"} {
		if _, _, err := parseDestination(input); err == nil {
			t.Errorf("parseDestination(%q) succeeded", input)
		}
	}
}

func TestSSHArgumentsIncludeParsedPort(t *testing.T) {
	got := sshArguments("breakout@95.165.81.109", "22061", "127.0.0.1:17471:127.0.0.1:47471")
	want := []string{"-N", "-T", "-n", "-o", "ExitOnForwardFailure=yes", "-p", "22061", "-L", "127.0.0.1:17471:127.0.0.1:47471", "breakout@95.165.81.109"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sshArguments() = %#v, want %#v", got, want)
	}
}

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

func TestTunnelCloseStopsSSHProcess(t *testing.T) {
	command := exec.Command(os.Args[0], "-test.run=TestTunnelCloseHelperProcess")
	command.Env = append(os.Environ(), "ARIADNE_TUNNEL_CLOSE_HELPER=1")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	tunnel := &Tunnel{command: command, done: make(chan struct{})}
	go func() {
		processErr := command.Wait()
		tunnel.errMu.Lock()
		tunnel.err = processErr
		tunnel.errMu.Unlock()
		close(tunnel.done)
	}()

	startedAt := time.Now()
	tunnel.Close()
	if elapsed := time.Since(startedAt); elapsed > 3*time.Second {
		t.Fatalf("Tunnel.Close took %s", elapsed)
	}
	select {
	case <-tunnel.Done():
	default:
		t.Fatal("Tunnel.Close returned before the SSH process stopped")
	}
}

func TestTunnelCloseHelperProcess(t *testing.T) {
	if os.Getenv("ARIADNE_TUNNEL_CLOSE_HELPER") != "1" {
		return
	}
	time.Sleep(time.Hour)
}
