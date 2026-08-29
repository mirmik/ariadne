//go:build windows

package filetransfer

import (
	"fmt"

	"golang.org/x/sys/windows"
)

func commitFile(temporary, destination string, overwrite bool) error {
	source, err := windows.UTF16PtrFromString(temporary)
	if err != nil {
		return err
	}
	target, err := windows.UTF16PtrFromString(destination)
	if err != nil {
		return err
	}
	flags := uint32(windows.MOVEFILE_WRITE_THROUGH)
	if overwrite {
		flags |= windows.MOVEFILE_REPLACE_EXISTING
	}
	if err := windows.MoveFileEx(source, target, flags); err != nil {
		return fmt.Errorf("publish destination: %w", err)
	}
	return nil
}
