//go:build !windows

package filetransfer

import (
	"fmt"
	"os"
)

func commitFile(temporary, destination string, overwrite bool) error {
	if overwrite {
		if err := os.Rename(temporary, destination); err != nil {
			return fmt.Errorf("replace destination: %w", err)
		}
		return nil
	}
	if err := os.Link(temporary, destination); err != nil {
		return fmt.Errorf("publish destination without overwrite: %w", err)
	}
	if err := os.Remove(temporary); err != nil {
		return fmt.Errorf("remove temporary upload link: %w", err)
	}
	return nil
}
