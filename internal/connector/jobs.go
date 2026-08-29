package connector

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"sync"
	"time"

	"github.com/mirmik/ariadne/internal/wire"
)

const (
	defaultJobReadLimit = 64 << 10
	maximumJobReadLimit = 256 << 10
)

type jobManager struct {
	mu          sync.RWMutex
	jobs        map[string]*backgroundJob
	config      Config
	environment []string
	lifecycle   sync.Once
}

type backgroundJob struct {
	mu              sync.RWMutex
	info            wire.JobInfo
	stdout          *spoolWriter
	stderr          *spoolWriter
	cancel          context.CancelFunc
	cancelRequested bool
	done            chan struct{}
}

type spoolWriter struct {
	mu        sync.RWMutex
	file      *os.File
	path      string
	limit     int64
	size      int64
	truncated bool
}

func newJobManager(config Config) *jobManager {
	return &jobManager{jobs: make(map[string]*backgroundJob), config: config, environment: append([]string(nil), config.ShellEnvironment...)}
}

func (manager *jobManager) Handle(lifecycle context.Context, request wire.JobRequest) wire.JobResponse {
	manager.bindLifecycle(lifecycle)
	manager.cleanup()
	switch request.Action {
	case wire.JobActionStart:
		if request.Exec == nil {
			return jobError("job start requires exec")
		}
		job, err := manager.start(lifecycle, *request.Exec, request.Command, request.Shell)
		if err != nil {
			return jobError(err.Error())
		}
		info := job.snapshot()
		return wire.JobResponse{Job: &info}
	case wire.JobActionList:
		return wire.JobResponse{Jobs: manager.list()}
	case wire.JobActionStatus:
		job := manager.lookup(request.JobID)
		if job == nil {
			return jobError("job not found")
		}
		info := job.snapshot()
		return wire.JobResponse{Job: &info}
	case wire.JobActionRead:
		job := manager.lookup(request.JobID)
		if job == nil {
			return jobError("job not found")
		}
		output, err := job.read(request.StdoutOffset, request.StderrOffset, request.Limit)
		if err != nil {
			return jobError(err.Error())
		}
		info := job.snapshot()
		return wire.JobResponse{Job: &info, Output: &output}
	case wire.JobActionCancel:
		job := manager.lookup(request.JobID)
		if job == nil {
			return jobError("job not found")
		}
		if err := job.cancelJob(); err != nil {
			return jobError(err.Error())
		}
		info := job.snapshot()
		return wire.JobResponse{Job: &info}
	case wire.JobActionRemove:
		if err := manager.remove(request.JobID); err != nil {
			return jobError(err.Error())
		}
		return wire.JobResponse{}
	default:
		return jobError(fmt.Sprintf("unsupported job action %q", request.Action))
	}
}

func (manager *jobManager) start(lifecycle context.Context, request wire.ExecRequest, displayCommand, shell string) (*backgroundJob, error) {
	if len(request.Argv) == 0 || request.Argv[0] == "" {
		return nil, errors.New("job argv must contain a non-empty executable")
	}
	if request.TimeoutMillis < 0 || request.TimeoutMillis > manager.config.MaxJobTimeout.Milliseconds() {
		return nil, fmt.Errorf("job timeout must be non-negative and not exceed %s", manager.config.MaxJobTimeout)
	}
	manager.mu.RLock()
	running := 0
	for _, candidate := range manager.jobs {
		if candidate.running() {
			running++
		}
	}
	manager.mu.RUnlock()
	if running >= manager.config.MaxConcurrentJobs {
		return nil, errors.New("connector is at background job capacity")
	}

	stdout, err := newSpoolWriter(manager.config.MaxJobOutputBytes)
	if err != nil {
		return nil, err
	}
	stderr, err := newSpoolWriter(manager.config.MaxJobOutputBytes)
	if err != nil {
		stdout.remove()
		return nil, err
	}
	id, err := newJobID()
	if err != nil {
		stdout.remove()
		stderr.remove()
		return nil, err
	}
	jobContext := lifecycle
	var cancel context.CancelFunc
	if request.TimeoutMillis > 0 {
		jobContext, cancel = context.WithTimeout(lifecycle, time.Duration(request.TimeoutMillis)*time.Millisecond)
	} else {
		jobContext, cancel = context.WithCancel(lifecycle)
	}
	command := exec.CommandContext(jobContext, request.Argv[0], request.Argv[1:]...)
	command.Dir = request.Cwd
	command.Stdout = stdout
	command.Stderr = stderr
	command.WaitDelay = 5 * time.Second
	if manager.config.ShellEnvironment != nil {
		command.Env = append([]string(nil), manager.environment...)
	}
	now := time.Now().UTC()
	job := &backgroundJob{
		info: wire.JobInfo{
			ID:        id,
			State:     "running",
			Argv:      append([]string(nil), request.Argv...),
			Command:   displayCommand,
			Shell:     shell,
			Cwd:       request.Cwd,
			CreatedAt: now,
			StartedAt: now,
			ExitCode:  -1,
		},
		stdout: stdout,
		stderr: stderr,
		cancel: cancel,
		done:   make(chan struct{}),
	}
	if err := command.Start(); err != nil {
		cancel()
		stdout.remove()
		stderr.remove()
		return nil, fmt.Errorf("start background job: %w", err)
	}
	manager.mu.Lock()
	manager.jobs[id] = job
	manager.mu.Unlock()
	go job.wait(command, jobContext)
	return job, nil
}

func (job *backgroundJob) wait(command *exec.Cmd, jobContext context.Context) {
	defer close(job.done)
	err := command.Wait()
	job.stdout.close()
	job.stderr.close()
	job.mu.Lock()
	defer job.mu.Unlock()
	job.info.FinishedAt = time.Now().UTC()
	if command.ProcessState != nil {
		job.info.ExitCode = command.ProcessState.ExitCode()
	}
	switch {
	case job.cancelRequested:
		job.info.State = "canceled"
		job.info.Error = "job canceled"
	case errors.Is(jobContext.Err(), context.DeadlineExceeded):
		job.info.State = "timed_out"
		job.info.Error = "job timed out"
	case errors.Is(jobContext.Err(), context.Canceled):
		job.info.State = "canceled"
		job.info.Error = "connector stopped"
	case err == nil:
		job.info.State = "succeeded"
	case command.ProcessState != nil:
		job.info.State = "failed"
	default:
		job.info.State = "failed"
		job.info.Error = err.Error()
	}
}

func (manager *jobManager) lookup(id string) *backgroundJob {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	return manager.jobs[id]
}

func (manager *jobManager) list() []wire.JobInfo {
	manager.mu.RLock()
	jobs := make([]*backgroundJob, 0, len(manager.jobs))
	for _, job := range manager.jobs {
		jobs = append(jobs, job)
	}
	manager.mu.RUnlock()
	result := make([]wire.JobInfo, 0, len(jobs))
	for _, job := range jobs {
		result = append(result, job.snapshot())
	}
	sort.Slice(result, func(left, right int) bool { return result[left].CreatedAt.After(result[right].CreatedAt) })
	return result
}

func (job *backgroundJob) snapshot() wire.JobInfo {
	job.mu.RLock()
	info := job.info
	info.Argv = append([]string(nil), job.info.Argv...)
	job.mu.RUnlock()
	info.StdoutSize, info.StdoutTruncated = job.stdout.stats()
	info.StderrSize, info.StderrTruncated = job.stderr.stats()
	return info
}

func (job *backgroundJob) running() bool {
	job.mu.RLock()
	defer job.mu.RUnlock()
	return job.info.State == "running"
}

func (job *backgroundJob) cancelJob() error {
	job.mu.Lock()
	defer job.mu.Unlock()
	if job.info.State != "running" {
		return errors.New("job is not running")
	}
	job.cancelRequested = true
	job.cancel()
	return nil
}

func (job *backgroundJob) read(stdoutOffset, stderrOffset int64, limit int) (wire.JobOutput, error) {
	if stdoutOffset < 0 || stderrOffset < 0 {
		return wire.JobOutput{}, errors.New("job output offsets must be non-negative")
	}
	if limit == 0 {
		limit = defaultJobReadLimit
	}
	if limit < 1 || limit > maximumJobReadLimit {
		return wire.JobOutput{}, fmt.Errorf("job output limit must be between 1 and %d", maximumJobReadLimit)
	}
	stdout, stdoutNext, err := job.stdout.read(stdoutOffset, limit)
	if err != nil {
		return wire.JobOutput{}, err
	}
	stderr, stderrNext, err := job.stderr.read(stderrOffset, limit)
	if err != nil {
		return wire.JobOutput{}, err
	}
	terminal := !job.running()
	stdoutSize, _ := job.stdout.stats()
	stderrSize, _ := job.stderr.stats()
	return wire.JobOutput{
		Stdout:           stdout,
		Stderr:           stderr,
		NextStdoutOffset: stdoutNext,
		NextStderrOffset: stderrNext,
		StdoutEOF:        terminal && stdoutNext >= stdoutSize,
		StderrEOF:        terminal && stderrNext >= stderrSize,
	}, nil
}

func (manager *jobManager) remove(id string) error {
	manager.mu.Lock()
	job := manager.jobs[id]
	if job == nil {
		manager.mu.Unlock()
		return errors.New("job not found")
	}
	if job.running() {
		manager.mu.Unlock()
		return errors.New("running job must be canceled before removal")
	}
	delete(manager.jobs, id)
	manager.mu.Unlock()
	job.stdout.remove()
	job.stderr.remove()
	return nil
}

func (manager *jobManager) cleanup() {
	cutoff := time.Now().UTC().Add(-manager.config.JobRetention)
	manager.mu.Lock()
	completed := make([]*backgroundJob, 0)
	for id, job := range manager.jobs {
		info := job.snapshot()
		if info.State != "running" {
			if !info.FinishedAt.IsZero() && info.FinishedAt.Before(cutoff) {
				delete(manager.jobs, id)
				job.stdout.remove()
				job.stderr.remove()
				continue
			}
			completed = append(completed, job)
		}
	}
	sort.Slice(completed, func(left, right int) bool {
		return completed[left].snapshot().FinishedAt.After(completed[right].snapshot().FinishedAt)
	})
	if len(completed) > manager.config.MaxRetainedJobs {
		for _, job := range completed[manager.config.MaxRetainedJobs:] {
			if _, exists := manager.jobs[job.info.ID]; exists {
				delete(manager.jobs, job.info.ID)
				job.stdout.remove()
				job.stderr.remove()
			}
		}
	}
	manager.mu.Unlock()
}

func (manager *jobManager) bindLifecycle(lifecycle context.Context) {
	manager.lifecycle.Do(func() {
		go func() {
			<-lifecycle.Done()
			manager.shutdown()
		}()
	})
}

func (manager *jobManager) shutdown() {
	manager.mu.RLock()
	jobs := make([]*backgroundJob, 0, len(manager.jobs))
	for _, job := range manager.jobs {
		jobs = append(jobs, job)
	}
	manager.mu.RUnlock()
	for _, job := range jobs {
		if job.running() {
			_ = job.cancelJob()
		}
	}
	for _, job := range jobs {
		<-job.done
	}
	manager.mu.Lock()
	for id := range manager.jobs {
		delete(manager.jobs, id)
	}
	manager.mu.Unlock()
	for _, job := range jobs {
		job.stdout.remove()
		job.stderr.remove()
	}
}

func newSpoolWriter(limit int64) (*spoolWriter, error) {
	file, err := os.CreateTemp("", "ariadne-job-*.log")
	if err != nil {
		return nil, fmt.Errorf("create job spool: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		os.Remove(file.Name())
		return nil, err
	}
	return &spoolWriter{file: file, path: file.Name(), limit: limit}, nil
}

func (writer *spoolWriter) Write(data []byte) (int, error) {
	original := len(data)
	writer.mu.Lock()
	defer writer.mu.Unlock()
	remaining := writer.limit - writer.size
	if remaining <= 0 {
		writer.truncated = true
		return original, nil
	}
	if int64(len(data)) > remaining {
		data = data[:remaining]
		writer.truncated = true
	}
	written, err := writer.file.Write(data)
	writer.size += int64(written)
	if err != nil {
		return written, err
	}
	return original, nil
}

func (writer *spoolWriter) stats() (int64, bool) {
	writer.mu.RLock()
	defer writer.mu.RUnlock()
	return writer.size, writer.truncated
}

func (writer *spoolWriter) read(offset int64, limit int) ([]byte, int64, error) {
	size, _ := writer.stats()
	if offset > size {
		return nil, offset, errors.New("job output offset exceeds available data")
	}
	length := size - offset
	if length > int64(limit) {
		length = int64(limit)
	}
	if length == 0 {
		return nil, offset, nil
	}
	file, err := os.Open(writer.path)
	if err != nil {
		return nil, offset, err
	}
	defer file.Close()
	data := make([]byte, int(length))
	count, err := file.ReadAt(data, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, offset, err
	}
	return data[:count], offset + int64(count), nil
}

func (writer *spoolWriter) close() {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	_ = writer.file.Sync()
	_ = writer.file.Close()
}

func (writer *spoolWriter) remove() {
	writer.close()
	_ = os.Remove(writer.path)
}

func newJobID() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return "j_" + hex.EncodeToString(raw), nil
}

func jobError(message string) wire.JobResponse {
	return wire.JobResponse{Error: message}
}
