package noderegistry

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

const (
	stateVersion = 1
	maxStateSize = 16 << 20
	// Public registration must not consume unbounded persistent storage.
	MaxUnclaimedNodes = 1024
)

var ErrRevoked = errors.New("node identity is revoked")
var ErrCapacity = errors.New("unclaimed node registry capacity reached")
var ErrRateLimited = errors.New("node registration rate limit reached")

type Record struct {
	NodeID           string     `json:"node_id"`
	PublicKey        string     `json:"public_key"`
	ClaimedAlias     string     `json:"claimed_alias,omitempty"`
	ReportedAlias    string     `json:"reported_alias"`
	SSHHostKey       string     `json:"ssh_host_key"`
	Platform         string     `json:"platform"`
	Architecture     string     `json:"architecture"`
	ConnectorVersion string     `json:"connector_version"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
	RevokedAt        *time.Time `json:"revoked_at,omitempty"`
}

type Observation struct {
	NodeID           string
	PublicKey        string
	ReportedAlias    string
	SSHHostKey       string
	Platform         string
	Architecture     string
	ConnectorVersion string
}

type stateFile struct {
	Version int               `json:"version"`
	Nodes   map[string]Record `json:"nodes"`
}

type Store struct {
	path         string
	mu           sync.Mutex
	state        stateFile
	observations *rate.Limiter
}

func DefaultPath() (string, error) {
	configDirectory, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("find user config directory: %w", err)
	}
	return filepath.Join(configDirectory, "ariadne", "node-registry.json"), nil
}

func Open(path string) (*Store, error) {
	store := &Store{path: path, state: stateFile{Version: stateVersion, Nodes: make(map[string]Record)}, observations: rate.NewLimiter(4, 16)}
	if path == "" {
		return store, nil
	}
	loaded, err := load(path)
	if errors.Is(err, os.ErrNotExist) {
		backup, backupErr := load(path + ".bak")
		if backupErr == nil {
			encoded, encodeErr := encodeState(backup)
			if encodeErr != nil {
				return nil, encodeErr
			}
			if writeErr := writeAtomic(path, encoded); writeErr != nil {
				return nil, fmt.Errorf("recover node registry from backup: %w", writeErr)
			}
			loaded = backup
			err = nil
		} else if !errors.Is(backupErr, os.ErrNotExist) {
			return nil, fmt.Errorf("load node registry backup: %w", backupErr)
		}
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err == nil {
		store.state = loaded
	}
	return store, nil
}

func (store *Store) Path() string { return store.path }

func (store *Store) Claims() map[string]string {
	store.mu.Lock()
	defer store.mu.Unlock()
	claims := make(map[string]string)
	for nodeID, record := range store.state.Nodes {
		if record.RevokedAt == nil && record.ClaimedAlias != "" {
			claims[nodeID] = record.ClaimedAlias
		}
	}
	return claims
}

func (store *Store) Observe(observation Observation, now time.Time) (Record, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	record, found := store.state.Nodes[observation.NodeID]
	if found && record.PublicKey != observation.PublicKey {
		return Record{}, errors.New("node public key does not match its registered identity")
	}
	if found && record.RevokedAt != nil {
		return Record{}, ErrRevoked
	}
	if !found {
		unclaimed := 0
		for _, candidate := range store.state.Nodes {
			if candidate.ClaimedAlias == "" && candidate.RevokedAt == nil {
				unclaimed++
			}
		}
		if unclaimed >= MaxUnclaimedNodes {
			return Record{}, ErrCapacity
		}
		record = Record{NodeID: observation.NodeID, PublicKey: observation.PublicKey, CreatedAt: now}
	}
	// Bound rewrites even when an attacker reuses previously admitted keys.
	// Administrative claim/revoke operations do not consume this budget.
	if !store.observations.AllowN(now, 1) {
		return Record{}, ErrRateLimited
	}
	record.ReportedAlias = observation.ReportedAlias
	record.SSHHostKey = observation.SSHHostKey
	record.Platform = observation.Platform
	record.Architecture = observation.Architecture
	record.ConnectorVersion = observation.ConnectorVersion
	record.UpdatedAt = now
	next := cloneState(store.state)
	next.Nodes[record.NodeID] = record
	if err := store.commit(next); err != nil {
		return Record{}, err
	}
	return record, nil
}

func (store *Store) Claim(nodeID, alias string, now time.Time) (Record, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	record, found := store.state.Nodes[nodeID]
	if !found {
		return Record{}, os.ErrNotExist
	}
	if record.RevokedAt != nil {
		return Record{}, ErrRevoked
	}
	for ownerID, candidate := range store.state.Nodes {
		if ownerID != nodeID && candidate.RevokedAt == nil && strings.EqualFold(candidate.ClaimedAlias, alias) {
			return Record{}, fmt.Errorf("alias %q is already claimed by another node", alias)
		}
	}
	record.ClaimedAlias = alias
	record.UpdatedAt = now
	next := cloneState(store.state)
	next.Nodes[nodeID] = record
	if err := store.commit(next); err != nil {
		return Record{}, err
	}
	return record, nil
}

func (store *Store) Revoke(target string, now time.Time) (Record, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	nodeID := target
	record, found := store.state.Nodes[nodeID]
	if !found {
		return Record{}, os.ErrNotExist
	}
	if record.RevokedAt != nil {
		return record, nil
	}
	revokedAt := now
	record.RevokedAt = &revokedAt
	record.UpdatedAt = now
	next := cloneState(store.state)
	next.Nodes[nodeID] = record
	if err := store.commit(next); err != nil {
		return Record{}, err
	}
	return record, nil
}

func (store *Store) commit(next stateFile) error {
	encoded, err := encodeState(next)
	if err != nil {
		return err
	}
	if store.path != "" {
		_, inspectErr := inspect(store.path)
		if inspectErr == nil {
			current, err := os.ReadFile(store.path)
			if err != nil {
				return fmt.Errorf("read node registry for backup: %w", err)
			}
			if err := writeAtomic(store.path+".bak", current); err != nil {
				return fmt.Errorf("back up node registry: %w", err)
			}
		} else if !errors.Is(inspectErr, os.ErrNotExist) {
			return inspectErr
		}
		if err := writeAtomic(store.path, encoded); err != nil {
			return fmt.Errorf("write node registry: %w", err)
		}
	}
	store.state = next
	return nil
}

func encodeState(state stateFile) ([]byte, error) {
	encoded, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode node registry: %w", err)
	}
	if len(encoded) >= maxStateSize {
		return nil, errors.New("node registry exceeds maximum serialized size")
	}
	return append(encoded, '\n'), nil
}

func cloneState(current stateFile) stateFile {
	next := stateFile{Version: stateVersion, Nodes: make(map[string]Record, len(current.Nodes))}
	for nodeID, record := range current.Nodes {
		next.Nodes[nodeID] = record
	}
	return next
}

func load(path string) (stateFile, error) {
	info, err := inspect(path)
	if err != nil {
		return stateFile{}, err
	}
	if info.Size() > maxStateSize {
		return stateFile{}, errors.New("node registry is unexpectedly large")
	}
	file, err := os.Open(path)
	if err != nil {
		return stateFile{}, fmt.Errorf("open node registry: %w", err)
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	var state stateFile
	if err := decoder.Decode(&state); err != nil {
		return stateFile{}, fmt.Errorf("decode node registry: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return stateFile{}, errors.New("decode node registry: unexpected data after JSON value")
	}
	if state.Version != stateVersion {
		return stateFile{}, fmt.Errorf("unsupported node registry version %d", state.Version)
	}
	if state.Nodes == nil {
		state.Nodes = make(map[string]Record)
	}
	aliases := make(map[string]string)
	for nodeID, record := range state.Nodes {
		if nodeID == "" || record.NodeID != nodeID || record.PublicKey == "" {
			return stateFile{}, fmt.Errorf("node registry contains an invalid record for %q", nodeID)
		}
		if record.RevokedAt == nil && record.ClaimedAlias != "" {
			key := strings.ToLower(record.ClaimedAlias)
			if owner := aliases[key]; owner != "" && owner != nodeID {
				return stateFile{}, fmt.Errorf("node registry alias %q has multiple owners", record.ClaimedAlias)
			}
			aliases[key] = nodeID
		}
	}
	return state, nil
}

func inspect(path string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect node registry: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, errors.New("node registry path must be a regular file, not a symlink")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("node registry permissions %04o are too broad; use 0600", info.Mode().Perm())
	}
	return info, nil
}

func writeAtomic(path string, data []byte) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create node registry directory: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".node-registry-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryName)
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := replaceFile(temporaryName, path); err != nil {
		return err
	}
	return nil
}
