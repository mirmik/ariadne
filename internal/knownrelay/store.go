package knownrelay

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/mirmik/ariadne/internal/transport"
)

const maxStoreSize = 1 << 20

type Decision int

const (
	Known Decision = iota
	Learned
	Replaced
)

type Result struct {
	Decision    Decision
	Pin         string
	PreviousPin string
}

type CertificateChangedError struct {
	Endpoint    string
	KnownPin    string
	ReceivedPin string
}

func (err *CertificateChangedError) Error() string {
	return fmt.Sprintf(
		"relay certificate changed for %s: known %s, received %s",
		err.Endpoint,
		err.KnownPin,
		err.ReceivedPin,
	)
}

type Store struct {
	path    string
	mu      sync.Mutex
	entries map[string]string
}

func DefaultPath() (string, error) {
	configDirectory, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("find user config directory: %w", err)
	}
	return filepath.Join(configDirectory, "ariadne", "known_relays"), nil
}

func Open(path string) (*Store, error) {
	entries, err := load(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if entries == nil {
		entries = make(map[string]string)
	}
	return &Store{path: path, entries: entries}, nil
}

func (store *Store) Path() string {
	return store.path
}

func (store *Store) Verify(endpoint string, certificateDER []byte, acceptReplacement bool) (Result, error) {
	if strings.TrimSpace(endpoint) == "" || strings.ContainsAny(endpoint, " \t\r\n") {
		return Result{}, errors.New("relay endpoint must be a non-empty value without whitespace")
	}
	if len(certificateDER) == 0 {
		return Result{}, errors.New("relay certificate is empty")
	}
	pin := transport.FormatCertificatePin(certificateDER)

	store.mu.Lock()
	defer store.mu.Unlock()

	knownPin, found := store.entries[endpoint]
	if found && knownPin == pin {
		return Result{Decision: Known, Pin: pin}, nil
	}
	if found && !acceptReplacement {
		return Result{}, &CertificateChangedError{
			Endpoint:    endpoint,
			KnownPin:    knownPin,
			ReceivedPin: pin,
		}
	}
	if err := appendEntry(store.path, endpoint, pin); err != nil {
		return Result{}, err
	}
	store.entries[endpoint] = pin
	if found {
		return Result{Decision: Replaced, Pin: pin, PreviousPin: knownPin}, nil
	}
	return Result{Decision: Learned, Pin: pin}, nil
}

func load(path string) (map[string]string, error) {
	info, err := inspect(path)
	if err != nil {
		return nil, err
	}
	if info.Size() > maxStoreSize {
		return nil, errors.New("known relays file is unexpectedly large")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open known relays file: %w", err)
	}
	defer file.Close()

	entries := make(map[string]string)
	scanner := bufio.NewScanner(file)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, fmt.Errorf("parse known relays file line %d: expected endpoint and certificate pin", lineNumber)
		}
		pin, err := transport.NormalizeCertificatePin(fields[1])
		if err != nil {
			return nil, fmt.Errorf("parse known relays file line %d: %w", lineNumber, err)
		}
		entries[fields[0]] = pin
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read known relays file: %w", err)
	}
	return entries, nil
}

func inspect(path string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect known relays file: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, errors.New("known relays path must be a regular file, not a symlink")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("known relays file permissions %04o are too broad; use 0600", info.Mode().Perm())
	}
	return info, nil
}

func appendEntry(path, endpoint, pin string) (returnErr error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create known relays directory: %w", err)
	}
	_, inspectErr := inspect(path)
	newFile := errors.Is(inspectErr, os.ErrNotExist)
	if inspectErr != nil && !newFile {
		return inspectErr
	}

	flags := os.O_WRONLY | os.O_APPEND
	if newFile {
		flags |= os.O_CREATE | os.O_EXCL
	}
	file, err := os.OpenFile(path, flags, 0o600)
	if err != nil {
		if newFile && errors.Is(err, os.ErrExist) {
			if _, inspectErr := inspect(path); inspectErr != nil {
				return inspectErr
			}
			newFile = false
			file, err = os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
		}
		if err != nil {
			return fmt.Errorf("open known relays file for append: %w", err)
		}
	}
	defer func() {
		if closeErr := file.Close(); returnErr == nil && closeErr != nil {
			returnErr = fmt.Errorf("close known relays file: %w", closeErr)
		}
	}()
	entry := endpoint + " " + pin + "\n"
	if newFile {
		entry = "# Ariadne known relays; later entries replace earlier entries for the same endpoint.\n" + entry
	}
	if _, err := file.WriteString(entry); err != nil {
		return fmt.Errorf("append known relay: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync known relays file: %w", err)
	}
	return nil
}
