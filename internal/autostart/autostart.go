package autostart

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const configVersion = 1

type Config struct {
	Version    int      `json:"version"`
	Executable string   `json:"executable"`
	Arguments  []string `json:"arguments"`
	LogFile    string   `json:"log_file,omitempty"`
}

type Result struct {
	Mechanism string
	Location  string
	Details   string
	Warning   string
}

func Install(arguments []string) (Result, error) {
	paths, err := currentPaths()
	if err != nil {
		return Result{}, err
	}
	source, err := os.Executable()
	if err != nil {
		return Result{}, fmt.Errorf("locate current executable: %w", err)
	}
	target, err := installExecutable(source, paths.binaryDirectory, paths.binaryName)
	if err != nil {
		return Result{}, err
	}
	config := Config{
		Version:    configVersion,
		Executable: target,
		Arguments:  append([]string(nil), arguments...),
		LogFile:    paths.logFile,
	}
	if err := saveConfig(paths.configFile, config); err != nil {
		return Result{}, err
	}
	return installPlatform(paths, config)
}

func Status() (Result, error) {
	paths, err := currentPaths()
	if err != nil {
		return Result{}, err
	}
	return statusPlatform(paths)
}

func Uninstall() (Result, error) {
	paths, err := currentPaths()
	if err != nil {
		return Result{}, err
	}
	config, loadErr := loadConfig(paths.configFile)
	if loadErr != nil && !errors.Is(loadErr, os.ErrNotExist) {
		return Result{}, loadErr
	}
	result, err := uninstallPlatform(paths)
	if err != nil {
		return Result{}, err
	}
	if loadErr == nil {
		if err := os.Remove(config.Executable); err != nil && !errors.Is(err, os.ErrNotExist) {
			result.Warning = fmt.Sprintf("autostart removed, but installed executable could not be removed: %v", err)
		}
	}
	if err := os.Remove(paths.configFile); err != nil && !errors.Is(err, os.ErrNotExist) {
		return Result{}, fmt.Errorf("remove autostart configuration: %w", err)
	}
	return result, nil
}

func Load() (Config, error) {
	paths, err := currentPaths()
	if err != nil {
		return Config{}, err
	}
	return loadConfig(paths.configFile)
}

type platformPaths struct {
	configFile      string
	binaryDirectory string
	binaryName      string
	logFile         string
	registration    string
}

func baseConfigFile() (string, error) {
	directory, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("find user config directory: %w", err)
	}
	return filepath.Join(directory, "ariadne", "autostart.json"), nil
}

func installExecutable(source, directory, name string) (string, error) {
	input, err := os.Open(source)
	if err != nil {
		return "", fmt.Errorf("open current executable: %w", err)
	}
	defer input.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, input); err != nil {
		return "", fmt.Errorf("hash current executable: %w", err)
	}
	digest := hex.EncodeToString(hash.Sum(nil))[:12]
	extension := filepath.Ext(name)
	base := name[:len(name)-len(extension)]
	target := filepath.Join(directory, base+"-"+digest+extension)
	if _, err := os.Stat(target); err == nil {
		return target, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("inspect installed executable: %w", err)
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", fmt.Errorf("create autostart binary directory: %w", err)
	}
	if _, err := input.Seek(0, io.SeekStart); err != nil {
		return "", fmt.Errorf("rewind current executable: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".ariadne-connector-*")
	if err != nil {
		return "", fmt.Errorf("create temporary executable: %w", err)
	}
	temporaryName := temporary.Name()
	succeeded := false
	defer func() {
		_ = temporary.Close()
		if !succeeded {
			_ = os.Remove(temporaryName)
		}
	}()
	if err := temporary.Chmod(0o700); err != nil {
		return "", fmt.Errorf("set installed executable permissions: %w", err)
	}
	if _, err := io.Copy(temporary, input); err != nil {
		return "", fmt.Errorf("copy current executable: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return "", fmt.Errorf("sync installed executable: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return "", fmt.Errorf("close installed executable: %w", err)
	}
	if err := os.Rename(temporaryName, target); err != nil {
		if _, statErr := os.Stat(target); statErr == nil {
			succeeded = true
			_ = os.Remove(temporaryName)
			return target, nil
		}
		return "", fmt.Errorf("publish installed executable: %w", err)
	}
	succeeded = true
	return target, nil
}

func saveConfig(path string, config Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create autostart config directory: %w", err)
	}
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("encode autostart configuration: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write autostart configuration: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("secure autostart configuration: %w", err)
	}
	return nil
}

func loadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read autostart configuration: %w", err)
	}
	var config Config
	if err := json.Unmarshal(data, &config); err != nil {
		return Config{}, fmt.Errorf("decode autostart configuration: %w", err)
	}
	if config.Version != configVersion {
		return Config{}, fmt.Errorf("unsupported autostart configuration version %d", config.Version)
	}
	if config.Executable == "" {
		return Config{}, errors.New("autostart configuration has no executable")
	}
	return config, nil
}
