// Package devices lists removable media and other mounted volumes that are
// likely sources for an import (USB sticks, SD cards, cameras, phones).
package devices

import (
	"bufio"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Volume is a mounted filesystem the user may import from.
type Volume struct {
	Label string
	Path  string
	FS    string
}

// List returns mounted removable volumes, MTP/gvfs mounts and anything under
// /media or /mnt.
func List() []Volume {
	var vols []Volume
	seen := map[string]bool{}
	add := func(v Volume) {
		if seen[v.Path] {
			return
		}
		seen[v.Path] = true
		vols = append(vols, v)
	}

	if f, err := os.Open("/proc/self/mountinfo"); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if v, ok := parseMountinfo(sc.Text()); ok && isCandidate(v.Path) {
				add(v)
			}
		}
		f.Close()
	}

	// gvfs (MTP phones, cameras via gphoto2) mounts live in a FUSE dir.
	gvfs := filepath.Join("/run/user", strconv.Itoa(os.Getuid()), "gvfs")
	if entries, err := os.ReadDir(gvfs); err == nil {
		for _, e := range entries {
			p := filepath.Join(gvfs, e.Name())
			add(Volume{Label: e.Name(), Path: p, FS: "gvfs"})
		}
	}

	sort.Slice(vols, func(i, j int) bool { return vols[i].Path < vols[j].Path })
	return vols
}

func isCandidate(p string) bool {
	for _, pre := range []string{"/run/media/", "/media/", "/mnt/"} {
		if strings.HasPrefix(p, pre) {
			return true
		}
	}
	return false
}

// parseMountinfo parses one line of /proc/self/mountinfo:
// 36 35 98:0 /mnt1 /mnt2 rw,noatime master:1 - ext3 /dev/root rw,errors=continue
func parseMountinfo(line string) (Volume, bool) {
	fields := strings.Fields(line)
	if len(fields) < 10 {
		return Volume{}, false
	}
	sep := -1
	for i := 6; i < len(fields); i++ {
		if fields[i] == "-" {
			sep = i
			break
		}
	}
	if sep < 0 || sep+1 >= len(fields) {
		return Volume{}, false
	}
	mp := unescape(fields[4])
	return Volume{Label: filepath.Base(mp), Path: mp, FS: fields[sep+1]}, true
}

// unescape decodes the octal escapes (\040 for space etc.) used by the kernel.
func unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
