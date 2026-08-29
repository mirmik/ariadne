package filetransfer

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type AtomicWriter struct {
	destination string
	temporary   string
	overwrite   bool
	file        *os.File
	committed   bool
}

func NewAtomicWriter(destination string, overwrite bool, mode os.FileMode) (*AtomicWriter, error) {
	if destination == "" {
		return nil, errors.New("destination path is required")
	}
	if !overwrite {
		if _, err := os.Lstat(destination); err == nil {
			return nil, fmt.Errorf("destination already exists: %s", destination)
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("inspect destination: %w", err)
		}
	}
	directory := filepath.Dir(destination)
	base := filepath.Base(destination)
	file, err := os.CreateTemp(directory, "."+base+".ariadne-*")
	if err != nil {
		return nil, fmt.Errorf("create temporary destination: %w", err)
	}
	if mode == 0 {
		mode = 0o600
	}
	if err := file.Chmod(mode.Perm()); err != nil {
		file.Close()
		os.Remove(file.Name())
		return nil, fmt.Errorf("set destination mode: %w", err)
	}
	return &AtomicWriter{destination: destination, temporary: file.Name(), overwrite: overwrite, file: file}, nil
}

func (writer *AtomicWriter) Write(data []byte) (int, error) {
	return writer.file.Write(data)
}

func (writer *AtomicWriter) SetMode(mode os.FileMode) error {
	if mode == 0 {
		return nil
	}
	if err := writer.file.Chmod(mode.Perm()); err != nil {
		return fmt.Errorf("set destination mode: %w", err)
	}
	return nil
}

func (writer *AtomicWriter) Commit() error {
	if writer.committed {
		return nil
	}
	if err := writer.file.Sync(); err != nil {
		return fmt.Errorf("sync temporary file: %w", err)
	}
	if err := writer.file.Close(); err != nil {
		return fmt.Errorf("close temporary file: %w", err)
	}
	if err := commitFile(writer.temporary, writer.destination, writer.overwrite); err != nil {
		return err
	}
	writer.committed = true
	return nil
}

func (writer *AtomicWriter) Abort() {
	if writer == nil || writer.committed {
		return
	}
	_ = writer.file.Close()
	_ = os.Remove(writer.temporary)
}
