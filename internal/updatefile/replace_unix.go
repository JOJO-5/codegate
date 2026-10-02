//go:build !windows

package updatefile

import "os"

func Replace(source, target string) error { return os.Rename(source, target) }
