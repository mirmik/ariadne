package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/coder/websocket"
	"github.com/mirmik/ariadne/internal/filetransfer"
	"github.com/mirmik/ariadne/internal/managementauth"
	"github.com/mirmik/ariadne/internal/wire"
)

const DefaultMaxDownloadBytes int64 = 1 << 30

type Config struct {
	RelayURL         string
	ManagementToken  string
	HTTPClient       *http.Client
	MaxDownloadBytes int64
}

type Client struct {
	baseURL          *url.URL
	httpClient       *http.Client
	token            string
	maxDownloadBytes int64
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
	if config.MaxDownloadBytes < 0 {
		return nil, errors.New("maximum download size must be non-negative")
	}
	if config.MaxDownloadBytes == 0 {
		config.MaxDownloadBytes = DefaultMaxDownloadBytes
	}
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
	return &Client{baseURL: baseURL, httpClient: httpClient, token: config.ManagementToken, maxDownloadBytes: config.MaxDownloadBytes}, nil
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

func (client *Client) OpenPairing(ctx context.Context) (wire.PairingOpenResponse, error) {
	request, err := client.request(ctx, http.MethodPost, "/v1/pairing", nil)
	if err != nil {
		return wire.PairingOpenResponse{}, err
	}
	response, err := client.httpClient.Do(request)
	if err != nil {
		return wire.PairingOpenResponse{}, fmt.Errorf("open relay pairing: %w", err)
	}
	defer response.Body.Close()
	if err := decodeHTTPError(response); err != nil {
		return wire.PairingOpenResponse{}, err
	}
	var result wire.PairingOpenResponse
	if err := decodeJSON(response.Body, &result); err != nil {
		return wire.PairingOpenResponse{}, fmt.Errorf("decode relay pairing response: %w", err)
	}
	return result, nil
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

func (client *Client) Revoke(ctx context.Context, nodeID string) error {
	if nodeID == "" {
		return errors.New("node ID is required")
	}
	path := "/v1/nodes/" + url.PathEscape(nodeID) + "/revoke"
	request, err := client.request(ctx, http.MethodPost, path, nil)
	if err != nil {
		return err
	}
	response, err := client.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("revoke node identity: %w", err)
	}
	defer response.Body.Close()
	return decodeHTTPError(response)
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

func (client *Client) StartJob(ctx context.Context, target string, execRequest wire.ExecRequest) (wire.JobInfo, error) {
	body, err := json.Marshal(execRequest)
	if err != nil {
		return wire.JobInfo{}, fmt.Errorf("encode job request: %w", err)
	}
	response, err := client.doJobRequest(ctx, http.MethodPost, target, "jobs", bytes.NewReader(body), nil)
	if err != nil {
		return wire.JobInfo{}, err
	}
	if response.Job == nil {
		return wire.JobInfo{}, errors.New("relay returned no job")
	}
	return *response.Job, nil
}

func (client *Client) ListJobs(ctx context.Context, target string) ([]wire.JobInfo, error) {
	response, err := client.doJobRequest(ctx, http.MethodGet, target, "jobs", nil, nil)
	if err != nil {
		return nil, err
	}
	return response.Jobs, nil
}

func (client *Client) JobStatus(ctx context.Context, target, jobID string) (wire.JobInfo, error) {
	response, err := client.doJobRequest(ctx, http.MethodGet, target, "jobs/"+url.PathEscape(jobID), nil, nil)
	if err != nil {
		return wire.JobInfo{}, err
	}
	if response.Job == nil {
		return wire.JobInfo{}, errors.New("relay returned no job")
	}
	return *response.Job, nil
}

func (client *Client) ReadJob(ctx context.Context, target, jobID string, stdoutOffset, stderrOffset int64, limit int) (wire.JobInfo, wire.JobOutput, error) {
	query := url.Values{
		"stdout_offset": {strconv.FormatInt(stdoutOffset, 10)},
		"stderr_offset": {strconv.FormatInt(stderrOffset, 10)},
	}
	if limit != 0 {
		query.Set("limit", strconv.Itoa(limit))
	}
	response, err := client.doJobRequest(ctx, http.MethodGet, target, "jobs/"+url.PathEscape(jobID)+"/output", nil, query)
	if err != nil {
		return wire.JobInfo{}, wire.JobOutput{}, err
	}
	if response.Job == nil || response.Output == nil {
		return wire.JobInfo{}, wire.JobOutput{}, errors.New("relay returned incomplete job output")
	}
	return *response.Job, *response.Output, nil
}

func (client *Client) CancelJob(ctx context.Context, target, jobID string) (wire.JobInfo, error) {
	response, err := client.doJobRequest(ctx, http.MethodPost, target, "jobs/"+url.PathEscape(jobID)+"/cancel", nil, nil)
	if err != nil {
		return wire.JobInfo{}, err
	}
	if response.Job == nil {
		return wire.JobInfo{}, errors.New("relay returned no job")
	}
	return *response.Job, nil
}

func (client *Client) RemoveJob(ctx context.Context, target, jobID string) error {
	_, err := client.doJobRequest(ctx, http.MethodDelete, target, "jobs/"+url.PathEscape(jobID), nil, nil)
	return err
}

func (client *Client) doJobRequest(ctx context.Context, method, target, action string, body io.Reader, query url.Values) (wire.JobResponse, error) {
	if target == "" || action == "" {
		return wire.JobResponse{}, errors.New("job target and action are required")
	}
	path := "/v1/nodes/" + url.PathEscape(target) + "/" + action
	request, err := client.request(ctx, method, path, body)
	if err != nil {
		return wire.JobResponse{}, err
	}
	request.URL.RawQuery = query.Encode()
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.httpClient.Do(request)
	if err != nil {
		return wire.JobResponse{}, fmt.Errorf("job request: %w", err)
	}
	defer response.Body.Close()
	if err := decodeHTTPError(response); err != nil {
		return wire.JobResponse{}, err
	}
	var result wire.JobResponse
	if err := decodeJSON(response.Body, &result); err != nil {
		return wire.JobResponse{}, fmt.Errorf("decode job response: %w", err)
	}
	return result, nil
}

func (client *Client) DialStream(ctx context.Context, target, protocol string) (*websocket.Conn, error) {
	connection, _, err := client.dialStream(ctx, target, protocol, nil)
	return connection, err
}

func (client *Client) UploadFile(ctx context.Context, target, localPath, remotePath string, overwrite bool) (wire.FileTransferResult, error) {
	file, err := os.Open(localPath)
	if err != nil {
		return wire.FileTransferResult{}, fmt.Errorf("open local upload: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return wire.FileTransferResult{}, fmt.Errorf("stat local upload: %w", err)
	}
	if !info.Mode().IsRegular() {
		return wire.FileTransferResult{}, errors.New("local upload path is not a regular file")
	}
	query := url.Values{
		"path":      {remotePath},
		"overwrite": {strconv.FormatBool(overwrite)},
		"mode":      {strconv.FormatUint(uint64(info.Mode().Perm()), 8)},
	}
	connection, _, err := client.dialStreamWithQuery(ctx, target, "file-upload", nil, query)
	if err != nil {
		return wire.FileTransferResult{}, err
	}
	defer connection.CloseNow()

	hash := sha256.New()
	buffer := make([]byte, 32<<10)
	var size int64
	for {
		count, readErr := file.Read(buffer)
		if count > 0 {
			size += int64(count)
			_, _ = hash.Write(buffer[:count])
			frame, encodeErr := wire.EncodeFileData(buffer[:count])
			if encodeErr != nil {
				return wire.FileTransferResult{}, encodeErr
			}
			if err := connection.Write(ctx, websocket.MessageBinary, frame); err != nil {
				return wire.FileTransferResult{}, fmt.Errorf("send upload data: %w", err)
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return wire.FileTransferResult{}, fmt.Errorf("read local upload: %w", readErr)
		}
	}
	localResult := wire.FileTransferResult{Size: size, SHA256: hex.EncodeToString(hash.Sum(nil))}
	complete, err := wire.EncodeFileComplete(localResult)
	if err != nil {
		return wire.FileTransferResult{}, err
	}
	if err := connection.Write(ctx, websocket.MessageBinary, complete); err != nil {
		return wire.FileTransferResult{}, fmt.Errorf("finish upload: %w", err)
	}
	remoteResult, err := readFileResult(ctx, connection, nil)
	if err != nil {
		return remoteResult, err
	}
	if remoteResult.Size != localResult.Size || !strings.EqualFold(remoteResult.SHA256, localResult.SHA256) {
		return remoteResult, errors.New("remote upload verification differs from local file")
	}
	_ = connection.Close(websocket.StatusNormalClosure, "upload complete")
	return remoteResult, nil
}

func (client *Client) DownloadFile(ctx context.Context, target, remotePath, localPath string, overwrite bool) (wire.FileTransferResult, error) {
	writer, err := filetransfer.NewAtomicWriter(localPath, overwrite, 0o600)
	if err != nil {
		return wire.FileTransferResult{}, err
	}
	defer writer.Abort()
	query := url.Values{"path": {remotePath}}
	connection, _, err := client.dialStreamWithQuery(ctx, target, "file-download", nil, query)
	if err != nil {
		return wire.FileTransferResult{}, err
	}
	defer connection.CloseNow()

	hash := sha256.New()
	localResult := wire.FileTransferResult{}
	remoteResult, err := readFileResult(ctx, connection, func(data []byte) error {
		if int64(len(data)) > client.maxDownloadBytes-localResult.Size {
			return fmt.Errorf("download exceeds client size limit of %d bytes", client.maxDownloadBytes)
		}
		localResult.Size += int64(len(data))
		_, _ = hash.Write(data)
		if _, err := writer.Write(data); err != nil {
			return fmt.Errorf("write local download: %w", err)
		}
		return nil
	})
	localResult.SHA256 = hex.EncodeToString(hash.Sum(nil))
	if err != nil {
		return remoteResult, err
	}
	if remoteResult.Size != localResult.Size || !strings.EqualFold(remoteResult.SHA256, localResult.SHA256) {
		return remoteResult, errors.New("download size or SHA-256 mismatch")
	}
	if err := writer.SetMode(os.FileMode(remoteResult.Mode)); err != nil {
		return remoteResult, err
	}
	if err := writer.Commit(); err != nil {
		return remoteResult, err
	}
	_ = connection.Close(websocket.StatusNormalClosure, "download complete")
	return remoteResult, nil
}

func readFileResult(ctx context.Context, connection *websocket.Conn, onData func([]byte) error) (wire.FileTransferResult, error) {
	for {
		messageType, payload, err := connection.Read(ctx)
		if err != nil {
			return wire.FileTransferResult{}, fmt.Errorf("read file transfer: %w", err)
		}
		if messageType != websocket.MessageBinary {
			return wire.FileTransferResult{}, errors.New("file stream returned a non-binary message")
		}
		frameType, content, err := wire.DecodeFileFrame(payload)
		if err != nil {
			return wire.FileTransferResult{}, err
		}
		switch frameType {
		case wire.FileFrameData:
			if onData == nil {
				return wire.FileTransferResult{}, errors.New("upload stream returned unexpected file data")
			}
			if err := onData(content); err != nil {
				return wire.FileTransferResult{}, err
			}
		case wire.FileFrameResult:
			result, err := wire.DecodeFileTransferResult(content)
			if err != nil {
				return wire.FileTransferResult{}, err
			}
			if result.Error != "" {
				return result, errors.New(result.Error)
			}
			return result, nil
		default:
			return wire.FileTransferResult{}, fmt.Errorf("unexpected file frame type %d", frameType)
		}
	}
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
	return client.dialStreamWithQuery(ctx, target, protocol, extraHeaders, nil)
}

func (client *Client) dialStreamWithQuery(ctx context.Context, target, protocol string, extraHeaders http.Header, query url.Values) (*websocket.Conn, *http.Response, error) {
	if target == "" {
		return nil, nil, errors.New("target is required")
	}
	if protocol == "" {
		return nil, nil, errors.New("stream protocol is required")
	}
	endpoint := client.endpoint("/v1/nodes/" + url.PathEscape(target) + "/streams/" + url.PathEscape(protocol))
	endpoint.RawQuery = query.Encode()
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
