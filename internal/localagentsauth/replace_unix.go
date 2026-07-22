//go:build !windows

package localagentsauth

import "os"

func replaceFile(source, destination string) error {
	return os.Rename(source, destination)
}
