package connector

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/mirmik/ariadne/internal/wire"
)

func TestJobManagerRetainsBoundedOutputAndSupportsCursors(t *testing.T) {
	manager := testJobManager(10)
	response := manager.Handle(context.Background(), wire.JobRequest{Action: wire.JobActionStart, Exec: jobHelperRequest("spam")})
	if response.Error != "" || response.Job == nil {
		t.Fatalf("start failed: %#v", response)
	}
	job := waitForManagedJob(t, manager, response.Job.ID)
	if job.State != "succeeded" || !job.StdoutTruncated || !job.StderrTruncated || job.StdoutSize != 10 || job.StderrSize != 10 {
		t.Fatalf("unexpected completed job: %#v", job)
	}
	first := manager.Handle(context.Background(), wire.JobRequest{Action: wire.JobActionRead, JobID: job.ID, Limit: 4})
	if first.Error != "" || first.Output == nil || len(first.Output.Stdout) != 4 || first.Output.NextStdoutOffset != 4 || first.Output.StdoutEOF {
		t.Fatalf("unexpected first output page: %#v", first)
	}
	second := manager.Handle(context.Background(), wire.JobRequest{Action: wire.JobActionRead, JobID: job.ID, StdoutOffset: first.Output.NextStdoutOffset, StderrOffset: first.Output.NextStderrOffset, Limit: 64})
	if second.Error != "" || second.Output == nil || second.Output.NextStdoutOffset != 10 || !second.Output.StdoutEOF || !second.Output.StderrEOF {
		t.Fatalf("unexpected second output page: %#v", second)
	}
	removed := manager.Handle(context.Background(), wire.JobRequest{Action: wire.JobActionRemove, JobID: job.ID})
	if removed.Error != "" || manager.lookup(job.ID) != nil {
		t.Fatalf("remove failed: %#v", removed)
	}
}

func TestJobManagerCancelsRunningJob(t *testing.T) {
	manager := testJobManager(1024)
	response := manager.Handle(context.Background(), wire.JobRequest{Action: wire.JobActionStart, Exec: jobHelperRequest("sleep")})
	if response.Error != "" || response.Job == nil {
		t.Fatalf("start failed: %#v", response)
	}
	canceled := manager.Handle(context.Background(), wire.JobRequest{Action: wire.JobActionCancel, JobID: response.Job.ID})
	if canceled.Error != "" {
		t.Fatalf("cancel failed: %#v", canceled)
	}
	job := waitForManagedJob(t, manager, response.Job.ID)
	if job.State != "canceled" {
		t.Fatalf("unexpected canceled job: %#v", job)
	}
	if repeated := manager.Handle(context.Background(), wire.JobRequest{Action: wire.JobActionCancel, JobID: response.Job.ID}); repeated.Error != "" {
		t.Fatalf("repeated cancel was not idempotent: %#v", repeated)
	}
}

func TestJobManagerEnforcesRetentionAndCompletedJobLimit(t *testing.T) {
	manager := testJobManager(1024)
	manager.config.MaxRetainedJobs = 1
	firstResponse := manager.Handle(context.Background(), wire.JobRequest{Action: wire.JobActionStart, Exec: jobHelperRequest("exit", "0")})
	if firstResponse.Error != "" || firstResponse.Job == nil {
		t.Fatalf("first start failed: %#v", firstResponse)
	}
	waitForManagedJob(t, manager, firstResponse.Job.ID)
	secondResponse := manager.Handle(context.Background(), wire.JobRequest{Action: wire.JobActionStart, Exec: jobHelperRequest("exit", "0")})
	if secondResponse.Error != "" || secondResponse.Job == nil {
		t.Fatalf("second start failed: %#v", secondResponse)
	}
	waitForManagedJob(t, manager, secondResponse.Job.ID)
	listed := manager.Handle(context.Background(), wire.JobRequest{Action: wire.JobActionList})
	if listed.Error != "" || len(listed.Jobs) != 1 || listed.Jobs[0].ID != secondResponse.Job.ID {
		t.Fatalf("completed job limit was not enforced: %#v", listed)
	}
	manager.config.JobRetention = time.Nanosecond
	time.Sleep(time.Millisecond)
	listed = manager.Handle(context.Background(), wire.JobRequest{Action: wire.JobActionList})
	if listed.Error != "" || len(listed.Jobs) != 0 {
		t.Fatalf("job retention was not enforced: %#v", listed)
	}
}

func testJobManager(outputLimit int64) *jobManager {
	environment := append([]string(nil), os.Environ()...)
	environment = append(environment, "ARIADNE_EXECUTOR_HELPER=1")
	return newJobManager(Config{
		ShellEnvironment:  environment,
		MaxConcurrentJobs: 2,
		MaxJobOutputBytes: outputLimit,
		MaxRetainedJobs:   8,
		MaxJobTimeout:     time.Minute,
		JobRetention:      time.Hour,
	})
}

func jobHelperRequest(arguments ...string) *wire.ExecRequest {
	request := helperRequest(arguments...)
	return &request
}

func waitForManagedJob(t *testing.T, manager *jobManager, jobID string) wire.JobInfo {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		response := manager.Handle(context.Background(), wire.JobRequest{Action: wire.JobActionStatus, JobID: jobID})
		if response.Error == "" && response.Job != nil && response.Job.State != "running" {
			return *response.Job
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("job %s did not finish", jobID)
	return wire.JobInfo{}
}
