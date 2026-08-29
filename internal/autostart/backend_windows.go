//go:build windows

package autostart

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const windowsTaskName = "Ariadne Connector"

func currentPaths() (platformPaths, error) {
	configFile, err := baseConfigFile()
	if err != nil {
		return platformPaths{}, err
	}
	localData := os.Getenv("LOCALAPPDATA")
	if localData == "" {
		localData, err = os.UserCacheDir()
		if err != nil {
			return platformPaths{}, fmt.Errorf("find local application data directory: %w", err)
		}
	}
	root := filepath.Join(localData, "Ariadne")
	return platformPaths{
		configFile:      configFile,
		binaryDirectory: filepath.Join(root, "bin"),
		binaryName:      "ariadne-connector.exe",
		logFile:         filepath.Join(root, "logs", "connector.log"),
		registration:    windowsTaskName,
	}, nil
}

func installPlatform(paths platformPaths, config Config) (Result, error) {
	script, err := windowsTaskScript(config.Executable)
	if err != nil {
		return Result{}, err
	}
	arguments := []string{"-NoProfile", "-NonInteractive", "-Command", script}
	if output, err := exec.Command("powershell.exe", arguments...).CombinedOutput(); err != nil {
		return Result{}, commandError("register Windows logon task", output, err)
	}
	return Result{
		Mechanism: "Windows Task Scheduler",
		Location:  windowsTaskName,
		Details:   "installed without a password; it will start at the next user logon",
	}, nil
}

func statusPlatform(_ platformPaths) (Result, error) {
	taskName := quotePowerShell(windowsTaskName)
	script := "$ErrorActionPreference = 'Stop'; $task = Get-ScheduledTask -TaskName " + taskName + " -ErrorAction SilentlyContinue; " +
		"if ($null -eq $task) { Write-Output 'not installed' } else { " +
		"$info = Get-ScheduledTaskInfo -TaskName " + taskName + "; " +
		"Write-Output ('installed; state=' + $task.State + '; last_result=' + $info.LastTaskResult) }"
	output, err := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script).CombinedOutput()
	if err != nil {
		return Result{}, commandError("query Windows logon task", output, err)
	}
	return Result{
		Mechanism: "Windows Task Scheduler",
		Location:  windowsTaskName,
		Details:   strings.TrimSpace(string(output)),
	}, nil
}

func uninstallPlatform(_ platformPaths) (Result, error) {
	taskName := quotePowerShell(windowsTaskName)
	script := "$ErrorActionPreference = 'Stop'; $task = Get-ScheduledTask -TaskName " + taskName + " -ErrorAction SilentlyContinue; " +
		"if ($null -ne $task) { Unregister-ScheduledTask -TaskName " + taskName + " -Confirm:$false }"
	output, err := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script).CombinedOutput()
	if err != nil {
		return Result{}, commandError("delete Windows logon task", output, err)
	}
	return Result{Mechanism: "Windows Task Scheduler", Location: windowsTaskName, Details: "removed; an already running connector is not stopped"}, nil
}

func windowsTaskScript(executable string) (string, error) {
	if strings.ContainsAny(executable, "\r\n") {
		return "", errors.New("installed executable path cannot contain newlines")
	}
	quotedExecutable := quotePowerShell(executable)
	quotedTaskName := quotePowerShell(windowsTaskName)
	return strings.Join([]string{
		"$ErrorActionPreference = 'Stop'",
		"$userId = [System.Security.Principal.WindowsIdentity]::GetCurrent().User.Value",
		"$action = New-ScheduledTaskAction -Execute " + quotedExecutable + " -Argument 'autostart run'",
		"$trigger = New-ScheduledTaskTrigger -AtLogOn -User $userId",
		"$principal = New-ScheduledTaskPrincipal -UserId $userId -LogonType Interactive -RunLevel Limited",
		"$settings = New-ScheduledTaskSettingsSet -RestartCount 999 -RestartInterval (New-TimeSpan -Minutes 1) -ExecutionTimeLimit ([TimeSpan]::Zero) -MultipleInstances IgnoreNew -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries",
		"Register-ScheduledTask -TaskName " + quotedTaskName + " -Action $action -Trigger $trigger -Principal $principal -Settings $settings -Force | Out-Null",
	}, "; "), nil
}

func quotePowerShell(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func commandError(action string, output []byte, err error) error {
	detail := strings.TrimSpace(string(output))
	if detail == "" {
		return fmt.Errorf("%s: %w", action, err)
	}
	return fmt.Errorf("%s: %w: %s", action, err, detail)
}
