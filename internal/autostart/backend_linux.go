//go:build linux && !android

package autostart

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const systemdUnitName = "ariadne-connector.service"

func currentPaths() (platformPaths, error) {
	configFile, err := baseConfigFile()
	if err != nil {
		return platformPaths{}, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return platformPaths{}, fmt.Errorf("find user home directory: %w", err)
	}
	configDirectory := filepath.Dir(filepath.Dir(configFile))
	return platformPaths{
		configFile:      configFile,
		binaryDirectory: filepath.Join(home, ".local", "lib", "ariadne"),
		binaryName:      "ariadne-connector",
		registration:    filepath.Join(configDirectory, "systemd", "user", systemdUnitName),
	}, nil
}

func installPlatform(paths platformPaths, config Config) (Result, error) {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return Result{}, errors.New("systemctl is required for Linux autostart")
	}
	if err := os.MkdirAll(filepath.Dir(paths.registration), 0o700); err != nil {
		return Result{}, fmt.Errorf("create user systemd directory: %w", err)
	}
	unit := renderSystemdUnit(config.Executable, os.Getenv("PATH"))
	if err := os.WriteFile(paths.registration, []byte(unit), 0o600); err != nil {
		return Result{}, fmt.Errorf("write user systemd unit: %w", err)
	}
	if output, err := exec.Command("systemctl", "--user", "daemon-reload").CombinedOutput(); err != nil {
		return Result{}, commandError("reload user systemd manager", output, err)
	}
	if output, err := exec.Command("systemctl", "--user", "enable", systemdUnitName).CombinedOutput(); err != nil {
		return Result{}, commandError("enable user systemd unit", output, err)
	}
	return Result{
		Mechanism: "systemd user unit",
		Location:  paths.registration,
		Details:   "enabled; it will start at the next user login",
	}, nil
}

func statusPlatform(paths platformPaths) (Result, error) {
	if _, err := os.Stat(paths.registration); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Result{Mechanism: "systemd user unit", Location: paths.registration, Details: "not installed"}, nil
		}
		return Result{}, fmt.Errorf("inspect user systemd unit: %w", err)
	}
	enabled := systemctlState("is-enabled", systemdUnitName)
	active := systemctlState("is-active", systemdUnitName)
	return Result{
		Mechanism: "systemd user unit",
		Location:  paths.registration,
		Details:   fmt.Sprintf("installed; enabled=%s; active=%s", enabled, active),
	}, nil
}

func uninstallPlatform(paths platformPaths) (Result, error) {
	if _, err := exec.LookPath("systemctl"); err == nil {
		_, _ = exec.Command("systemctl", "--user", "disable", systemdUnitName).CombinedOutput()
	}
	if err := os.Remove(paths.registration); err != nil && !errors.Is(err, os.ErrNotExist) {
		return Result{}, fmt.Errorf("remove user systemd unit: %w", err)
	}
	if _, err := exec.LookPath("systemctl"); err == nil {
		if output, reloadErr := exec.Command("systemctl", "--user", "daemon-reload").CombinedOutput(); reloadErr != nil {
			return Result{}, commandError("reload user systemd manager", output, reloadErr)
		}
	}
	return Result{Mechanism: "systemd user unit", Location: paths.registration, Details: "removed; an already running connector is not stopped"}, nil
}

func renderSystemdUnit(executable, pathEnvironment string) string {
	pathLine := ""
	if pathEnvironment != "" && !strings.ContainsAny(pathEnvironment, "\r\n") {
		pathLine = "Environment=\"PATH=" + escapeSystemd(pathEnvironment) + "\"\n"
	}
	return `[Unit]
Description=Ariadne connector
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
` + pathLine + `ExecStart="` + escapeSystemd(executable) + `" autostart run
Restart=on-failure
RestartSec=5s

[Install]
WantedBy=default.target
`
}

func escapeSystemd(value string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, `%`, `%%`).Replace(value)
}

func systemctlState(command string, arguments ...string) string {
	output, err := exec.Command("systemctl", append([]string{"--user", command}, arguments...)...).CombinedOutput()
	state := strings.TrimSpace(string(output))
	if state == "" {
		if err != nil {
			return "unknown"
		}
		return "yes"
	}
	return state
}

func commandError(action string, output []byte, err error) error {
	detail := strings.TrimSpace(string(output))
	if detail == "" {
		return fmt.Errorf("%s: %w", action, err)
	}
	return fmt.Errorf("%s: %w: %s", action, err, detail)
}
