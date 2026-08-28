package managementauth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const tokenBytes = 32

func DefaultPath() (string, error) {
	configDirectory, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("find user config directory: %w", err)
	}
	return filepath.Join(configDirectory, "ariadne", "management.token"), nil
}

func LoadOrCreate(path string) (string, bool, error) {
	token, err := Load(path)
	if err == nil {
		return token, false, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", false, err
	}
	raw := make([]byte, tokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", false, fmt.Errorf("generate management token: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(raw)
	if err := saveNew(path, token); err != nil {
		if errors.Is(err, os.ErrExist) {
			loaded, loadErr := Load(path)
			return loaded, false, loadErr
		}
		return "", false, err
	}
	return token, true, nil
}

func Load(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("inspect management token file: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", errors.New("management token path must be a regular file, not a symlink")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("management token file permissions %04o are too broad; use 0600", info.Mode().Perm())
	}
	if info.Size() > 1024 {
		return "", errors.New("management token file is unexpectedly large")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read management token file: %w", err)
	}
	token := strings.TrimSpace(string(data))
	if err := Validate(token); err != nil {
		return "", fmt.Errorf("invalid management token file: %w", err)
	}
	return token, nil
}

func Validate(token string) error {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) != tokenBytes {
		return errors.New("token must be 32 random bytes encoded as unpadded base64url")
	}
	return nil
}

func Equal(left, right string) bool {
	if len(left) != len(right) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

func saveNew(path, token string) (returnErr error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create management token directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create management token file: %w", err)
	}
	succeeded := false
	defer func() {
		if closeErr := file.Close(); returnErr == nil && closeErr != nil {
			returnErr = fmt.Errorf("close management token file: %w", closeErr)
		}
		if !succeeded {
			_ = os.Remove(path)
		}
	}()
	if _, err := file.WriteString(token + "\n"); err != nil {
		return fmt.Errorf("write management token file: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync management token file: %w", err)
	}
	succeeded = true
	return nil
}
