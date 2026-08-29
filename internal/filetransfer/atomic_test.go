package filetransfer

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAtomicWriterPublishesAndHonorsOverwrite(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "artifact.bin")
	writer, err := NewAtomicWriter(destination, false, 0o640)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Commit(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(destination)
	if err != nil || string(data) != "first" {
		t.Fatalf("unexpected destination: %q err=%v", data, err)
	}

	if _, err := NewAtomicWriter(destination, false, 0o600); err == nil {
		t.Fatal("existing destination was accepted without overwrite")
	}
	replacement, err := NewAtomicWriter(destination, true, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := replacement.Write([]byte("second")); err != nil {
		t.Fatal(err)
	}
	if err := replacement.Commit(); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(destination)
	if err != nil || string(data) != "second" {
		t.Fatalf("unexpected replacement: %q err=%v", data, err)
	}
}

func TestAtomicWriterAbortRemovesTemporaryFile(t *testing.T) {
	directory := t.TempDir()
	destination := filepath.Join(directory, "artifact.bin")
	writer, err := NewAtomicWriter(destination, false, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	writer.Abort()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("temporary file remained: %#v", entries)
	}
}
