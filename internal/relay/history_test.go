package relay

import (
	"fmt"
	"testing"

	"github.com/mirmik/ariadne/internal/wire"
)

func TestRecentIDsBounded(t *testing.T) {
	var history recentIDs
	for i := range recentIDLimit + 2 {
		history.add(fmt.Sprint(i))
	}
	if len(history.ids) != recentIDLimit || len(history.order) != recentIDLimit {
		t.Fatal("history is unbounded")
	}
	if history.contains("0") || history.contains("1") || !history.contains("2") || !history.contains(fmt.Sprint(recentIDLimit+1)) {
		t.Fatal("history did not retain latest IDs")
	}
	for range recentIDLimit {
		history.add("2")
	}
	if len(history.ids) != recentIDLimit || !history.contains("3") {
		t.Fatal("duplicates evict history")
	}
}

func TestLateResponseHistorySeparatesRequestKinds(t *testing.T) {
	session := newNodeSession(nil, &reviewConnection{}, wire.NodeInfo{})
	session.lateExec.add("exec-id")
	session.lateJobs.add("job-id")
	for _, input := range []struct {
		kind    wire.MessageType
		id      string
		payload any
	}{
		{wire.MessageExecResult, "job-id", wire.ExecResult{}},
		{wire.MessageJobResponse, "exec-id", wire.JobResponse{}},
		{wire.MessageExecResult, "never-issued", wire.ExecResult{}},
		{wire.MessageJobResponse, "never-issued", wire.JobResponse{}},
	} {
		if err := session.handleControl(testEnvelope(t, input.kind, input.id, input.payload)); err == nil {
			t.Fatalf("accepted %s for %s", input.kind, input.id)
		}
	}
	for i := range recentIDLimit {
		session.lateExec.add(fmt.Sprintf("new-%d", i))
	}
	if err := session.handleControl(testEnvelope(t, wire.MessageExecResult, "exec-id", wire.ExecResult{})); err == nil {
		t.Fatal("accepted evicted late ID")
	}
}
