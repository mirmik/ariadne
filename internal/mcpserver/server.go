package mcpserver

import (
	"context"
	"errors"
	"time"

	"github.com/mirmik/ariadne/internal/execspec"
	"github.com/mirmik/ariadne/internal/wire"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	defaultExecTimeout = 30 * time.Second
	maximumExecTimeout = 10 * time.Minute
	defaultFileTimeout = 10 * time.Minute
	maximumFileTimeout = time.Hour
	maximumJobTimeout  = 24 * time.Hour
	jobControlTimeout  = 30 * time.Second
	clientGrace        = 10 * time.Second
)

type API interface {
	Nodes(context.Context) ([]wire.NodeInfo, error)
	Claim(context.Context, string, string) (wire.NodeInfo, error)
	Exec(context.Context, string, wire.ExecRequest) (wire.ExecResult, error)
	UploadFile(context.Context, string, string, string, bool) (wire.FileTransferResult, error)
	DownloadFile(context.Context, string, string, string, bool) (wire.FileTransferResult, error)
	StartJob(context.Context, string, wire.ExecRequest) (wire.JobInfo, error)
	ListJobs(context.Context, string) ([]wire.JobInfo, error)
	JobStatus(context.Context, string, string) (wire.JobInfo, error)
	ReadJob(context.Context, string, string, int64, int64, int) (wire.JobInfo, wire.JobOutput, error)
	CancelJob(context.Context, string, string) (wire.JobInfo, error)
	RemoveJob(context.Context, string, string) error
}

type Node struct {
	ID               string   `json:"id" jsonschema:"stable node identity"`
	Alias            string   `json:"alias" jsonschema:"human-readable alias reported by the node"`
	AliasClaimed     bool     `json:"alias_claimed" jsonschema:"whether management explicitly confirmed the alias"`
	Platform         string   `json:"platform" jsonschema:"remote operating system"`
	Architecture     string   `json:"architecture" jsonschema:"remote CPU architecture"`
	ConnectorVersion string   `json:"connector_version" jsonschema:"Ariadne connector version"`
	ConnectedAt      string   `json:"connected_at" jsonschema:"connection time in RFC3339 format"`
	Online           bool     `json:"online" jsonschema:"whether the node is currently connected"`
	Capabilities     []string `json:"capabilities,omitempty" jsonschema:"optional connector capabilities"`
}

type NodesInput struct{}

type NodesOutput struct {
	Nodes []Node `json:"nodes"`
}

type ClaimInput struct {
	NodeID string `json:"node_id" jsonschema:"exact stable node ID returned by ariadne_nodes"`
	Alias  string `json:"alias" jsonschema:"trusted alias to assign to the node"`
}

type ClaimOutput struct {
	Node Node `json:"node"`
}

type ExecInput struct {
	Target        string   `json:"target" jsonschema:"claimed alias or exact node ID"`
	Command       string   `json:"command,omitempty" jsonschema:"preferred: command line interpreted by the remote platform shell"`
	Argv          []string `json:"argv,omitempty" jsonschema:"advanced alternative: executable followed by exact arguments, without shell interpretation"`
	Shell         string   `json:"shell,omitempty" jsonschema:"shell for command: auto (default), posix, powershell, or cmd"`
	Cwd           string   `json:"cwd,omitempty" jsonschema:"working directory on the remote node"`
	TimeoutMillis int64    `json:"timeout_ms,omitempty" jsonschema:"remote command timeout in milliseconds; defaults to 30000 and must not exceed 600000"`
}

type ExecOutput struct {
	OK              bool   `json:"ok"`
	ExitCode        int    `json:"exit_code"`
	Shell           string `json:"shell,omitempty"`
	Stdout          string `json:"stdout,omitempty"`
	Stderr          string `json:"stderr,omitempty"`
	Error           string `json:"error,omitempty"`
	TimedOut        bool   `json:"timed_out,omitempty"`
	StdoutTruncated bool   `json:"stdout_truncated,omitempty"`
	StderrTruncated bool   `json:"stderr_truncated,omitempty"`
	DurationMillis  int64  `json:"duration_ms"`
}

type FileUploadInput struct {
	Target        string `json:"target" jsonschema:"claimed alias or exact node ID"`
	LocalPath     string `json:"local_path" jsonschema:"source path on the MCP host"`
	RemotePath    string `json:"remote_path" jsonschema:"destination path on the remote node"`
	Overwrite     bool   `json:"overwrite,omitempty" jsonschema:"replace an existing destination atomically"`
	TimeoutMillis int64  `json:"timeout_ms,omitempty" jsonschema:"transfer timeout in milliseconds; defaults to 600000 and must not exceed 3600000"`
}

type FileDownloadInput struct {
	Target        string `json:"target" jsonschema:"claimed alias or exact node ID"`
	RemotePath    string `json:"remote_path" jsonschema:"source path on the remote node"`
	LocalPath     string `json:"local_path" jsonschema:"destination path on the MCP host"`
	Overwrite     bool   `json:"overwrite,omitempty" jsonschema:"replace an existing destination atomically"`
	TimeoutMillis int64  `json:"timeout_ms,omitempty" jsonschema:"transfer timeout in milliseconds; defaults to 600000 and must not exceed 3600000"`
}

type FileTransferOutput struct {
	OK         bool   `json:"ok"`
	Size       int64  `json:"size"`
	SHA256     string `json:"sha256"`
	LocalPath  string `json:"local_path"`
	RemotePath string `json:"remote_path"`
}

type JobStartInput struct {
	Target        string   `json:"target" jsonschema:"claimed alias or exact node ID"`
	Command       string   `json:"command,omitempty" jsonschema:"preferred: command line interpreted by the remote platform shell"`
	Argv          []string `json:"argv,omitempty" jsonschema:"advanced alternative: executable followed by exact arguments, without shell interpretation"`
	Shell         string   `json:"shell,omitempty" jsonschema:"shell for command: auto (default), posix, powershell, or cmd"`
	Cwd           string   `json:"cwd,omitempty" jsonschema:"working directory on the remote node"`
	TimeoutMillis int64    `json:"timeout_ms,omitempty" jsonschema:"optional job runtime limit in milliseconds; zero means no job timeout, maximum 86400000"`
}

type JobTargetInput struct {
	Target string `json:"target" jsonschema:"claimed alias or exact node ID"`
}

type JobIDInput struct {
	Target string `json:"target" jsonschema:"claimed alias or exact node ID"`
	JobID  string `json:"job_id" jsonschema:"background job ID returned by ariadne_job_start or ariadne_job_list"`
}

type JobReadInput struct {
	Target       string `json:"target" jsonschema:"claimed alias or exact node ID"`
	JobID        string `json:"job_id" jsonschema:"background job ID"`
	StdoutOffset int64  `json:"stdout_offset,omitempty" jsonschema:"next stdout byte offset; start with zero"`
	StderrOffset int64  `json:"stderr_offset,omitempty" jsonschema:"next stderr byte offset; start with zero"`
	Limit        int    `json:"limit,omitempty" jsonschema:"maximum bytes to read from each stream; defaults to 65536 and cannot exceed 262144"`
}

type Job struct {
	ID              string   `json:"id"`
	State           string   `json:"state"`
	Argv            []string `json:"argv,omitempty"`
	Command         string   `json:"command,omitempty"`
	Shell           string   `json:"shell,omitempty"`
	Cwd             string   `json:"cwd,omitempty"`
	CreatedAt       string   `json:"created_at"`
	StartedAt       string   `json:"started_at"`
	FinishedAt      string   `json:"finished_at,omitempty"`
	ExitCode        int      `json:"exit_code"`
	Error           string   `json:"error,omitempty"`
	StdoutSize      int64    `json:"stdout_size"`
	StderrSize      int64    `json:"stderr_size"`
	StdoutTruncated bool     `json:"stdout_truncated,omitempty"`
	StderrTruncated bool     `json:"stderr_truncated,omitempty"`
}

type JobOutput struct {
	Job              Job    `json:"job"`
	Stdout           string `json:"stdout,omitempty"`
	Stderr           string `json:"stderr,omitempty"`
	NextStdoutOffset int64  `json:"next_stdout_offset"`
	NextStderrOffset int64  `json:"next_stderr_offset"`
	StdoutEOF        bool   `json:"stdout_eof"`
	StderrEOF        bool   `json:"stderr_eof"`
}

type JobResult struct {
	Job Job `json:"job"`
}

type JobsResult struct {
	Jobs []Job `json:"jobs"`
}

type JobRemoveOutput struct {
	OK bool `json:"ok"`
}

type handlers struct {
	api API
}

func New(api API, version string) *mcp.Server {
	server := mcp.NewServer(
		&mcp.Implementation{Name: "ariadne", Version: version},
		&mcp.ServerOptions{Instructions: "Use ariadne_nodes before choosing a target. Prefer ariadne_exec command for short work. Use ariadne_job_start for long-running work, then poll with ariadne_job_status or cursor through output with ariadne_job_read; jobs continue across relay disconnects while the connector remains running. Use cwd instead of embedding cd when practical. Use argv only when exact no-shell argument boundaries matter. Use ariadne_file_upload and ariadne_file_download for path-to-path transfer so file bytes do not pass through model context. Treat exec, job, and file tools as remote operations with the connected user's permissions; preserve the user's authorization boundaries and inspect before mutating."},
	)
	h := &handlers{api: api}
	mcp.AddTool(server, &mcp.Tool{
		Name:        "ariadne_nodes",
		Title:       "List Ariadne nodes",
		Description: "List nodes currently connected to the Ariadne relay, including trusted alias state, platform, architecture, connector version, and stable node ID.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: boolPointer(false), DestructiveHint: boolPointer(false)},
	}, h.nodes)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "ariadne_claim",
		Title:       "Claim Ariadne node alias",
		Description: "Assign a trusted alias to an exact Ariadne node ID. Use only after identifying the intended live node with ariadne_nodes.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, IdempotentHint: true, OpenWorldHint: boolPointer(false), DestructiveHint: boolPointer(false)},
	}, h.claim)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "ariadne_exec",
		Title:       "Execute command on Ariadne node",
		Description: "Execute a command string through the connected node's native shell (PowerShell on Windows, POSIX shell elsewhere), or optionally execute an exact argv vector without a shell. Returns the selected shell and structured stdout, stderr, exit status, duration, timeout, and truncation flags. This may modify the remote machine.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, IdempotentHint: false, OpenWorldHint: boolPointer(true), DestructiveHint: boolPointer(true)},
	}, h.exec)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "ariadne_file_upload",
		Title:       "Upload file to Ariadne node",
		Description: "Stream a regular file from a path on the MCP host to a path on a connected node. The destination is published atomically after size and SHA-256 verification; overwrite is explicit.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, IdempotentHint: false, OpenWorldHint: boolPointer(false), DestructiveHint: boolPointer(true)},
	}, h.fileUpload)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "ariadne_file_download",
		Title:       "Download file from Ariadne node",
		Description: "Stream a regular file from a connected node to a path on the MCP host. The local destination is published atomically after size and SHA-256 verification; overwrite is explicit.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, IdempotentHint: false, OpenWorldHint: boolPointer(false), DestructiveHint: boolPointer(true)},
	}, h.fileDownload)
	mcp.AddTool(server, &mcp.Tool{Name: "ariadne_job_start", Title: "Start background job on Ariadne node", Description: "Start a connector-owned background command and return immediately. The job and its capped stdout/stderr spool survive relay disconnects while the connector process remains running.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, IdempotentHint: false, OpenWorldHint: boolPointer(true), DestructiveHint: boolPointer(true)}}, h.jobStart)
	mcp.AddTool(server, &mcp.Tool{Name: "ariadne_job_list", Title: "List background jobs on Ariadne node", Description: "List running and retained completed jobs owned by the connector.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: boolPointer(false), DestructiveHint: boolPointer(false)}}, h.jobList)
	mcp.AddTool(server, &mcp.Tool{Name: "ariadne_job_status", Title: "Get Ariadne background job status", Description: "Get current state, exit status, and output sizes for one connector-owned job.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: boolPointer(false), DestructiveHint: boolPointer(false)}}, h.jobStatus)
	mcp.AddTool(server, &mcp.Tool{Name: "ariadne_job_read", Title: "Read Ariadne background job output", Description: "Read bounded stdout and stderr chunks using independent byte cursors. Continue with the returned offsets until both EOF flags are true.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: boolPointer(false), DestructiveHint: boolPointer(false)}}, h.jobRead)
	mcp.AddTool(server, &mcp.Tool{Name: "ariadne_job_cancel", Title: "Cancel Ariadne background job", Description: "Request cancellation of a running connector-owned job.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, IdempotentHint: false, OpenWorldHint: boolPointer(false), DestructiveHint: boolPointer(true)}}, h.jobCancel)
	mcp.AddTool(server, &mcp.Tool{Name: "ariadne_job_remove", Title: "Remove Ariadne background job", Description: "Remove a completed job and its retained stdout/stderr spool. A running job must be canceled first.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, IdempotentHint: false, OpenWorldHint: boolPointer(false), DestructiveHint: boolPointer(true)}}, h.jobRemove)
	return server
}

func (h *handlers) nodes(ctx context.Context, _ *mcp.CallToolRequest, _ NodesInput) (*mcp.CallToolResult, NodesOutput, error) {
	nodes, err := h.api.Nodes(ctx)
	if err != nil {
		return nil, NodesOutput{}, err
	}
	output := NodesOutput{Nodes: make([]Node, 0, len(nodes))}
	for _, node := range nodes {
		output.Nodes = append(output.Nodes, nodeOutput(node))
	}
	return nil, output, nil
}

func (h *handlers) claim(ctx context.Context, _ *mcp.CallToolRequest, input ClaimInput) (*mcp.CallToolResult, ClaimOutput, error) {
	if input.NodeID == "" || input.Alias == "" {
		return nil, ClaimOutput{}, errors.New("node_id and alias are required")
	}
	node, err := h.api.Claim(ctx, input.NodeID, input.Alias)
	if err != nil {
		return nil, ClaimOutput{}, err
	}
	return nil, ClaimOutput{Node: nodeOutput(node)}, nil
}

func (h *handlers) exec(ctx context.Context, _ *mcp.CallToolRequest, input ExecInput) (*mcp.CallToolResult, ExecOutput, error) {
	if input.Target == "" {
		return nil, ExecOutput{}, errors.New("target is required")
	}
	execRequest := wire.ExecRequest{
		Command: input.Command,
		Argv:    append([]string(nil), input.Argv...),
		Shell:   input.Shell,
		Cwd:     input.Cwd,
	}
	if err := execspec.Validate(execRequest); err != nil {
		return nil, ExecOutput{}, err
	}
	timeout := time.Duration(input.TimeoutMillis) * time.Millisecond
	if input.TimeoutMillis == 0 {
		timeout = defaultExecTimeout
	}
	if timeout < time.Millisecond || timeout > maximumExecTimeout {
		return nil, ExecOutput{}, errors.New("timeout_ms must be between 1 and 600000")
	}
	requestContext, cancel := context.WithTimeout(ctx, timeout+clientGrace)
	defer cancel()
	execRequest.TimeoutMillis = timeout.Milliseconds()
	result, err := h.api.Exec(requestContext, input.Target, execRequest)
	if err != nil {
		return nil, ExecOutput{}, err
	}
	output := ExecOutput{
		OK:              result.Error == "" && result.ExitCode == 0,
		ExitCode:        result.ExitCode,
		Shell:           result.Shell,
		Stdout:          string(result.Stdout),
		Stderr:          string(result.Stderr),
		Error:           result.Error,
		TimedOut:        result.TimedOut,
		StdoutTruncated: result.StdoutTruncated,
		StderrTruncated: result.StderrTruncated,
		DurationMillis:  result.DurationMillis,
	}
	return nil, output, nil
}

func (h *handlers) fileUpload(ctx context.Context, _ *mcp.CallToolRequest, input FileUploadInput) (*mcp.CallToolResult, FileTransferOutput, error) {
	if input.Target == "" || input.LocalPath == "" || input.RemotePath == "" {
		return nil, FileTransferOutput{}, errors.New("target, local_path, and remote_path are required")
	}
	timeout, err := fileTimeout(input.TimeoutMillis)
	if err != nil {
		return nil, FileTransferOutput{}, err
	}
	requestContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	result, err := h.api.UploadFile(requestContext, input.Target, input.LocalPath, input.RemotePath, input.Overwrite)
	if err != nil {
		return nil, FileTransferOutput{}, err
	}
	return nil, FileTransferOutput{OK: true, Size: result.Size, SHA256: result.SHA256, LocalPath: input.LocalPath, RemotePath: input.RemotePath}, nil
}

func (h *handlers) fileDownload(ctx context.Context, _ *mcp.CallToolRequest, input FileDownloadInput) (*mcp.CallToolResult, FileTransferOutput, error) {
	if input.Target == "" || input.LocalPath == "" || input.RemotePath == "" {
		return nil, FileTransferOutput{}, errors.New("target, local_path, and remote_path are required")
	}
	timeout, err := fileTimeout(input.TimeoutMillis)
	if err != nil {
		return nil, FileTransferOutput{}, err
	}
	requestContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	result, err := h.api.DownloadFile(requestContext, input.Target, input.RemotePath, input.LocalPath, input.Overwrite)
	if err != nil {
		return nil, FileTransferOutput{}, err
	}
	return nil, FileTransferOutput{OK: true, Size: result.Size, SHA256: result.SHA256, LocalPath: input.LocalPath, RemotePath: input.RemotePath}, nil
}

func (h *handlers) jobStart(ctx context.Context, _ *mcp.CallToolRequest, input JobStartInput) (*mcp.CallToolResult, JobResult, error) {
	if input.Target == "" {
		return nil, JobResult{}, errors.New("target is required")
	}
	request := wire.ExecRequest{Command: input.Command, Argv: append([]string(nil), input.Argv...), Shell: input.Shell, Cwd: input.Cwd, TimeoutMillis: input.TimeoutMillis}
	if err := execspec.Validate(request); err != nil {
		return nil, JobResult{}, err
	}
	if input.TimeoutMillis < 0 || input.TimeoutMillis > maximumJobTimeout.Milliseconds() {
		return nil, JobResult{}, errors.New("timeout_ms must be between 0 and 86400000")
	}
	requestContext, cancel := context.WithTimeout(ctx, jobControlTimeout)
	defer cancel()
	job, err := h.api.StartJob(requestContext, input.Target, request)
	if err != nil {
		return nil, JobResult{}, err
	}
	return nil, JobResult{Job: jobOutput(job)}, nil
}

func (h *handlers) jobList(ctx context.Context, _ *mcp.CallToolRequest, input JobTargetInput) (*mcp.CallToolResult, JobsResult, error) {
	if input.Target == "" {
		return nil, JobsResult{}, errors.New("target is required")
	}
	requestContext, cancel := context.WithTimeout(ctx, jobControlTimeout)
	defer cancel()
	jobs, err := h.api.ListJobs(requestContext, input.Target)
	if err != nil {
		return nil, JobsResult{}, err
	}
	output := JobsResult{Jobs: make([]Job, 0, len(jobs))}
	for _, job := range jobs {
		output.Jobs = append(output.Jobs, jobOutput(job))
	}
	return nil, output, nil
}

func (h *handlers) jobStatus(ctx context.Context, _ *mcp.CallToolRequest, input JobIDInput) (*mcp.CallToolResult, JobResult, error) {
	if err := validateJobIDInput(input); err != nil {
		return nil, JobResult{}, err
	}
	requestContext, cancel := context.WithTimeout(ctx, jobControlTimeout)
	defer cancel()
	job, err := h.api.JobStatus(requestContext, input.Target, input.JobID)
	if err != nil {
		return nil, JobResult{}, err
	}
	return nil, JobResult{Job: jobOutput(job)}, nil
}

func (h *handlers) jobRead(ctx context.Context, _ *mcp.CallToolRequest, input JobReadInput) (*mcp.CallToolResult, JobOutput, error) {
	if input.Target == "" || input.JobID == "" {
		return nil, JobOutput{}, errors.New("target and job_id are required")
	}
	if input.StdoutOffset < 0 || input.StderrOffset < 0 || input.Limit < 0 || input.Limit > 262144 {
		return nil, JobOutput{}, errors.New("offsets must be non-negative and limit must not exceed 262144")
	}
	requestContext, cancel := context.WithTimeout(ctx, jobControlTimeout)
	defer cancel()
	job, output, err := h.api.ReadJob(requestContext, input.Target, input.JobID, input.StdoutOffset, input.StderrOffset, input.Limit)
	if err != nil {
		return nil, JobOutput{}, err
	}
	return nil, JobOutput{Job: jobOutput(job), Stdout: string(output.Stdout), Stderr: string(output.Stderr), NextStdoutOffset: output.NextStdoutOffset, NextStderrOffset: output.NextStderrOffset, StdoutEOF: output.StdoutEOF, StderrEOF: output.StderrEOF}, nil
}

func (h *handlers) jobCancel(ctx context.Context, _ *mcp.CallToolRequest, input JobIDInput) (*mcp.CallToolResult, JobResult, error) {
	if err := validateJobIDInput(input); err != nil {
		return nil, JobResult{}, err
	}
	requestContext, cancel := context.WithTimeout(ctx, jobControlTimeout)
	defer cancel()
	job, err := h.api.CancelJob(requestContext, input.Target, input.JobID)
	if err != nil {
		return nil, JobResult{}, err
	}
	return nil, JobResult{Job: jobOutput(job)}, nil
}

func (h *handlers) jobRemove(ctx context.Context, _ *mcp.CallToolRequest, input JobIDInput) (*mcp.CallToolResult, JobRemoveOutput, error) {
	if err := validateJobIDInput(input); err != nil {
		return nil, JobRemoveOutput{}, err
	}
	requestContext, cancel := context.WithTimeout(ctx, jobControlTimeout)
	defer cancel()
	if err := h.api.RemoveJob(requestContext, input.Target, input.JobID); err != nil {
		return nil, JobRemoveOutput{}, err
	}
	return nil, JobRemoveOutput{OK: true}, nil
}

func validateJobIDInput(input JobIDInput) error {
	if input.Target == "" || input.JobID == "" {
		return errors.New("target and job_id are required")
	}
	return nil
}

func fileTimeout(milliseconds int64) (time.Duration, error) {
	if milliseconds == 0 {
		return defaultFileTimeout, nil
	}
	timeout := time.Duration(milliseconds) * time.Millisecond
	if timeout < time.Millisecond || timeout > maximumFileTimeout {
		return 0, errors.New("timeout_ms must be between 1 and 3600000")
	}
	return timeout, nil
}

func nodeOutput(node wire.NodeInfo) Node {
	return Node{
		ID:               node.ID,
		Alias:            node.Alias,
		AliasClaimed:     node.AliasClaimed,
		Platform:         node.Platform,
		Architecture:     node.Architecture,
		ConnectorVersion: node.ConnectorVersion,
		ConnectedAt:      node.ConnectedAt.Format(time.RFC3339),
		Online:           node.Online,
		Capabilities:     append([]string(nil), node.Capabilities...),
	}
}

func jobOutput(job wire.JobInfo) Job {
	finishedAt := ""
	if !job.FinishedAt.IsZero() {
		finishedAt = job.FinishedAt.Format(time.RFC3339Nano)
	}
	return Job{ID: job.ID, State: job.State, Argv: append([]string(nil), job.Argv...), Command: job.Command, Shell: job.Shell, Cwd: job.Cwd, CreatedAt: job.CreatedAt.Format(time.RFC3339Nano), StartedAt: job.StartedAt.Format(time.RFC3339Nano), FinishedAt: finishedAt, ExitCode: job.ExitCode, Error: job.Error, StdoutSize: job.StdoutSize, StderrSize: job.StderrSize, StdoutTruncated: job.StdoutTruncated, StderrTruncated: job.StderrTruncated}
}

func boolPointer(value bool) *bool {
	return &value
}
