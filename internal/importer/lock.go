package importer

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

var (
	// ErrLocked is returned by LockFile when another open file holds the lock.
	ErrLocked = errors.New("lock is held by another process")
	// ErrLockUnsupported wraps the error of a filesystem without flock (some FUSE and NFS mounts).
	ErrLockUnsupported = errors.New("file locking is not supported here")
	// ErrDestBusy is returned by Run when another import into the same destination is running.
	ErrDestBusy = errors.New("another import into this destination is running")
)

// LockFile takes an exclusive, non-blocking lock on path, creating it (and its
// directory) if needed. The caller closes the returned file to release it.
//
// flock is per open file description, so a second LockFile of the same path
// fails even in the same process.
func LockFile(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		if errors.Is(err, unix.ENOLCK) || errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.ENOSYS) {
			return nil, fmt.Errorf("%w: %v", ErrLockUnsupported, err)
		}
		return nil, err
	}
	return f, nil
}
