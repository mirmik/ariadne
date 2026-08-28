package connector

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"time"

	"github.com/mirmik/ariadne/internal/wire"
)

const defaultMaxOutputBytes = wire.MaxExecOutputBytes

type Executor interface {
	Execute(context.Context, wire.ExecRequest) wire.ExecResult
}

type LocalExecutor struct {
	MaxOutputBytes int
	Environment    []string
}

func (executor LocalExecutor) Execute(ctx context.Context, request wire.ExecRequest) (result wire.ExecResult) {
	startedAt := time.Now()
	result = wire.ExecResult{ExitCode: -1}
	defer func() {
		result.DurationMillis = time.Since(startedAt).Milliseconds()
	}()
	if len(request.Argv) == 0 || request.Argv[0] == "" {
		result.Error = "argv must contain a non-empty executable"
		return result
	}

	maxOutputBytes := executor.MaxOutputBytes
	if maxOutputBytes <= 0 {
		maxOutputBytes = defaultMaxOutputBytes
	}
	stdout := newCappedBuffer(maxOutputBytes)
	stderr := newCappedBuffer(maxOutputBytes)
	command := exec.CommandContext(ctx, request.Argv[0], request.Argv[1:]...)
	command.Dir = request.Cwd
	command.Stdout = stdout
	command.Stderr = stderr
	if executor.Environment != nil {
		command.Env = append([]string(nil), executor.Environment...)
	}

	err := command.Run()
	result.Stdout = stdout.Bytes()
	result.Stderr = stderr.Bytes()
	result.StdoutTruncated = stdout.Truncated()
	result.StderrTruncated = stderr.Truncated()
	if command.ProcessState != nil {
		result.ExitCode = command.ProcessState.ExitCode()
	}
	if err == nil {
		return result
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		result.TimedOut = true
		result.Error = "command timed out"
		return result
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		result.Error = "command canceled"
		return result
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		result.ExitCode = exitError.ExitCode()
		return result
	}
	result.Error = err.Error()
	return result
}

type cappedBuffer struct {
	buffer    bytes.Buffer
	limit     int
	truncated bool
}

func newCappedBuffer(limit int) *cappedBuffer {
	return &cappedBuffer{limit: limit}
}

func (buffer *cappedBuffer) Write(data []byte) (int, error) {
	originalLength := len(data)
	remaining := buffer.limit - buffer.buffer.Len()
	if remaining < 0 {
		remaining = 0
	}
	if remaining > 0 {
		if len(data) > remaining {
			data = data[:remaining]
		}
		_, _ = buffer.buffer.Write(data)
	}
	if originalLength > remaining {
		buffer.truncated = true
	}
	return originalLength, nil
}

func (buffer *cappedBuffer) Bytes() []byte {
	return append([]byte(nil), buffer.buffer.Bytes()...)
}

func (buffer *cappedBuffer) Truncated() bool {
	return buffer.truncated
}
