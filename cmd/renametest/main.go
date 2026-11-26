// Command renametest is a hardware-check tool for the renameNoReplace fallback
// in internal/importer/rename.go on vfat/exfat/gvfs mounts, not installed by
// make install.
//
// It reports the filesystem type of each given directory, then probes which
// branch renameNoReplace takes there: atomic renameat2(RENAME_NOREPLACE), the
// hard-link fallback, or the racy Lstat fallback.
//
// Usage:
//
//	go run ./cmd/renametest <dir>...
package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: go run ./cmd/renametest <dir>...")
		os.Exit(1)
	}
	for _, dir := range os.Args[1:] {
		check(dir)
	}
}

// check probes one directory and prints its results.
func check(dir string) {
	tmp, err := os.MkdirTemp(dir, "rn-")
	if err != nil {
		fmt.Printf("%s: %v\n", dir, err)
		return
	}
	defer os.RemoveAll(tmp)

	fmt.Printf("%s (temp %s)\n", dir, tmp)
	printFS(tmp)

	a := filepath.Join(tmp, "a")
	b := filepath.Join(tmp, "b")
	free := filepath.Join(tmp, "free")
	c := filepath.Join(tmp, "c")

	// (i) NOREPLACE onto an existing name must fail with EEXIST.
	var errExist, errFree, errLink error
	if errExist = freshFiles(tmp); errExist != nil {
		fmt.Printf("  (i)   cannot create probe files: %v\n", errExist)
	} else {
		errExist = unix.Renameat2(unix.AT_FDCWD, a, unix.AT_FDCWD, b, unix.RENAME_NOREPLACE)
		fmt.Printf("  (i)   noreplace onto existing b: %s\n", fmtErr(errExist))
	}

	// (ii) NOREPLACE onto a free name: the branch renameNoReplace prefers.
	if errFree = freshFiles(tmp); errFree != nil {
		fmt.Printf("  (ii)  cannot create probe files: %v\n", errFree)
	} else {
		errFree = unix.Renameat2(unix.AT_FDCWD, a, unix.AT_FDCWD, free, unix.RENAME_NOREPLACE)
		fmt.Printf("  (ii)  noreplace onto free target: %s\n", fmtErr(errFree))
	}

	// (iii) Hard link to a free name: the first fallback.
	if errLink = freshFiles(tmp); errLink != nil {
		fmt.Printf("  (iii) cannot create probe files: %v\n", errLink)
	} else {
		errLink = os.Link(a, c)
		fmt.Printf("  (iii) link to free name c: %s\n", fmtErr(errLink))
	}

	fmt.Printf("  branch: %s\n", branch(errExist, errFree, errLink))
}

// freshFiles writes a fresh pair of probe files, a with content "a" and b with
// content "b".
func freshFiles(dir string) error {
	if err := os.WriteFile(filepath.Join(dir, "a"), []byte("a"), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "b"), []byte("b"), 0o644)
}

// printFS reports the filesystem type of path, naming the magics that matter
// for renameNoReplace.
func printFS(path string) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		fmt.Printf("  fs: statfs: %v\n", err)
		return
	}
	t := uint64(uint32(st.Type))
	fmt.Printf("  fs: 0x%x %s\n", t, fsName(t))
}

func fsName(t uint64) string {
	switch t {
	case 0x4d44:
		return "vfat/msdos"
	case 0x2011bab0:
		return "exfat"
	case 0x65735546:
		return "fuse (gvfs/MTP)"
	case 0x9123683e:
		return "btrfs"
	case 0xef53:
		return "ext4"
	case 0x01021994:
		return "tmpfs"
	case 0x5346544e:
		return "ntfs"
	default:
		return "unknown"
	}
}

// branch names the renameNoReplace branch the probes imply, mirroring the
// decision in internal/importer/rename.go.
func branch(errExist, errFree, errLink error) string {
	switch {
	case errFree == nil && errors.Is(errExist, unix.EEXIST):
		return "renameat2"
	case isUnsupported(errFree) && errLink == nil:
		return "link fallback"
	case isUnsupported(errFree) && (errors.Is(errLink, unix.EPERM) ||
		errors.Is(errLink, unix.EOPNOTSUPP) ||
		errors.Is(errLink, unix.ENOSYS) ||
		errors.Is(errLink, unix.EMLINK)):
		return "lstat fallback (racy)"
	default:
		return fmt.Sprintf("error: exist=%v free=%v link=%v", errExist, errFree, errLink)
	}
}

// isUnsupported reports whether err makes renameNoReplace fall back from
// renameat2.
func isUnsupported(err error) bool {
	return errors.Is(err, unix.EINVAL) ||
		errors.Is(err, unix.ENOSYS) ||
		errors.Is(err, unix.EOPNOTSUPP)
}

func fmtErr(err error) string {
	if err == nil {
		return "ok"
	}
	return err.Error()
}
