//go:build !windows

package config

import (
	"errors"
	"golang.org/x/sys/unix"
	"os"
)

func tryConfigLock(f *os.File) (bool, error) {
	err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) {
		return false, nil
	}
	return err == nil, err
}
func unlockConfigFile(f *os.File) { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN) }
