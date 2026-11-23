package collector

import (
	"fmt"
	"io/fs"
	"syscall"
)

func cgroupGeneration(files fs.FS, path string) string {
	info, err := fs.Stat(files, path)
	if path == "" || err != nil {
		return ""
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return fmt.Sprintf("%d:%d", stat.Dev, stat.Ino)
	}
	return ""
}
