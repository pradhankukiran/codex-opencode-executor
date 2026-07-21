//go:build !unix

package runner

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// DirLock is a best-effort exclusive lock using O_EXCL create on non-unix.
type DirLock struct {
	path string
}

func AcquireDirLock(lockDir, canonicalDir string) (*DirLock, error) {
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		return nil, fmt.Errorf("create lock dir: %w", err)
	}
	path := filepath.Join(lockDir, lockFileName(canonicalDir))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return nil, fmt.Errorf("directory already in use by another runner")
		}
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	_, _ = fmt.Fprintf(f, "%d\n%s\n%d\n", os.Getpid(), canonicalDir, time.Now().Unix())
	_ = f.Close()
	return &DirLock{path: path}, nil
}

func (l *DirLock) Release() error {
	if l == nil || l.path == "" {
		return nil
	}
	err := os.Remove(l.path)
	l.path = ""
	return err
}

func lockFileName(canonicalDir string) string {
	sum := sha256.Sum256([]byte(canonicalDir))
	return hex.EncodeToString(sum[:]) + ".lock"
}
