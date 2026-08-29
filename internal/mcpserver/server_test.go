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

func TestToolAnnotationsDescribeMutationRisk(t *testing.T) {
	session := connectTestClient(t, &fakeAPI{})
	tools := make(map[string]*mcp.Tool)
	for tool, err := range session.Tools(context.Background(), nil) {
		if err != nil {
			t.Fatal(err)
		}
		tools[tool.Name] = tool
	}
	if len(tools) != 3 {
		t.Fatalf("got %d tools, want 3", len(tools))
	}
	if !tools["ariadne_nodes"].Annotations.ReadOnlyHint {
		t.Fatal("ariadne_nodes is not marked read-only")
	}
	if tools["ariadne_exec"].Annotations.ReadOnlyHint || tools["ariadne_exec"].Annotations.DestructiveHint == nil || !*tools["ariadne_exec"].Annotations.DestructiveHint {
		t.Fatal("ariadne_exec does not advertise its mutation risk")
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
