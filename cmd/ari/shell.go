package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/coder/websocket"
	"github.com/mirmik/ariadne/internal/client"
	"golang.org/x/crypto/ssh"
	"golang.org/x/term"
)

func runShell(ctx context.Context, apiClient *client.Client, arguments []string) (int, error) {
	if len(arguments) != 1 {
		return 1, errors.New("usage: ari shell TARGET")
	}
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return 1, fmt.Errorf("generate one-time SSH session key: %w", err)
	}
	clientSigner, err := ssh.NewSignerFromKey(privateKey)
	if err != nil {
		return 1, fmt.Errorf("create SSH session signer: %w", err)
	}
	encodedClientKey := base64.RawStdEncoding.EncodeToString(clientSigner.PublicKey().Marshal())

	websocketConnection, peer, err := apiClient.DialShellStream(ctx, arguments[0], encodedClientKey)
	if err != nil {
		return 1, err
	}
	defer websocketConnection.CloseNow()
	hostKey, err := parseSSHHostKey(peer.SSHHostKey)
	if err != nil {
		return 1, fmt.Errorf("relay returned an invalid host key for node %s: %w", peer.NodeID, err)
	}

	networkConnection := websocket.NetConn(ctx, websocketConnection, websocket.MessageBinary)
	defer networkConnection.Close()
	sshConnection, channels, requests, err := ssh.NewClientConn(networkConnection, peer.NodeID, &ssh.ClientConfig{
		User:            "ariadne",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(clientSigner)},
		HostKeyCallback: ssh.FixedHostKey(hostKey),
		ClientVersion:   "SSH-2.0-Ariadne-CLI",
	})
	if err != nil {
		return 1, fmt.Errorf("SSH handshake with %s: %w", arguments[0], err)
	}
	sshClient := ssh.NewClient(sshConnection, channels, requests)
	defer sshClient.Close()
	session, err := sshClient.NewSession()
	if err != nil {
		return 1, fmt.Errorf("open SSH session: %w", err)
	}
	defer session.Close()
	session.Stdin = os.Stdin
	session.Stdout = os.Stdout
	session.Stderr = os.Stderr

	stdinFD := int(os.Stdin.Fd())
	interactive := term.IsTerminal(stdinFD)
	if interactive {
		width, height, err := term.GetSize(stdinFD)
		if err != nil {
			return 1, fmt.Errorf("read terminal size: %w", err)
		}
		terminalName := clientTerminalName()
		if err := session.RequestPty(terminalName, height, width, ssh.TerminalModes{
			ssh.ECHO:          1,
			ssh.TTY_OP_ISPEED: 38400,
			ssh.TTY_OP_OSPEED: 38400,
		}); err != nil {
			return 1, fmt.Errorf("request remote PTY: %w", err)
		}
		previousState, err := term.MakeRaw(stdinFD)
		if err != nil {
			return 1, fmt.Errorf("put terminal in raw mode: %w", err)
		}
		defer func() {
			_ = term.Restore(stdinFD, previousState)
		}()
		stopResize := forwardTerminalResize(ctx, session, stdinFD)
		defer stopResize()
	}

	if err := session.Shell(); err != nil {
		return 1, fmt.Errorf("start remote shell: %w", err)
	}
	waitErr := session.Wait()
	if waitErr == nil {
		return 0, nil
	}
	var exitError *ssh.ExitError
	if errors.As(waitErr, &exitError) {
		status := exitError.ExitStatus()
		if status >= 0 && status <= 255 {
			return status, nil
		}
		return 1, nil
	}
	if ctx.Err() != nil {
		return 1, ctx.Err()
	}
	return 1, fmt.Errorf("remote shell ended: %w", waitErr)
}

func parseSSHHostKey(encoded string) (ssh.PublicKey, error) {
	raw, err := base64.RawStdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, errors.New("host key is not valid base64")
	}
	key, err := ssh.ParsePublicKey(raw)
	if err != nil {
		return nil, err
	}
	if key.Type() != ssh.KeyAlgoED25519 {
		return nil, fmt.Errorf("host key type is %s, expected %s", key.Type(), ssh.KeyAlgoED25519)
	}
	return key, nil
}

func clientTerminalName() string {
	name := os.Getenv("TERM")
	if name == "" || len(name) > 128 || strings.ContainsRune(name, '\x00') {
		return "xterm-256color"
	}
	return name
}

func forwardTerminalResize(ctx context.Context, session *ssh.Session, stdinFD int) func() {
	resizeSignals := make(chan os.Signal, 1)
	signal.Notify(resizeSignals, syscall.SIGWINCH)
	resizeContext, cancel := context.WithCancel(ctx)
	go func() {
		for {
			select {
			case <-resizeSignals:
				width, height, err := term.GetSize(stdinFD)
				if err == nil {
					_ = session.WindowChange(height, width)
				}
			case <-resizeContext.Done():
				return
			}
		}
	}()
	return func() {
		signal.Stop(resizeSignals)
		cancel()
	}
}
