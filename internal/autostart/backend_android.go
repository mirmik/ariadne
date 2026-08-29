//go:build android

package autostart

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func currentPaths() (platformPaths, error) {
	configFile, err := baseConfigFile()
	if err != nil {
		return platformPaths{}, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return platformPaths{}, fmt.Errorf("find user home directory: %w", err)
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		cache = filepath.Join(home, ".cache")
	}
	return platformPaths{
		configFile:      configFile,
		binaryDirectory: filepath.Join(home, ".local", "lib", "ariadne"),
		binaryName:      "ariadne-connector",
		logFile:         filepath.Join(cache, "ariadne", "connector.log"),
		registration:    filepath.Join(home, ".termux", "boot", "20-ariadne-connector"),
	}, nil
}

func installPlatform(paths platformPaths, config Config) (Result, error) {
	if os.Getenv("PREFIX") == "" || !strings.Contains(os.Getenv("PREFIX"), "com.termux") {
		return Result{}, errors.New("Termux environment was not detected")
	}
	if err := os.MkdirAll(filepath.Dir(paths.registration), 0o700); err != nil {
		return Result{}, fmt.Errorf("create Termux:Boot directory: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(paths.logFile), 0o700); err != nil {
		return Result{}, fmt.Errorf("create Termux log directory: %w", err)
	}
	script := renderTermuxBoot(config.Executable, paths.logFile)
	if err := os.WriteFile(paths.registration, []byte(script), 0o700); err != nil {
		return Result{}, fmt.Errorf("write Termux:Boot script: %w", err)
	}
	return Result{
		Mechanism: "Termux:Boot script",
		Location:  paths.registration,
		Details:   "installed; it will start at the next Android boot",
		Warning:   "Termux:Boot must be installed and opened once; Android battery restrictions may still suspend the connector",
	}, nil
}

func statusPlatform(paths platformPaths) (Result, error) {
	_, err := os.Stat(paths.registration)
	if errors.Is(err, os.ErrNotExist) {
		return Result{Mechanism: "Termux:Boot script", Location: paths.registration, Details: "not installed"}, nil
	}
	if err != nil {
		return Result{}, fmt.Errorf("inspect Termux:Boot script: %w", err)
	}
	return Result{
		Mechanism: "Termux:Boot script",
		Location:  paths.registration,
		Details:   "installed (running state is not managed by Termux:Boot)",
	}, nil
}

func uninstallPlatform(paths platformPaths) (Result, error) {
	if err := os.Remove(paths.registration); err != nil && !errors.Is(err, os.ErrNotExist) {
		return Result{}, fmt.Errorf("remove Termux:Boot script: %w", err)
	}
	return Result{
		Mechanism: "Termux:Boot script",
		Location:  paths.registration,
		Details:   "removed; an already running connector is not stopped",
	}, nil
}

func renderTermuxBoot(executable, logFile string) string {
	quoted := quoteShell(executable)
	return "#!/data/data/com.termux/files/usr/bin/sh\n" +
		"nohup " + quoted + " autostart run >>" + quoteShell(logFile) + " 2>&1 &\n"
}

func quoteShell(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
