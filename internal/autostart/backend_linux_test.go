//go:build linux && !android

package autostart

import (
	"strings"
	"testing"
)

func TestRenderSystemdUnit(t *testing.T) {
	unit := renderSystemdUnit(`/home/user/a%b/ariadne connector`, `/usr/bin:/home/user/bin`)
	for _, expected := range []string{
		`ExecStart="/home/user/a%%b/ariadne connector" autostart run`,
		`Environment="PATH=/usr/bin:/home/user/bin"`,
		`Restart=on-failure`,
		`WantedBy=default.target`,
	} {
		if !strings.Contains(unit, expected) {
			t.Fatalf("unit does not contain %q:\n%s", expected, unit)
		}
	}
}
