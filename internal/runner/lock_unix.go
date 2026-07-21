//go:build unix

package runner

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// DirLock holds an exclusive flock on a per-directory lock file.
type DirLock struct {
	path string
	file *os.File
}

// AcquireDirLock obtains an exclusive non-blocking lock for canonicalDir.
// lockDir is where lock files live (created with 0700).
// Stale locks are recovered automatically: flock is released by the kernel when
// the holding process exits, so a dead holder's lock cannot block forever.
func AcquireDirLock(lockDir, canonicalDir string) (*DirLock, error) {
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		return nil, fmt.Errorf("create lock dir: %w", err)
	}

	name := lockFileName(canonicalDir)
	path := filepath.Join(lockDir, name)

	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		holder := readLockHolder(path)
		if holder != "" {
			return nil, fmt.Errorf("directory already in use by another runner (pid %s)", holder)
		}
		return nil, fmt.Errorf("directory already in use by another runner")
	}

	if err := f.Truncate(0); err != nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
		return nil, fmt.Errorf("write lock file: %w", err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
		return nil, fmt.Errorf("seek lock file: %w", err)
	}
	payload := fmt.Sprintf("%d\n%s\n%d\n", os.Getpid(), canonicalDir, time.Now().Unix())
	if _, err := f.WriteString(payload); err != nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
		return nil, fmt.Errorf("write lock file: %w", err)
	}
	_ = f.Sync()

	return &DirLock{path: path, file: f}, nil
}

// Release unlocks and closes the lock file.
func (l *DirLock) Release() error {
	if l == nil || l.file == nil {
		return nil
	}
	_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	err := l.file.Close()
	l.file = nil
	return err
}

func lockFileName(canonicalDir string) string {
	sum := sha256.Sum256([]byte(canonicalDir))
	return hex.EncodeToString(sum[:]) + ".lock"
}

func readLockHolder(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(strings.SplitN(string(data), "\n", 2)[0])
	if line == "" {
		return ""
	}
	if _, err := strconv.Atoi(line); err != nil {
		return ""
	}
	return line
}
