package execspec

import (
	"errors"
	"fmt"
	"strings"

	"github.com/mirmik/ariadne/internal/wire"
)

const (
	ShellAuto       = "auto"
	ShellPOSIX      = "posix"
	ShellPowerShell = "powershell"
	ShellCMD        = "cmd"
)

// Prepare converts the agent-friendly command form into the exact argv form
// understood by every connector. Direct argv requests pass through unchanged.
func Prepare(request wire.ExecRequest, platform string) (wire.ExecRequest, string, error) {
	if err := Validate(request); err != nil {
		return wire.ExecRequest{}, "", err
	}
	prepared := request
	if request.Command == "" {
		prepared.Argv = append([]string(nil), request.Argv...)
		return prepared, "", nil
	}

	shell := request.Shell
	if shell == "" || shell == ShellAuto {
		if platform == "windows" {
			shell = ShellPowerShell
		} else if platform == "" {
			return wire.ExecRequest{}, "", errors.New("cannot select an automatic shell: node platform is unknown")
		} else {
			shell = ShellPOSIX
		}
	}

	var executable string
	switch shell {
	case ShellPOSIX:
		executable = "sh"
		if platform == "android" {
			// Go uses execve directly, bypassing Termux's LD_PRELOAD exec hook.
			// Android permits the system shell; it then reaches the Termux shell
			// through the normal termux-exec wrapper. Pass the command as $1 so
			// its contents are never interpolated into the trampoline script.
			prepared.Argv = []string{"/system/bin/sh", "-c", `exec sh -lc "$1"`, "ariadne-shell", request.Command}
		} else {
			prepared.Argv = []string{executable, "-lc", request.Command}
		}
	case ShellPowerShell:
		if platform == "windows" {
			executable = "powershell.exe"
		} else {
			executable = "pwsh"
		}
		prepared.Argv = []string{executable, "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", request.Command}
	case ShellCMD:
		if platform != "windows" {
			return wire.ExecRequest{}, "", fmt.Errorf("cmd shell is not available on %s nodes", platform)
		}
		executable = "cmd.exe"
		prepared.Argv = []string{executable, "/D", "/S", "/C", request.Command}
	default:
		return wire.ExecRequest{}, "", fmt.Errorf("unsupported shell %q; use auto, posix, powershell, or cmd", request.Shell)
	}
	prepared.Command = ""
	prepared.Shell = ""
	return prepared, executable, nil
}

func Validate(request wire.ExecRequest) error {
	hasCommand := strings.TrimSpace(request.Command) != ""
	hasArgv := len(request.Argv) > 0
	if hasCommand == hasArgv {
		return errors.New("provide exactly one of command or argv")
	}
	if hasArgv {
		if request.Argv[0] == "" {
			return errors.New("argv must contain a non-empty executable")
		}
		if request.Shell != "" {
			return errors.New("shell can only be used with command")
		}
		return nil
	}
	switch request.Shell {
	case "", ShellAuto, ShellPOSIX, ShellPowerShell, ShellCMD:
		return nil
	default:
		return fmt.Errorf("unsupported shell %q; use auto, posix, powershell, or cmd", request.Shell)
	}
}
