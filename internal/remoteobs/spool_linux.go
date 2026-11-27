package remoteobs

import (
	"os"
	"syscall"
)

func lockSpool(file *os.File) error {
	return syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}
