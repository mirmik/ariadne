package sshtunnel

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mirmik/ariadne/internal/transport"
)

const defaultReadyTimeout = 15 * time.Second

type Config struct {
	Destination   string
	RemoteAddress string
	ReadyTimeout  time.Duration
}

type Tunnel struct {
	URL       string
	command   *exec.Cmd
	done      chan struct{}
	errMu     sync.RWMutex
	err       error
	closeOnce sync.Once
}

func Start(ctx context.Context, config Config) (*Tunnel, error) {
	destination, port, err := parseDestination(config.Destination)
	if err != nil {
		return nil, err
	}
	remoteHost, remotePort, err := net.SplitHostPort(config.RemoteAddress)
	if err != nil || remotePort == "" || !transport.IsLoopbackHost(remoteHost) {
		return nil, errors.New("SSH tunnel remote address must be a loopback host and port")
	}
	if config.ReadyTimeout <= 0 {
		config.ReadyTimeout = defaultReadyTimeout
	}
	if _, err := exec.LookPath("ssh"); err != nil {
		return nil, errors.New("OpenSSH client is required for SSH tunnel mode")
	}
	localAddress, err := availableLoopbackAddress()
	if err != nil {
		return nil, err
	}
	forward := localAddress + ":" + config.RemoteAddress
	arguments := sshArguments(destination, port, forward)
	command := exec.CommandContext(ctx, "ssh", arguments...)
	command.Stdout = os.Stderr
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("start SSH tunnel: %w", err)
	}
	tunnel := &Tunnel{
		URL:     "http://" + localAddress,
		command: command,
		done:    make(chan struct{}),
	}
	go func() {
		processErr := command.Wait()
		tunnel.errMu.Lock()
		tunnel.err = processErr
		tunnel.errMu.Unlock()
		close(tunnel.done)
	}()

	deadline := time.NewTimer(config.ReadyTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-tunnel.done:
			processErr := tunnel.Err()
			if processErr == nil {
				processErr = errors.New("SSH exited before establishing the tunnel")
			}
			return nil, fmt.Errorf("SSH tunnel failed: %w", processErr)
		case <-ticker.C:
			connection, dialErr := net.DialTimeout("tcp", localAddress, 200*time.Millisecond)
			if dialErr == nil {
				_ = connection.Close()
				return tunnel, nil
			}
		case <-deadline.C:
			tunnel.Close()
			return nil, errors.New("timed out waiting for SSH tunnel")
		case <-ctx.Done():
			tunnel.Close()
			return nil, ctx.Err()
		}
	}
}

func sshArguments(destination, port, forward string) []string {
	arguments := []string{
		"-N", "-T", "-n",
		"-o", "ExitOnForwardFailure=yes",
	}
	if port != "" {
		arguments = append(arguments, "-p", port)
	}
	arguments = append(arguments, "-L", forward, destination)
	return arguments
}

func parseDestination(raw string) (destination, port string, err error) {
	if raw == "" {
		return "", "", errors.New("SSH tunnel destination is required")
	}
	if strings.TrimSpace(raw) != raw || strings.ContainsAny(raw, "\t\r\n\x00") {
		return "", "", errors.New("SSH tunnel destination must not contain whitespace or control characters")
	}
	if strings.HasPrefix(raw, "-") {
		return "", "", errors.New("SSH tunnel destination must not start with an option prefix")
	}

	userPrefix := ""
	hostPort := raw
	if at := strings.LastIndexByte(raw, '@'); at >= 0 {
		if at == 0 || at == len(raw)-1 {
			return "", "", errors.New("SSH tunnel destination must contain a non-empty user and host")
		}
		userPrefix = raw[:at+1]
		hostPort = raw[at+1:]
	}

	host := hostPort
	portText := ""
	if strings.HasPrefix(hostPort, "[") {
		closing := strings.IndexByte(hostPort, ']')
		if closing < 0 {
			return "", "", errors.New("SSH tunnel destination has an unterminated IPv6 address")
		}
		host = hostPort[1:closing]
		remainder := hostPort[closing+1:]
		switch {
		case remainder == "":
		case strings.HasPrefix(remainder, ":"):
			portText = remainder[1:]
		default:
			return "", "", errors.New("SSH tunnel destination has invalid text after the bracketed host")
		}
	} else if strings.Count(hostPort, ":") == 1 {
		host, portText, _ = strings.Cut(hostPort, ":")
	}

	if host == "" || strings.HasPrefix(host, "-") {
		return "", "", errors.New("SSH tunnel destination must contain a valid host")
	}
	if portText != "" {
		parsedPort, parseErr := strconv.ParseUint(portText, 10, 16)
		if parseErr != nil || parsedPort == 0 {
			return "", "", errors.New("SSH tunnel port must be an integer between 1 and 65535")
		}
		port = strconv.FormatUint(parsedPort, 10)
	} else if strings.HasSuffix(hostPort, ":") {
		return "", "", errors.New("SSH tunnel destination has an empty port")
	}

	return userPrefix + host, port, nil
}

func (tunnel *Tunnel) Done() <-chan struct{} {
	return tunnel.done
}

func (tunnel *Tunnel) Err() error {
	tunnel.errMu.RLock()
	defer tunnel.errMu.RUnlock()
	return tunnel.err
}

func (tunnel *Tunnel) Close() {
	tunnel.closeOnce.Do(func() {
		select {
		case <-tunnel.done:
			return
		default:
		}
		if runtime.GOOS == "windows" {
			_ = tunnel.command.Process.Kill()
		} else if err := tunnel.command.Process.Signal(os.Interrupt); err != nil {
			_ = tunnel.command.Process.Kill()
		}
		select {
		case <-tunnel.done:
		case <-time.After(2 * time.Second):
			_ = tunnel.command.Process.Kill()
			<-tunnel.done
		}
	})
}

func availableLoopbackAddress() (string, error) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("allocate local SSH tunnel port: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		return "", fmt.Errorf("release local SSH tunnel port: %w", err)
	}
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), nil
}
