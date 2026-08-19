package connector

import (
	"bytes"
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/crypto/ssh"
)

const (
	sshSessionUser        = "ariadne"
	maxTerminalNameLength = 128
)

type embeddedSSHServer struct {
	hostSigner  ssh.Signer
	shell       string
	environment []string
	logger      *slog.Logger
}

type sshPTYRequest struct {
	Term    string
	Columns uint32
	Rows    uint32
	Width   uint32
	Height  uint32
	Modes   string
}

type sshWindowChange struct {
	Columns uint32
	Rows    uint32
	Width   uint32
	Height  uint32
}

type sshSignalRequest struct {
	Signal string
}

type shellProcess struct {
	command *exec.Cmd
	pty     *os.File
	done    chan error

	closeOnce sync.Once
	ptyMu     sync.Mutex
}

func newEmbeddedSSHServer(hostSigner ssh.Signer, shell string, environment []string, logger *slog.Logger) *embeddedSSHServer {
	return &embeddedSSHServer{
		hostSigner:  hostSigner,
		shell:       shell,
		environment: append([]string(nil), environment...),
		logger:      logger,
	}
}

func (server *embeddedSSHServer) serve(ctx context.Context, connection net.Conn, authorizedKey ssh.PublicKey) error {
	authorizedKeyBytes := authorizedKey.Marshal()
	config := &ssh.ServerConfig{
		MaxAuthTries:  2,
		ServerVersion: "SSH-2.0-Ariadne",
		PublicKeyCallback: func(metadata ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if metadata.User() != sshSessionUser {
				return nil, fmt.Errorf("unknown SSH user %q", metadata.User())
			}
			candidate := key.Marshal()
			if len(candidate) != len(authorizedKeyBytes) || subtle.ConstantTimeCompare(candidate, authorizedKeyBytes) != 1 {
				return nil, errors.New("SSH session key is not authorized for this stream")
			}
			return &ssh.Permissions{}, nil
		},
	}
	config.AddHostKey(server.hostSigner)

	sshConnection, channels, requests, err := ssh.NewServerConn(connection, config)
	if err != nil {
		return fmt.Errorf("embedded SSH handshake: %w", err)
	}
	defer sshConnection.Close()
	go ssh.DiscardRequests(requests)

	sessionDone := make(chan error, 1)
	acceptedSession := false
	for {
		select {
		case newChannel, open := <-channels:
			if !open {
				if acceptedSession {
					select {
					case err := <-sessionDone:
						return err
					default:
					}
				}
				return nil
			}
			if newChannel.ChannelType() != "session" {
				_ = newChannel.Reject(ssh.UnknownChannelType, "only session channels are supported")
				continue
			}
			if acceptedSession {
				_ = newChannel.Reject(ssh.ResourceShortage, "one session is allowed per stream")
				continue
			}
			channel, channelRequests, err := newChannel.Accept()
			if err != nil {
				return fmt.Errorf("accept SSH session channel: %w", err)
			}
			acceptedSession = true
			go func() {
				sessionDone <- server.serveSession(ctx, channel, channelRequests)
			}()

		case err := <-sessionDone:
			return err

		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (server *embeddedSSHServer) serveSession(ctx context.Context, channel ssh.Channel, requests <-chan *ssh.Request) error {
	defer channel.Close()
	var requestedPTY *sshPTYRequest
	var process *shellProcess

	for {
		var processDone <-chan error
		if process != nil {
			processDone = process.done
		}
		select {
		case request, open := <-requests:
			if !open {
				if process != nil {
					process.close()
				}
				return nil
			}
			switch request.Type {
			case "pty-req":
				if process != nil || requestedPTY != nil {
					replySSHRequest(request, false)
					continue
				}
				var decoded sshPTYRequest
				if err := ssh.Unmarshal(request.Payload, &decoded); err != nil || !validPTYRequest(decoded) {
					replySSHRequest(request, false)
					continue
				}
				requestedPTY = &decoded
				replySSHRequest(request, true)

			case "shell":
				if process != nil || len(request.Payload) != 0 {
					replySSHRequest(request, false)
					continue
				}
				started, err := server.startShell(ctx, channel, requestedPTY)
				if err != nil {
					replySSHRequest(request, false)
					return err
				}
				process = started
				replySSHRequest(request, true)

			case "window-change":
				var change sshWindowChange
				ok := process != nil && process.pty != nil && ssh.Unmarshal(request.Payload, &change) == nil && validWindow(change.Columns, change.Rows)
				if ok {
					ok = process.resize(change) == nil
				}
				replySSHRequest(request, ok)

			case "signal":
				var signalRequest sshSignalRequest
				ok := process != nil && ssh.Unmarshal(request.Payload, &signalRequest) == nil && process.signal(signalRequest.Signal) == nil
				replySSHRequest(request, ok)

			default:
				replySSHRequest(request, false)
			}

		case err := <-processDone:
			status := processExitStatus(err)
			_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{Status: status}))
			return nil

		case <-ctx.Done():
			if process != nil {
				process.close()
			}
			return ctx.Err()
		}
	}
}

func (server *embeddedSSHServer) startShell(ctx context.Context, channel ssh.Channel, requestedPTY *sshPTYRequest) (*shellProcess, error) {
	environment := server.shellEnvironment()
	shell, err := resolveShell(server.shell, environment)
	if err != nil {
		return nil, err
	}
	environment = setEnvironment(environment, "SHELL", shell)
	command := exec.CommandContext(ctx, shell)
	if requestedPTY != nil {
		environment = setEnvironment(environment, "TERM", requestedPTY.Term)
	}
	command.Env = environment
	process := &shellProcess{command: command, done: make(chan error, 1)}

	if requestedPTY != nil {
		terminal, err := pty.StartWithSize(command, &pty.Winsize{
			Cols: uint16(requestedPTY.Columns),
			Rows: uint16(requestedPTY.Rows),
			X:    uint16Clamped(requestedPTY.Width),
			Y:    uint16Clamped(requestedPTY.Height),
		})
		if err != nil {
			return nil, fmt.Errorf("start shell with PTY: %w", err)
		}
		process.pty = terminal
		outputDone := make(chan struct{})
		go func() {
			_, _ = io.Copy(terminal, channel)
		}()
		go func() {
			_, copyErr := io.Copy(channel, terminal)
			if copyErr != nil && !isPTYEnd(copyErr) && ctx.Err() == nil {
				server.logger.Debug("embedded SSH PTY output ended", "error", copyErr)
			}
			close(outputDone)
		}()
		go func() {
			waitErr := command.Wait()
			select {
			case <-outputDone:
			case <-time.After(time.Second):
				process.closePTY()
			}
			process.closePTY()
			process.done <- waitErr
		}()
		return process, nil
	}

	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("create shell stdin: %w", err)
	}
	command.Stdout = channel
	command.Stderr = channel.Stderr()
	if err := command.Start(); err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("start shell: %w", err)
	}
	go func() {
		_, _ = io.Copy(stdin, channel)
		_ = stdin.Close()
	}()
	go func() {
		process.done <- command.Wait()
	}()
	return process, nil
}

func (server *embeddedSSHServer) shellEnvironment() []string {
	if server.environment != nil {
		return safeEnvironment(server.environment)
	}
	return safeEnvironment(os.Environ())
}

func (process *shellProcess) signal(name string) error {
	if process.command.Process == nil {
		return errors.New("shell process has not started")
	}
	var signal os.Signal
	switch strings.ToUpper(name) {
	case "INT":
		signal = os.Interrupt
	case "TERM":
		signal = syscall.SIGTERM
	case "HUP":
		signal = syscall.SIGHUP
	case "KILL":
		signal = os.Kill
	default:
		return fmt.Errorf("unsupported SSH signal %q", name)
	}
	return process.command.Process.Signal(signal)
}

func (process *shellProcess) close() {
	process.closeOnce.Do(func() {
		if process.command.Process != nil {
			_ = process.command.Process.Kill()
		}
		if process.pty != nil {
			process.closePTY()
		}
	})
}

func (process *shellProcess) resize(change sshWindowChange) error {
	process.ptyMu.Lock()
	defer process.ptyMu.Unlock()
	if process.pty == nil {
		return errors.New("shell has no PTY")
	}
	return pty.Setsize(process.pty, &pty.Winsize{
		Cols: uint16(change.Columns),
		Rows: uint16(change.Rows),
		X:    uint16Clamped(change.Width),
		Y:    uint16Clamped(change.Height),
	})
}

func (process *shellProcess) closePTY() {
	process.ptyMu.Lock()
	defer process.ptyMu.Unlock()
	if process.pty != nil {
		_ = process.pty.Close()
	}
}

func validPTYRequest(request sshPTYRequest) bool {
	return request.Term != "" && len(request.Term) <= maxTerminalNameLength && !strings.ContainsRune(request.Term, '\x00') && validWindow(request.Columns, request.Rows)
}

func validWindow(columns, rows uint32) bool {
	return columns > 0 && columns <= uint32(^uint16(0)) && rows > 0 && rows <= uint32(^uint16(0))
}

func uint16Clamped(value uint32) uint16 {
	if value > uint32(^uint16(0)) {
		return ^uint16(0)
	}
	return uint16(value)
}

func resolveShell(configured string, environment []string) (string, error) {
	candidates := []string{configured, environmentValue(environment, "SHELL"), "sh"}
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		path, err := exec.LookPath(candidate)
		if err == nil {
			return path, nil
		}
		if configured != "" && candidate == configured {
			return "", fmt.Errorf("find configured shell %q: %w", configured, err)
		}
	}
	return "", errors.New("no shell executable found; set --shell or SHELL")
}

func environmentValue(environment []string, name string) string {
	prefix := name + "="
	for _, item := range environment {
		if strings.HasPrefix(item, prefix) {
			return strings.TrimPrefix(item, prefix)
		}
	}
	return ""
}

func setEnvironment(environment []string, name, value string) []string {
	prefix := name + "="
	updated := make([]string, 0, len(environment)+1)
	for _, item := range environment {
		if !strings.HasPrefix(item, prefix) {
			updated = append(updated, item)
		}
	}
	return append(updated, prefix+value)
}

func processExitStatus(err error) uint32 {
	if err == nil {
		return 0
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		code := exitError.ExitCode()
		if code >= 0 && code <= 255 {
			return uint32(code)
		}
	}
	return 255
}

func replySSHRequest(request *ssh.Request, accepted bool) {
	if request.WantReply {
		_ = request.Reply(accepted, nil)
	}
}

func isPTYEnd(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, os.ErrClosed) || bytes.Contains([]byte(err.Error()), []byte("input/output error"))
}
