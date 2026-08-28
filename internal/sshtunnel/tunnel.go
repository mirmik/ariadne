package sshtunnel

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
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
	if config.Destination == "" {
		return nil, errors.New("SSH tunnel destination is required")
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
	command := exec.CommandContext(ctx, "ssh",
		"-N", "-T", "-n",
		"-o", "ExitOnForwardFailure=yes",
		"-L", forward,
		config.Destination,
	)
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
		_ = tunnel.command.Process.Signal(os.Interrupt)
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
