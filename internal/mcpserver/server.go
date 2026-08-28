package mcpserver

import (
	"context"
	"errors"
	"time"

	"github.com/mirmik/ariadne/internal/wire"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	defaultExecTimeout = 30 * time.Second
	maximumExecTimeout = 10 * time.Minute
	clientGrace        = 10 * time.Second
)

type API interface {
	Nodes(context.Context) ([]wire.NodeInfo, error)
	Claim(context.Context, string, string) (wire.NodeInfo, error)
	Exec(context.Context, string, wire.ExecRequest) (wire.ExecResult, error)
}

type Node struct {
	ID               string `json:"id" jsonschema:"stable node identity"`
	Alias            string `json:"alias" jsonschema:"human-readable alias reported by the node"`
	AliasClaimed     bool   `json:"alias_claimed" jsonschema:"whether management explicitly confirmed the alias"`
	Platform         string `json:"platform" jsonschema:"remote operating system"`
	Architecture     string `json:"architecture" jsonschema:"remote CPU architecture"`
	ConnectorVersion string `json:"connector_version" jsonschema:"Ariadne connector version"`
	ConnectedAt      string `json:"connected_at" jsonschema:"connection time in RFC3339 format"`
	Online           bool   `json:"online" jsonschema:"whether the node is currently connected"`
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
	Argv          []string `json:"argv" jsonschema:"executable followed by exact arguments; shell operators are not interpreted"`
	Cwd           string   `json:"cwd,omitempty" jsonschema:"working directory on the remote node"`
	TimeoutMillis int64    `json:"timeout_ms,omitempty" jsonschema:"remote command timeout in milliseconds; defaults to 30000 and must not exceed 600000"`
}

type ExecOutput struct {
	OK              bool   `json:"ok"`
	ExitCode        int    `json:"exit_code"`
	Stdout          string `json:"stdout,omitempty"`
	Stderr          string `json:"stderr,omitempty"`
	Error           string `json:"error,omitempty"`
	TimedOut        bool   `json:"timed_out,omitempty"`
	StdoutTruncated bool   `json:"stdout_truncated,omitempty"`
	StderrTruncated bool   `json:"stderr_truncated,omitempty"`
	DurationMillis  int64  `json:"duration_ms"`
}

type handlers struct {
	api API
}

func New(api API, version string) *mcp.Server {
	server := mcp.NewServer(
		&mcp.Implementation{Name: "ariadne", Version: version},
		&mcp.ServerOptions{Instructions: "Use ariadne_nodes before choosing a target. ariadne_exec runs argv directly: shell operators are not interpreted, so prefer cwd and exact arguments. Use a platform shell explicitly only when shell syntax is required. Treat ariadne_exec as remote code execution with the connected user's permissions; preserve the user's authorization boundaries and inspect before mutating."},
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
		Description: "Execute an argv vector on a connected node with an optional working directory and timeout. Returns structured stdout, stderr, exit status, duration, timeout, and truncation flags. This may modify the remote machine.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, IdempotentHint: false, OpenWorldHint: boolPointer(true), DestructiveHint: boolPointer(true)},
	}, h.exec)
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
	if len(input.Argv) == 0 || input.Argv[0] == "" {
		return nil, ExecOutput{}, errors.New("argv must contain a non-empty executable")
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
	result, err := h.api.Exec(requestContext, input.Target, wire.ExecRequest{
		Argv:          append([]string(nil), input.Argv...),
		Cwd:           input.Cwd,
		TimeoutMillis: timeout.Milliseconds(),
	})
	if err != nil {
		return nil, ExecOutput{}, err
	}
	output := ExecOutput{
		OK:              result.Error == "" && result.ExitCode == 0,
		ExitCode:        result.ExitCode,
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
	}
}

func boolPointer(value bool) *bool {
	return &value
}
