package managementauth

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadOrCreate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config", "management.token")
	first, created, err := LoadOrCreate(path)
	if err != nil {
		t.Fatal(err)
	}
	if !created || Validate(first) != nil {
		t.Fatalf("unexpected generated token: created=%v token=%q", created, first)
	}
	second, created, err := LoadOrCreate(path)
	if err != nil {
		t.Fatal(err)
	}
	if created || !Equal(first, second) {
		t.Fatal("existing management token was not reused")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("token permissions are %04o, expected 0600", info.Mode().Perm())
	}
}

func TestLoadRejectsSymlink(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "target")
	if err := os.WriteFile(target, []byte("not-a-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "management.token")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := Load(link); err == nil {
		t.Fatal("Load accepted a symlink")
	}
}
