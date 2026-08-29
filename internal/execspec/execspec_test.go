package execspec

import (
	"reflect"
	"testing"

	"github.com/mirmik/ariadne/internal/wire"
)

func TestPrepare(t *testing.T) {
	tests := []struct {
		name      string
		platform  string
		request   wire.ExecRequest
		wantArgv  []string
		wantShell string
	}{
		{
			name:      "Unix auto shell",
			platform:  "linux",
			request:   wire.ExecRequest{Command: "task build && git status --short", Shell: ShellAuto},
			wantArgv:  []string{"sh", "-lc", "task build && git status --short"},
			wantShell: "sh",
		},
		{
			name:      "Android auto shell",
			platform:  "android",
			request:   wire.ExecRequest{Command: "pwd"},
			wantArgv:  []string{"/system/bin/sh", "-c", `exec sh -lc "$1"`, "ariadne-shell", "pwd"},
			wantShell: "sh",
		},
		{
			name:      "Windows auto shell",
			platform:  "windows",
			request:   wire.ExecRequest{Command: "Get-ChildItem | Select-Object -First 1"},
			wantArgv:  []string{"powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", "Get-ChildItem | Select-Object -First 1"},
			wantShell: "powershell.exe",
		},
		{
			name:      "explicit Windows cmd",
			platform:  "windows",
			request:   wire.ExecRequest{Command: "echo %PATH%", Shell: ShellCMD},
			wantArgv:  []string{"cmd.exe", "/D", "/S", "/C", "echo %PATH%"},
			wantShell: "cmd.exe",
		},
		{
			name:     "direct argv",
			platform: "windows",
			request:  wire.ExecRequest{Argv: []string{"task", "build"}},
			wantArgv: []string{"task", "build"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			prepared, shell, err := Prepare(test.request, test.platform)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(prepared.Argv, test.wantArgv) || prepared.Command != "" || prepared.Shell != "" || shell != test.wantShell {
				t.Fatalf("Prepare() = request %#v, shell %q; want argv %#v, shell %q", prepared, shell, test.wantArgv, test.wantShell)
			}
		})
	}
}

func TestPrepareRejectsAmbiguousOrInvalidInput(t *testing.T) {
	tests := []wire.ExecRequest{
		{},
		{Command: "pwd", Argv: []string{"pwd"}},
		{Argv: []string{""}},
		{Argv: []string{"pwd"}, Shell: ShellPOSIX},
		{Command: "pwd", Shell: "fish"},
	}
	for _, request := range tests {
		if _, _, err := Prepare(request, "linux"); err == nil {
			t.Fatalf("Prepare accepted %#v", request)
		}
	}
}
