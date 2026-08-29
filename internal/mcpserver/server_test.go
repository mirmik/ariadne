package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/mirmik/ariadne/internal/wire"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type fakeAPI struct {
	nodes       []wire.NodeInfo
	nodesErr    error
	claimedNode wire.NodeInfo
	claimErr    error
	execResult  wire.ExecResult
	execErr     error
	claimID     string
	claimAlias  string
	execTarget  string
	execRequest wire.ExecRequest
	fileResult  wire.FileTransferResult
	fileErr     error
	fileTarget  string
	localPath   string
	remotePath  string
	overwrite   bool
	job         wire.JobInfo
	jobs        []wire.JobInfo
	jobOutput   wire.JobOutput
	jobErr      error
	jobTarget   string
	jobID       string
	jobRequest  wire.ExecRequest
	stdoutOff   int64
	stderrOff   int64
	readLimit   int
}

func (api *fakeAPI) Nodes(context.Context) ([]wire.NodeInfo, error) {
	return append([]wire.NodeInfo(nil), api.nodes...), api.nodesErr
}

func (api *fakeAPI) Claim(_ context.Context, nodeID, alias string) (wire.NodeInfo, error) {
	api.claimID = nodeID
	api.claimAlias = alias
	return api.claimedNode, api.claimErr
}

func (api *fakeAPI) Exec(_ context.Context, target string, request wire.ExecRequest) (wire.ExecResult, error) {
	api.execTarget = target
	api.execRequest = request
	return api.execResult, api.execErr
}

func (api *fakeAPI) UploadFile(_ context.Context, target, localPath, remotePath string, overwrite bool) (wire.FileTransferResult, error) {
	api.fileTarget, api.localPath, api.remotePath, api.overwrite = target, localPath, remotePath, overwrite
	return api.fileResult, api.fileErr
}

func (api *fakeAPI) DownloadFile(_ context.Context, target, remotePath, localPath string, overwrite bool) (wire.FileTransferResult, error) {
	api.fileTarget, api.localPath, api.remotePath, api.overwrite = target, localPath, remotePath, overwrite
	return api.fileResult, api.fileErr
}

func (api *fakeAPI) StartJob(_ context.Context, target string, request wire.ExecRequest) (wire.JobInfo, error) {
	api.jobTarget, api.jobRequest = target, request
	return api.job, api.jobErr
}

func (api *fakeAPI) ListJobs(_ context.Context, target string) ([]wire.JobInfo, error) {
	api.jobTarget = target
	return append([]wire.JobInfo(nil), api.jobs...), api.jobErr
}

func (api *fakeAPI) JobStatus(_ context.Context, target, jobID string) (wire.JobInfo, error) {
	api.jobTarget, api.jobID = target, jobID
	return api.job, api.jobErr
}

func (api *fakeAPI) ReadJob(_ context.Context, target, jobID string, stdoutOffset, stderrOffset int64, limit int) (wire.JobInfo, wire.JobOutput, error) {
	api.jobTarget, api.jobID, api.stdoutOff, api.stderrOff, api.readLimit = target, jobID, stdoutOffset, stderrOffset, limit
	return api.job, api.jobOutput, api.jobErr
}

func (api *fakeAPI) CancelJob(_ context.Context, target, jobID string) (wire.JobInfo, error) {
	api.jobTarget, api.jobID = target, jobID
	return api.job, api.jobErr
}

func (api *fakeAPI) RemoveJob(_ context.Context, target, jobID string) error {
	api.jobTarget, api.jobID = target, jobID
	return api.jobErr
}

func TestNodesToolReturnsStructuredNodeInformation(t *testing.T) {
	connectedAt := time.Date(2026, 8, 28, 13, 52, 26, 0, time.FixedZone("MSK", 3*60*60))
	api := &fakeAPI{nodes: []wire.NodeInfo{{
		ID:               "n_test",
		Alias:            "radio",
		AliasClaimed:     true,
		Platform:         "windows",
		Architecture:     "amd64",
		ConnectorVersion: "v0.1.5",
		ConnectedAt:      connectedAt,
		Online:           true,
	}}}
	session := connectTestClient(t, api)
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "ariadne_nodes"})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("ariadne_nodes returned a tool error: %#v", result.Content)
	}
	var output NodesOutput
	decodeStructuredOutput(t, result.StructuredContent, &output)
	if len(output.Nodes) != 1 || output.Nodes[0].Alias != "radio" || output.Nodes[0].ConnectedAt != connectedAt.Format(time.RFC3339) {
		t.Fatalf("unexpected nodes output: %#v", output)
	}
}

func TestClaimToolForwardsExactIdentityAndAlias(t *testing.T) {
	api := &fakeAPI{claimedNode: wire.NodeInfo{ID: "n_test", Alias: "radio", AliasClaimed: true, Online: true}}
	session := connectTestClient(t, api)
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "ariadne_claim",
		Arguments: map[string]any{"node_id": "n_test", "alias": "radio"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError || api.claimID != "n_test" || api.claimAlias != "radio" {
		t.Fatalf("claim was not forwarded correctly: result=%#v id=%q alias=%q", result, api.claimID, api.claimAlias)
	}
}

func TestExecToolForwardsArgvAndPreservesCommandFailure(t *testing.T) {
	api := &fakeAPI{execResult: wire.ExecResult{
		ExitCode:       2,
		Stdout:         []byte("configure output"),
		Stderr:         []byte("compiler missing"),
		DurationMillis: 321,
	}}
	session := connectTestClient(t, api)
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "ariadne_exec",
		Arguments: map[string]any{
			"target":     "radio",
			"argv":       []string{"task", "build"},
			"cwd":        `C:\Users\rfmeas\project\termin`,
			"timeout_ms": 120000,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("a remote non-zero exit became an MCP tool error: %#v", result.Content)
	}
	var output ExecOutput
	decodeStructuredOutput(t, result.StructuredContent, &output)
	if output.OK || output.ExitCode != 2 || output.Stderr != "compiler missing" {
		t.Fatalf("unexpected exec output: %#v", output)
	}
	if api.execTarget != "radio" || api.execRequest.Cwd != `C:\Users\rfmeas\project\termin` || api.execRequest.TimeoutMillis != 120000 {
		t.Fatalf("exec request was not forwarded: target=%q request=%#v", api.execTarget, api.execRequest)
	}
	if len(api.execRequest.Argv) != 2 || api.execRequest.Argv[0] != "task" || api.execRequest.Argv[1] != "build" {
		t.Fatalf("argv was not preserved: %#v", api.execRequest.Argv)
	}
}

func TestExecToolPrefersCommandAndForwardsShellSelection(t *testing.T) {
	api := &fakeAPI{execResult: wire.ExecResult{
		ExitCode:       0,
		Shell:          "powershell.exe",
		Stdout:         []byte("built"),
		DurationMillis: 50,
	}}
	session := connectTestClient(t, api)
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "ariadne_exec",
		Arguments: map[string]any{
			"target":  "radio",
			"command": "task build; git status --short",
			"shell":   "auto",
			"cwd":     `C:\Users\rfmeas\project\termin`,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("command execution returned a tool error: %#v", result.Content)
	}
	var output ExecOutput
	decodeStructuredOutput(t, result.StructuredContent, &output)
	if !output.OK || output.Shell != "powershell.exe" || output.Stdout != "built" {
		t.Fatalf("unexpected exec output: %#v", output)
	}
	if api.execRequest.Command != "task build; git status --short" || api.execRequest.Shell != "auto" || len(api.execRequest.Argv) != 0 {
		t.Fatalf("command request was not preserved: %#v", api.execRequest)
	}
}

func TestExecToolReportsValidationAndTransportErrorsAsToolErrors(t *testing.T) {
	for _, test := range []struct {
		name string
		api  *fakeAPI
		args map[string]any
	}{
		{name: "missing command", api: &fakeAPI{}, args: map[string]any{"target": "radio"}},
		{name: "ambiguous input", api: &fakeAPI{}, args: map[string]any{"target": "radio", "command": "pwd", "argv": []string{"pwd"}}},
		{name: "relay failure", api: &fakeAPI{execErr: errors.New("relay unavailable")}, args: map[string]any{"target": "radio", "argv": []string{"echo"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			session := connectTestClient(t, test.api)
			result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "ariadne_exec", Arguments: test.args})
			if err != nil {
				t.Fatal(err)
			}
			if !result.IsError {
				t.Fatalf("expected MCP tool error, got %#v", result)
			}
		})
	}
}

func TestFileToolsForwardPathsWithoutPassingBytesThroughMCP(t *testing.T) {
	api := &fakeAPI{fileResult: wire.FileTransferResult{Size: 1234, SHA256: "abcd"}}
	session := connectTestClient(t, api)
	upload, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "ariadne_file_upload",
		Arguments: map[string]any{
			"target":      "radio",
			"local_path":  "/tmp/local.bin",
			"remote_path": `C:\Temp\remote.bin`,
			"overwrite":   true,
		},
	})
	if err != nil || upload.IsError {
		t.Fatalf("upload failed: result=%#v err=%v", upload, err)
	}
	var uploadOutput FileTransferOutput
	decodeStructuredOutput(t, upload.StructuredContent, &uploadOutput)
	if !uploadOutput.OK || uploadOutput.Size != 1234 || uploadOutput.SHA256 != "abcd" || api.fileTarget != "radio" || api.localPath != "/tmp/local.bin" || api.remotePath != `C:\Temp\remote.bin` || !api.overwrite {
		t.Fatalf("upload was not forwarded: output=%#v api=%#v", uploadOutput, api)
	}

	download, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "ariadne_file_download",
		Arguments: map[string]any{
			"target":      "radio",
			"remote_path": `C:\Temp\remote.bin`,
			"local_path":  "/tmp/download.bin",
		},
	})
	if err != nil || download.IsError {
		t.Fatalf("download failed: result=%#v err=%v", download, err)
	}
	if api.localPath != "/tmp/download.bin" || api.remotePath != `C:\Temp\remote.bin` || api.overwrite {
		t.Fatalf("download was not forwarded: api=%#v", api)
	}
}

func TestJobToolsUseDetachedAPIAndOutputCursors(t *testing.T) {
	now := time.Now().UTC()
	api := &fakeAPI{job: wire.JobInfo{ID: "j_test", State: "running", CreatedAt: now, StartedAt: now, ExitCode: -1}, jobOutput: wire.JobOutput{Stdout: []byte("hello"), Stderr: []byte("warn"), NextStdoutOffset: 5, NextStderrOffset: 4}}
	session := connectTestClient(t, api)
	started, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "ariadne_job_start", Arguments: map[string]any{"target": "radio", "command": "task build", "cwd": `C:\work`}})
	if err != nil || started.IsError {
		t.Fatalf("start failed: result=%#v err=%v", started, err)
	}
	if api.jobTarget != "radio" || api.jobRequest.Command != "task build" || api.jobRequest.Cwd != `C:\work` || api.jobRequest.TimeoutMillis != 0 {
		t.Fatalf("start was not forwarded: %#v", api)
	}

	read, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "ariadne_job_read", Arguments: map[string]any{"target": "radio", "job_id": "j_test", "stdout_offset": 2, "stderr_offset": 1, "limit": 4096}})
	if err != nil || read.IsError {
		t.Fatalf("read failed: result=%#v err=%v", read, err)
	}
	var output JobOutput
	decodeStructuredOutput(t, read.StructuredContent, &output)
	if output.Stdout != "hello" || output.Stderr != "warn" || output.NextStdoutOffset != 5 || api.stdoutOff != 2 || api.stderrOff != 1 || api.readLimit != 4096 {
		t.Fatalf("unexpected job read: output=%#v api=%#v", output, api)
	}
}

func TestToolAnnotationsDescribeMutationRisk(t *testing.T) {
	session := connectTestClient(t, &fakeAPI{})
	tools := make(map[string]*mcp.Tool)
	for tool, err := range session.Tools(context.Background(), nil) {
		if err != nil {
			t.Fatal(err)
		}
		tools[tool.Name] = tool
	}
	if len(tools) != 11 {
		t.Fatalf("got %d tools, want 11", len(tools))
	}
	if !tools["ariadne_nodes"].Annotations.ReadOnlyHint {
		t.Fatal("ariadne_nodes is not marked read-only")
	}
	if tools["ariadne_exec"].Annotations.ReadOnlyHint || tools["ariadne_exec"].Annotations.DestructiveHint == nil || !*tools["ariadne_exec"].Annotations.DestructiveHint {
		t.Fatal("ariadne_exec does not advertise its mutation risk")
	}
	if tools["ariadne_file_upload"].Annotations.DestructiveHint == nil || !*tools["ariadne_file_upload"].Annotations.DestructiveHint {
		t.Fatal("ariadne_file_upload does not advertise its mutation risk")
	}
	if !tools["ariadne_job_read"].Annotations.ReadOnlyHint || tools["ariadne_job_cancel"].Annotations.DestructiveHint == nil || !*tools["ariadne_job_cancel"].Annotations.DestructiveHint {
		t.Fatal("job tool annotations do not describe their risk")
	}
}

func connectTestClient(t *testing.T, api API) *mcp.ClientSession {
	t.Helper()
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := New(api, "test").Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "ariadne-test", Version: "test"}, nil)
	clientSession, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		serverSession.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = clientSession.Close()
		_ = serverSession.Close()
	})
	return clientSession
}

func decodeStructuredOutput(t *testing.T, value any, destination any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, destination); err != nil {
		t.Fatal(err)
	}
}
