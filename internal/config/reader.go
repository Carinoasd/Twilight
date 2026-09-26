package config

import (
	"context"
	"strings"
	"time"
)

type Reader struct {
	path string
}

func NewReader(path string) Reader {
	return Reader{path: strings.TrimSpace(path)}
}

func (r Reader) Path() string {
	if r.path == "" {
		return defaultConfigPath()
	}
	return r.path
}

func (r Reader) Read() (Config, error) {
	return Load(r.Path())
}

// ReadLocked is for process startup and standalone readers. Callers already
// holding the configuration write lock use Read to avoid a recursive lock.
func (r Reader) ReadLocked() (Config, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	var cfg Config
	err := WithWriteLock(ctx, r.Path(), func() error {
		var err error
		cfg, err = r.Read()
		return err
	})
	return cfg, err
}
