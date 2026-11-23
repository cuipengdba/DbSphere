//go:build !linux

package collector

import "io/fs"

func cgroupGeneration(fs.FS, string) string { return "" }
