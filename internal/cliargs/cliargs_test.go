package cliargs

import (
	"reflect"
	"testing"
)

func TestFromArgv(t *testing.T) {
	tests := []struct {
		name string
		goos string
		argv []string
		want []string
	}{
		{
			name: "ordinary command line",
			goos: "linux",
			argv: []string{"ariadne-connector", "--alias", "phone"},
			want: []string{"--alias", "phone"},
		},
		{
			name: "duplicated Android program name",
			goos: "android",
			argv: []string{"/data/data/com.termux/files/home/ariadne-connector", "/data/data/com.termux/files/home/ariadne-connector", "--alias", "phone"},
			want: []string{"--alias", "phone"},
		},
		{
			name: "same first argument is retained outside Android",
			goos: "linux",
			argv: []string{"ari", "ari", "nodes"},
			want: []string{"ari", "nodes"},
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
			if got := FromArgv(test.goos, test.argv); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("FromArgv(%q, %#v) = %#v, want %#v", test.goos, test.argv, got, test.want)
			}
		})
	}
}
