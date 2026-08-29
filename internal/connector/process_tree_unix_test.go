//go:build !windows

package connector

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mirmik/ariadne/internal/wire"
)

func TestExecTimeoutTerminatesDescendantProcessTree(t *testing.T) {
	pidPath := t.TempDir() + "/child.pid"
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	result := (LocalExecutor{MaxOutputBytes: 1024}).Execute(ctx, descendantRequest(pidPath))
	if !result.TimedOut {
		t.Fatalf("command did not time out: %#v", result)
	}
	assertRecordedProcessGone(t, pidPath)
}

func TestJobCancellationTerminatesDescendantProcessTree(t *testing.T) {
	pidPath := t.TempDir() + "/child.pid"
	manager := testJobManager(1024)
	request := descendantRequest(pidPath)
	response := manager.Handle(context.Background(), wire.JobRequest{Action: wire.JobActionStart, Exec: &request})
	if response.Error != "" || response.Job == nil {
		t.Fatalf("start failed: %#v", response)
	}
	waitForPIDFile(t, pidPath)
	canceled := manager.Handle(context.Background(), wire.JobRequest{Action: wire.JobActionCancel, JobID: response.Job.ID})
	if canceled.Error != "" {
		t.Fatalf("cancel failed: %#v", canceled)
	}
	if job := waitForManagedJob(t, manager, response.Job.ID); job.State != "canceled" {
		t.Fatalf("unexpected job state: %#v", job)
	}
	assertRecordedProcessGone(t, pidPath)
}

func TestConnectorShutdownTerminatesTreesAndRemovesSpools(t *testing.T) {
	pidPath := t.TempDir() + "/child.pid"
	manager := testJobManager(1024)
	lifecycle, stop := context.WithCancel(context.Background())
	request := descendantRequest(pidPath)
	response := manager.Handle(lifecycle, wire.JobRequest{Action: wire.JobActionStart, Exec: &request})
	if response.Error != "" || response.Job == nil {
		t.Fatalf("start failed: %#v", response)
	}
	job := manager.lookup(response.Job.ID)
	if job == nil {
		t.Fatal("started job is missing")
	}
	stdoutPath, stderrPath := job.stdout.path, job.stderr.path
	waitForPIDFile(t, pidPath)
	stop()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && manager.lookup(response.Job.ID) != nil {
		time.Sleep(10 * time.Millisecond)
	}
	if manager.lookup(response.Job.ID) != nil {
		t.Fatal("connector shutdown retained the job")
	}
	assertRecordedProcessGone(t, pidPath)
	for _, path := range []string{stdoutPath, stderrPath} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("job spool %s remains after shutdown: %v", path, err)
		}
	}
}

func descendantRequest(pidPath string) wire.ExecRequest {
	command := "sleep 30 & child=$!; printf '%s\\n' \"$child\" > \"$1\"; wait"
	return wire.ExecRequest{Argv: []string{"/bin/sh", "-c", command, "ariadne-tree-test", pidPath}}
}

func waitForPIDFile(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data)))
			if parseErr == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("child PID was not recorded in %s", path)
	return 0
}

func assertRecordedProcessGone(t *testing.T, path string) {
	t.Helper()
	pid := waitForPIDFile(t, path)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) || processIsZombie(pid) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("descendant process %d is still alive", pid)
}

func processIsZombie(pid int) bool {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	closing := strings.LastIndexByte(string(data), ')')
	return closing >= 0 && len(data) > closing+2 && data[closing+2] == 'Z'
}
