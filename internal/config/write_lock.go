package config

import (
	"context"
	"os"
	"path/filepath"
	"time"
)

// The sidecar inode is never removed: unlinking a held lock allows a second
// writer to lock a different inode. The OS releases the lock after a crash.
func WithWriteLock(ctx context.Context, path string, apply func() error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path+".write.lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		locked, err := tryConfigLock(f)
		if err != nil {
			return err
		}
		if locked {
			defer unlockConfigFile(f)
			return apply()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}
