//go:build !windows

package updatefile

import "os"

func transientSharingError(err error) bool        { return false }
func openStateFile(path string) (*os.File, error) { return os.Open(path) }

func Replace(source, target string) error { return os.Rename(source, target) }
