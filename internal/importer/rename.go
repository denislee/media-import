package importer

import (
	"errors"
	"io/fs"
	"os"

	"golang.org/x/sys/unix"
)

// renameat2 and linkFile are replaced by tests.
var renameat2 = func(oldpath, newpath string) error {
	return unix.Renameat2(unix.AT_FDCWD, oldpath, unix.AT_FDCWD, newpath, unix.RENAME_NOREPLACE)
}

var linkFile = os.Link

// renameNoReplace is like rename(2) but fails with an error matching
// fs.ErrExist instead of replacing newpath. The link fallback is atomic; the
// Lstat fallback (for vfat/exfat without hard links) leaves a small race
// window.
func renameNoReplace(oldpath, newpath string) error {
	err := renameat2(oldpath, newpath)
	if err == nil {
		return nil
	}
	if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EOPNOTSUPP) {
		lerr := linkFile(oldpath, newpath)
		if lerr == nil {
			_ = os.Remove(oldpath)
			return nil
		}
		if errors.Is(lerr, fs.ErrExist) {
			return lerr
		}
		if errors.Is(lerr, unix.EPERM) || errors.Is(lerr, unix.EOPNOTSUPP) || errors.Is(lerr, unix.ENOSYS) || errors.Is(lerr, unix.EMLINK) {
			if _, serr := os.Lstat(newpath); serr == nil {
				return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: fs.ErrExist}
			} else if !errors.Is(serr, fs.ErrNotExist) {
				return serr
			}
			return os.Rename(oldpath, newpath)
		}
		return lerr
	}
	return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: err}
}
