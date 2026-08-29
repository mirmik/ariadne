package autostart

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestInstallExecutableUsesContentAddressedStableName(t *testing.T) {
	directory := t.TempDir()
	source := filepath.Join(directory, "downloaded-connector")
	if err := os.WriteFile(source, []byte("connector bytes"), 0o700); err != nil {
		t.Fatal(err)
	}
	targetDirectory := filepath.Join(directory, "installed")
	target, err := installExecutable(source, targetDirectory, "ariadne-connector")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(filepath.Base(target), "ariadne-connector-") {
		t.Fatalf("unexpected target name %q", target)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "connector bytes" {
		t.Fatalf("installed bytes = %q", data)
	}
	again, err := installExecutable(source, targetDirectory, "ariadne-connector")
	if err != nil {
		t.Fatal(err)
	}
	if again != target {
		t.Fatalf("same content installed as %q, want %q", again, target)
	}
}

func TestConfigRoundTripPreservesArgumentBoundaries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ariadne", "autostart.json")
	want := Config{
		Version:    configVersion,
		Executable: `/path with spaces/ariadne-connector`,
		Arguments:  []string{"--relay", "relay.example", "--alias", "radio room"},
		LogFile:    "/tmp/ariadne.log",
	}
	if err := saveConfig(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("loaded config %#v, want %#v", got, want)
	}
}

func TestLoadConfigRejectsUnknownVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "autostart.json")
	if err := os.WriteFile(path, []byte(`{"version":9,"executable":"connector"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(path); err == nil || !strings.Contains(err.Error(), "version 9") {
		t.Fatalf("expected version error, got %v", err)
	}
}
