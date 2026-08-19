package identity

import (
	"crypto"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const stateVersion = 1

var nodeIDEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

type Identity struct {
	privateKey ed25519.PrivateKey
}

type stateFile struct {
	Version int    `json:"version"`
	Seed    string `json:"seed"`
}

func DefaultPath() (string, error) {
	configDirectory, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("find user config directory: %w", err)
	}
	return filepath.Join(configDirectory, "ariadne", "identity.json"), nil
}

func LoadOrCreate(path string) (*Identity, bool, error) {
	loaded, err := Load(path)
	if err == nil {
		return loaded, false, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, false, err
	}

	created, err := Generate()
	if err != nil {
		return nil, false, err
	}
	if err := created.saveNew(path); err != nil {
		if errors.Is(err, os.ErrExist) {
			loaded, loadErr := Load(path)
			return loaded, false, loadErr
		}
		return nil, false, err
	}
	return created, true, nil
}

func Load(path string) (*Identity, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect identity file: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, errors.New("identity path must be a regular file, not a symlink")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("identity file permissions %04o are too broad; use 0600", info.Mode().Perm())
	}
	if info.Size() > 16<<10 {
		return nil, errors.New("identity file is unexpectedly large")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read identity file: %w", err)
	}
	var state stateFile
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil {
		return nil, fmt.Errorf("decode identity file: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, errors.New("decode identity file: unexpected data after JSON value")
	}
	if state.Version != stateVersion {
		return nil, fmt.Errorf("unsupported identity file version %d", state.Version)
	}
	seed, err := base64.RawStdEncoding.DecodeString(state.Seed)
	if err != nil {
		return nil, fmt.Errorf("decode identity seed: %w", err)
	}
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("identity seed has %d bytes, expected %d", len(seed), ed25519.SeedSize)
	}
	return &Identity{privateKey: ed25519.NewKeyFromSeed(seed)}, nil
}

func Generate() (*Identity, error) {
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate Ed25519 identity: %w", err)
	}
	return &Identity{privateKey: privateKey}, nil
}

func (identity *Identity) NodeID() string {
	return NodeID(identity.PublicKey())
}

func (identity *Identity) PublicKey() ed25519.PublicKey {
	publicKey := identity.privateKey.Public().(ed25519.PublicKey)
	return append(ed25519.PublicKey(nil), publicKey...)
}

func (identity *Identity) EncodedPublicKey() string {
	return base64.RawStdEncoding.EncodeToString(identity.PublicKey())
}

func (identity *Identity) Sign(message []byte) string {
	signature := ed25519.Sign(identity.privateKey, message)
	return base64.RawStdEncoding.EncodeToString(signature)
}

// SSHHostSigner returns a stable Ed25519 key that is domain-separated from
// the node identity key. Rotating the node identity rotates this key as well.
func (identity *Identity) SSHHostSigner() crypto.Signer {
	mac := hmac.New(sha256.New, identity.privateKey.Seed())
	_, _ = mac.Write([]byte("ariadne/ssh-host/v1"))
	return ed25519.NewKeyFromSeed(mac.Sum(nil))
}

func ParsePublicKey(encoded string) (ed25519.PublicKey, error) {
	raw, err := base64.RawStdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("decode Ed25519 public key: %w", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("Ed25519 public key has %d bytes, expected %d", len(raw), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(raw), nil
}

func ParseSignature(encoded string) ([]byte, error) {
	raw, err := base64.RawStdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("decode Ed25519 signature: %w", err)
	}
	if len(raw) != ed25519.SignatureSize {
		return nil, fmt.Errorf("Ed25519 signature has %d bytes, expected %d", len(raw), ed25519.SignatureSize)
	}
	return raw, nil
}

func NodeID(publicKey ed25519.PublicKey) string {
	digest := sha256.Sum256(publicKey)
	return "n_" + strings.ToLower(nodeIDEncoding.EncodeToString(digest[:20]))
}

func (identity *Identity) saveNew(path string) (returnErr error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create identity directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create identity file: %w", err)
	}
	succeeded := false
	defer func() {
		if closeErr := file.Close(); returnErr == nil && closeErr != nil {
			returnErr = fmt.Errorf("close identity file: %w", closeErr)
		}
		if !succeeded {
			_ = os.Remove(path)
		}
	}()

	state := stateFile{
		Version: stateVersion,
		Seed:    base64.RawStdEncoding.EncodeToString(identity.privateKey.Seed()),
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode identity file: %w", err)
	}
	data = append(data, '\n')
	if _, err := file.Write(data); err != nil {
		return fmt.Errorf("write identity file: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync identity file: %w", err)
	}
	succeeded = true
	return nil
}
