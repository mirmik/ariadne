package cliargs

import (
	"reflect"
	"testing"
)

func TestFromArgv(t *testing.T) {
	tests := []struct {
		name          string
		goos          string
		workingDir    string
		termuxSelfExe string
		argv          []string
		want          []string
	}{
		{
			name: "ordinary command line",
			goos: "linux",
			argv: []string{"ariadne-connector", "--alias", "phone"},
			want: []string{"--alias", "phone"},
		},
		{
			name:          "Termux linker argument",
			goos:          "android",
			workingDir:    "/data/data/com.termux/files/home",
			termuxSelfExe: "./ariadne-connector",
			argv:          []string{"./ariadne-connector", "/data/data/com.termux/files/home/ariadne-connector", "--alias", "phone"},
			want:          []string{"--alias", "phone"},
		},
		{
			name:          "absolute Termux linker argument",
			goos:          "android",
			workingDir:    "/data/data/com.termux/files/home",
			termuxSelfExe: "/data/data/com.termux/files/home/ariadne-connector",
			argv:          []string{"/data/data/com.termux/files/home/ariadne-connector", "/data/data/com.termux/files/home/ariadne-connector", "--help"},
			want:          []string{"--help"},
		},
		{
			name:          "Termux marker is ignored outside Android",
			goos:          "linux",
			workingDir:    "/tmp",
			termuxSelfExe: "ari",
			argv:          []string{"ari", "ari", "nodes"},
			want:          []string{"ari", "nodes"},
		},
		{
			name: "first real Android argument is preserved without Termux marker",
			goos: "android",
			argv: []string{"ariadne-connector", "ariadne-connector", "--help"},
			want: []string{"ariadne-connector", "--help"},
		},
		{
			name:          "unrelated first argument is preserved",
			goos:          "android",
			workingDir:    "/data/data/com.termux/files/home",
			termuxSelfExe: "/data/data/com.termux/files/home/ariadne-connector",
			argv:          []string{"ariadne-connector", "radio", "--help"},
			want:          []string{"radio", "--help"},
		},
		{
			name: "program name only",
			goos: "android",
			argv: []string{"ari"},
			want: nil,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := FromArgv(test.goos, test.workingDir, test.termuxSelfExe, test.argv); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("FromArgv(%q, %q, %q, %#v) = %#v, want %#v", test.goos, test.workingDir, test.termuxSelfExe, test.argv, got, test.want)
			}
		})
	}
}
