//go:build !linux

package remoteobs

import "os"

func lockSpool(_ *os.File) error { return ErrInvalid }
