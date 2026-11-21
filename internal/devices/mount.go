package devices

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
)

// Unmounted is a removable partition that has a filesystem but no mount point.
type Unmounted struct {
	Dev   string // /dev/sdc1
	Label string
	FS    string
	Size  int64
}

// ErrPasswordRequired means mounting needs root and sudo wants a password.
var ErrPasswordRequired = errors.New("password required")

// ErrBadPassword means sudo rejected the supplied password.
var ErrBadPassword = errors.New("incorrect password")

type lsblkDev struct {
	Name        string     `json:"name"`
	Path        string     `json:"path"`
	Label       *string    `json:"label"`
	FSType      *string    `json:"fstype"`
	Mountpoints []*string  `json:"mountpoints"`
	RM          bool       `json:"rm"`
	Hotplug     bool       `json:"hotplug"`
	Tran        *string    `json:"tran"`
	Type        string     `json:"type"`
	Size        int64      `json:"size"`
	Children    []lsblkDev `json:"children"`
}

// listUnmounted returns hot-pluggable/removable partitions (USB sticks, SD
// cards, cameras in mass-storage mode) that are not mounted yet.
func listUnmounted() []Unmounted {
	out, err := exec.Command("lsblk", "-J", "-b", "-o",
		"NAME,PATH,LABEL,FSTYPE,MOUNTPOINTS,RM,HOTPLUG,TRAN,TYPE,SIZE").Output()
	if err != nil {
		return nil
	}
	return parseLsblk(out)
}

func parseLsblk(data []byte) []Unmounted {
	var doc struct {
		Blockdevices []lsblkDev `json:"blockdevices"`
	}
	if json.Unmarshal(data, &doc) != nil {
		return nil
	}
	var res []Unmounted
	var walk func(d lsblkDev, removable bool)
	walk = func(d lsblkDev, removable bool) {
		removable = removable || d.RM || d.Hotplug || (d.Tran != nil && *d.Tran == "usb")
		if removable && mountable(d) {
			u := Unmounted{Dev: d.Path, FS: *d.FSType, Size: d.Size}
			if d.Label != nil {
				u.Label = *d.Label
			}
			if u.Dev == "" {
				u.Dev = "/dev/" + d.Name
			}
			res = append(res, u)
		}
		for _, c := range d.Children {
			walk(c, removable)
		}
	}
	for _, d := range doc.Blockdevices {
		walk(d, false)
	}
	return res
}

func mountable(d lsblkDev) bool {
	if d.FSType == nil || d.Size == 0 {
		return false
	}
	switch *d.FSType {
	case "", "swap", "crypto_LUKS", "LVM2_member", "linux_raid_member", "zfs_member", "BitLocker":
		return false
	}
	for _, m := range d.Mountpoints {
		if m != nil && *m != "" {
			return false
		}
	}
	return true
}

// Name is a human label for the partition.
func (u Unmounted) Name() string {
	if u.Label != "" {
		return u.Label
	}
	return filepath.Base(u.Dev)
}

// Mount mounts the partition and returns its mount point. It tries udisks
// first (no root needed), then sudo. If sudo needs a password and password is
// empty, it returns ErrPasswordRequired so the caller can ask the user.
func Mount(u Unmounted, password string) (string, error) {
	if password == "" {
		if mp, err := udisksMount(u.Dev); err == nil {
			return mp, nil
		}
	}
	return sudoMount(u, password)
}

func udisksMount(dev string) (string, error) {
	p, err := exec.LookPath("udisksctl")
	if err != nil {
		return "", err
	}
	out, err := exec.Command(p, "mount", "--no-user-interaction", "-b", dev).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("udisksctl: %s", strings.TrimSpace(string(out)))
	}
	// "Mounted /dev/sdc1 at /run/media/dns/CARD"
	s := strings.TrimSpace(string(out))
	if i := strings.LastIndex(s, " at "); i >= 0 {
		return strings.TrimSuffix(s[i+4:], "."), nil
	}
	return "", errors.New("udisksctl: unexpected output: " + s)
}

// ownerOpts lists filesystems without Unix ownership, which need uid/gid
// mount options so the user can write (and delete, in move mode).
var ownerOpts = map[string]bool{
	"vfat": true, "exfat": true, "ntfs": true, "ntfs3": true,
	"hfsplus": true, "iso9660": true, "udf": true,
}

func sudoMount(u Unmounted, password string) (string, error) {
	usr := os.Getenv("USER")
	if cu, err := user.Current(); err == nil {
		usr = cu.Username
	}
	base := filepath.Join("/run/media", usr)
	mp := filepath.Join(base, mountName(u.Name()))
	for i := 1; busy(mp); i++ {
		mp = filepath.Join(base, fmt.Sprintf("%s_%d", mountName(u.Name()), i))
	}

	opts := "nosuid,nodev"
	if ownerOpts[u.FS] {
		opts += fmt.Sprintf(",uid=%d,gid=%d", os.Getuid(), os.Getgid())
	}
	err := sudoRun(password, `mkdir -p "$1" && mount -o "$2" -- "$3" "$1"`, mp, opts, u.Dev)
	if err != nil {
		return "", wrap("mount "+u.Dev, err)
	}
	return mp, nil
}

// Eject unmounts a volume and, where possible, powers off / ejects the
// device so it can be unplugged safely. It tries gio (gvfs), then udisks,
// then sudo; like Mount, it returns ErrPasswordRequired when sudo needs a
// password and password is empty.
func Eject(v Volume, password string) error {
	if v.FS == "gvfs" {
		out, err := exec.Command("gio", "mount", "-u", v.Path).CombinedOutput()
		if err != nil {
			return fmt.Errorf("eject %s: %s", v.Label, strings.TrimSpace(string(out)+" "+err.Error()))
		}
		return nil
	}
	if !strings.HasPrefix(v.Dev, "/dev/") {
		return fmt.Errorf("eject %s: not a block device (%s)", v.Label, v.Dev)
	}
	if password == "" {
		if p, err := exec.LookPath("udisksctl"); err == nil {
			if exec.Command(p, "unmount", "--no-user-interaction", "-b", v.Dev).Run() == nil {
				// Best effort: fails harmlessly if other partitions are still mounted.
				_ = exec.Command(p, "power-off", "--no-user-interaction", "-b", v.Dev).Run()
				return nil
			}
		}
	}
	// Remove the mount point only if it lives in the per-user media dir;
	// then eject the media (ignored if the device does not support it).
	rm := "0"
	if strings.HasPrefix(v.Path, "/run/media/") {
		rm = "1"
	}
	err := sudoRun(password, `sync && umount -- "$1" || exit
[ "$3" = 1 ] && rmdir -- "$1" 2>/dev/null
command -v eject >/dev/null && eject -- "$2" 2>/dev/null
exit 0`, v.Path, v.Dev, rm)
	return wrap("eject "+v.Label, err)
}

// sudoRun runs script under sudo with sh positional args, so nothing is
// interpolated by the shell. With an empty password sudo runs
// non-interactively; otherwise the password is fed on stdin.
func sudoRun(password, script string, args ...string) error {
	sudo, err := exec.LookPath("sudo")
	if err != nil {
		return errors.New("neither udisksctl nor sudo is available")
	}
	a := []string{"-p", ""}
	if password == "" {
		a = append(a, "-n")
	} else {
		a = append(a, "-S")
	}
	a = append(a, "--", "sh", "-c", script, "sh")
	cmd := exec.Command(sudo, append(a, args...)...)
	if password != "" {
		cmd.Stdin = strings.NewReader(password + "\n")
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		switch {
		case password == "" && strings.Contains(msg, "password is required"):
			return ErrPasswordRequired
		case strings.Contains(msg, "incorrect password") || strings.Contains(msg, "Sorry, try again"):
			return ErrBadPassword
		case msg == "":
			msg = err.Error()
		}
		return errors.New(msg)
	}
	return nil
}

func wrap(what string, err error) error {
	if err == nil || errors.Is(err, ErrPasswordRequired) || errors.Is(err, ErrBadPassword) {
		return err
	}
	return fmt.Errorf("%s: %w", what, err)
}

// busy reports whether p exists and is non-empty or already a mount point.
func busy(p string) bool {
	entries, err := os.ReadDir(p)
	if err != nil {
		return !os.IsNotExist(err)
	}
	return len(entries) > 0 || isMountpoint(p)
}

func isMountpoint(p string) bool {
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if v, ok := parseMountinfo(line); ok && v.Path == p {
			return true
		}
	}
	return false
}

func mountName(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '/' || r == 0 || r < ' ' {
			return '_'
		}
		return r
	}, strings.TrimSpace(s))
	if s == "" || s == "." || s == ".." {
		return "disk"
	}
	return s
}
