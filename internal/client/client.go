package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/coder/websocket"
	"github.com/mirmik/ariadne/internal/managementauth"
	"github.com/mirmik/ariadne/internal/wire"
)

type Config struct {
	RelayURL        string
	ManagementToken string
	HTTPClient      *http.Client
}

type Client struct {
	baseURL    *url.URL
	httpClient *http.Client
	token      string
}

type HTTPError struct {
	StatusCode int
	Message    string
}

type StreamPeer struct {
	NodeID     string
	SSHHostKey string
}

func (err *HTTPError) Error() string {
	return fmt.Sprintf("relay returned HTTP %d: %s", err.StatusCode, err.Message)
}

func New(config Config) (*Client, error) {
	baseURL, err := parseRelayURL(config.RelayURL)
	if err != nil {
		return nil, err
	}
	if err := managementauth.Validate(config.ManagementToken); err != nil {
		return nil, fmt.Errorf("management token: %w", err)
	}
	httpClient := config.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{baseURL: baseURL, httpClient: httpClient, token: config.ManagementToken}, nil
}

func (client *Client) Nodes(ctx context.Context) ([]wire.NodeInfo, error) {
	request, err := client.request(ctx, http.MethodGet, "/v1/nodes", nil)
	if err != nil {
		return nil, err
	}
	response, err := client.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	defer response.Body.Close()
	if err := decodeHTTPError(response); err != nil {
		return nil, err
	}
	var nodesResponse wire.NodesResponse
	if err := decodeJSON(response.Body, &nodesResponse); err != nil {
		return nil, fmt.Errorf("decode node list: %w", err)
	}
	return nodesResponse.Nodes, nil
}

func (client *Client) Claim(ctx context.Context, nodeID, alias string) (wire.NodeInfo, error) {
	if nodeID == "" || alias == "" {
		return wire.NodeInfo{}, errors.New("node ID and alias are required")
	}
	body, err := json.Marshal(wire.ClaimRequest{Alias: alias})
	if err != nil {
		return wire.NodeInfo{}, fmt.Errorf("encode claim request: %w", err)
	}
	path := "/v1/nodes/" + url.PathEscape(nodeID) + "/claim"
	request, err := client.request(ctx, http.MethodPost, path, bytes.NewReader(body))
	if err != nil {
		return wire.NodeInfo{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.httpClient.Do(request)
	if err != nil {
		return wire.NodeInfo{}, fmt.Errorf("claim node alias: %w", err)
	}
	defer response.Body.Close()
	if err := decodeHTTPError(response); err != nil {
		return wire.NodeInfo{}, err
	}
	var node wire.NodeInfo
	if err := decodeJSON(response.Body, &node); err != nil {
		return wire.NodeInfo{}, fmt.Errorf("decode claimed node: %w", err)
	}
	return node, nil
}

func (client *Client) Exec(ctx context.Context, target string, execRequest wire.ExecRequest) (wire.ExecResult, error) {
	if target == "" {
		return wire.ExecResult{}, errors.New("target is required")
	}
	body, err := json.Marshal(execRequest)
	if err != nil {
		return wire.ExecResult{}, fmt.Errorf("encode exec request: %w", err)
	}
	path := "/v1/nodes/" + url.PathEscape(target) + "/exec"
	request, err := client.request(ctx, http.MethodPost, path, bytes.NewReader(body))
	if err != nil {
		return wire.ExecResult{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.httpClient.Do(request)
	if err != nil {
		return wire.ExecResult{}, fmt.Errorf("execute command: %w", err)
	}
	defer response.Body.Close()
	if err := decodeHTTPError(response); err != nil {
		return wire.ExecResult{}, err
	}
	var result wire.ExecResult
	if err := decodeJSON(response.Body, &result); err != nil {
		return wire.ExecResult{}, fmt.Errorf("decode exec result: %w", err)
	}
	return result, nil
}

func (client *Client) DialStream(ctx context.Context, target, protocol string) (*websocket.Conn, error) {
	connection, _, err := client.dialStream(ctx, target, protocol, nil)
	return connection, err
}

func (client *Client) DialShellStream(ctx context.Context, target, sshClientPublicKey string) (*websocket.Conn, StreamPeer, error) {
	if sshClientPublicKey == "" {
		return nil, StreamPeer{}, errors.New("SSH session public key is required")
	}
	headers := make(http.Header)
	headers.Set(wire.HeaderSSHClientKey, sshClientPublicKey)
	connection, response, err := client.dialStream(ctx, target, "shell", headers)
	if err != nil {
		return nil, StreamPeer{}, err
	}
	peer := StreamPeer{
		NodeID:     response.Header.Get(wire.HeaderNodeID),
		SSHHostKey: response.Header.Get(wire.HeaderSSHHostKey),
	}
	if peer.NodeID == "" || peer.SSHHostKey == "" {
		connection.CloseNow()
		return nil, StreamPeer{}, errors.New("relay did not identify the shell stream peer")
	}
	return connection, peer, nil
}

func (client *Client) dialStream(ctx context.Context, target, protocol string, extraHeaders http.Header) (*websocket.Conn, *http.Response, error) {
	if target == "" {
		return nil, nil, errors.New("target is required")
	}
	if protocol == "" {
		return nil, nil, errors.New("stream protocol is required")
	}
	endpoint := client.endpoint("/v1/nodes/" + url.PathEscape(target) + "/streams/" + url.PathEscape(protocol))
	headers := extraHeaders.Clone()
	if headers == nil {
		headers = make(http.Header)
	}
	headers.Set("Authorization", "Bearer "+client.token)
	connection, response, err := websocket.Dial(ctx, endpoint.String(), &websocket.DialOptions{
		HTTPClient:      client.httpClient,
		HTTPHeader:      headers,
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		if response != nil {
			return nil, response, fmt.Errorf("open %s stream: relay returned HTTP %d: %w", protocol, response.StatusCode, err)
		}
		return nil, nil, fmt.Errorf("open %s stream: %w", protocol, err)
	}
	connection.SetReadLimit(wire.MaxStreamPayloadSize)
	return connection, response, nil
}

func (client *Client) request(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	request, err := http.NewRequestWithContext(ctx, method, client.endpoint(path).String(), body)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+client.token)
	return request, nil
}

func (client *Client) endpoint(path string) *url.URL {
	endpoint := *client.baseURL
	endpoint.Path = strings.TrimSuffix(client.baseURL.Path, "/") + path
	return &endpoint
}

func parseRelayURL(raw string) (*url.URL, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, errors.New("relay URL is required")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse relay URL: %w", err)
	}
	switch parsed.Scheme {
	case "http", "https":
	case "ws":
		parsed.Scheme = "http"
	case "wss":
		parsed.Scheme = "https"
	default:
		return nil, errors.New("relay URL scheme must be http, https, ws, or wss")
	}
	if parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("relay URL must contain a host and no credentials, query, or fragment")
	}
	parsed.Path = strings.TrimSuffix(parsed.Path, "/")
	return parsed, nil
}

func decodeHTTPError(response *http.Response) error {
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return nil
	}
	var apiError wire.APIError
	decoder := json.NewDecoder(io.LimitReader(response.Body, 64<<10))
	if err := decoder.Decode(&apiError); err != nil || apiError.Error == "" {
		apiError.Error = http.StatusText(response.StatusCode)
	}
	return &HTTPError{StatusCode: response.StatusCode, Message: apiError.Error}
}

func decodeJSON(reader io.Reader, destination any) error {
	data, err := io.ReadAll(io.LimitReader(reader, wire.MaxControlMessageSize+1))
	if err != nil {
		return err
	}
	if len(data) > wire.MaxControlMessageSize {
		return fmt.Errorf("JSON response exceeds %d bytes", wire.MaxControlMessageSize)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("unexpected data after JSON value")
	}
	return nil
}
