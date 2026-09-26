package config

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestConfigWriteLockProcessHelper(t *testing.T) {
	path := os.Getenv("TWILIGHT_TEST_CONFIG_LOCK")
	if path == "" {
		return
	}
	if err := WithWriteLock(context.Background(), path, func() error {
		fmt.Println("lock-acquired")
		time.Sleep(30 * time.Second)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestConfigWriteLockCrossProcessCrashRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	child := exec.Command(os.Args[0], "-test.run=^TestConfigWriteLockProcessHelper$")
	child.Env = append(os.Environ(), "TWILIGHT_TEST_CONFIG_LOCK="+path)
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
	ready := make(chan bool, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if scanner.Text() == "lock-acquired" {
				ready <- true
				return
			}
		}
		ready <- false
	}()
	select {
	case acquired := <-ready:
		if !acquired {
			t.Fatal("child failed to acquire lock")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("child lock timeout")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	called := false
	err = WithWriteLock(ctx, path, func() error { called = true; return nil })
	if !errors.Is(err, context.DeadlineExceeded) || called {
		t.Fatalf("concurrent process bypassed lock: called=%v err=%v", called, err)
	}
	if err = child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = child.Wait()
	ctx, release := context.WithTimeout(context.Background(), 2*time.Second)
	defer release()
	if err := WithWriteLock(ctx, path, func() error { return nil }); err != nil {
		t.Fatalf("crashed process retained lock: %v", err)
	}
}
