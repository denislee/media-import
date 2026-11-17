package media

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DateSource says where a capture time came from.
type DateSource string

const (
	SourceEXIF     DateSource = "exif"
	SourceVideo    DateSource = "video-header"
	SourceFFprobe  DateSource = "ffprobe"
	SourceFilename DateSource = "filename"
	SourceModTime  DateSource = "mtime"
)

// errMvhdUnset marks an mvhd atom that exists but carries creation time 0.
var errMvhdUnset = errors.New("creation time not set")

// minValid rejects obviously bogus timestamps (unset camera clocks, 1904 epochs).
var minValid = time.Date(1980, 1, 2, 0, 0, 0, 0, time.UTC)

func plausible(t time.Time) bool {
	return !t.IsZero() && t.After(minValid) && t.Before(time.Now().Add(48*time.Hour))
}

// DateOptions controls which (slower) strategies CaptureTime may use.
type DateOptions struct {
	UseFFprobe bool
}

var (
	ffprobeOnce sync.Once
	ffprobePath string
)

// FFprobeAvailable reports whether ffprobe is on PATH.
func FFprobeAvailable() bool {
	ffprobeOnce.Do(func() { ffprobePath, _ = exec.LookPath("ffprobe") })
	return ffprobePath != ""
}

// CaptureTime determines the best guess for when a media file was taken.
// Images and RAW files use embedded EXIF only; videos read the QuickTime mvhd
// atom first and skip ffprobe when that atom exists but is unusable. ffprobe
// (opt-in) covers the remaining videos and audio, then the filename, then the
// file's modification time. The returned time is in local time.
func CaptureTime(path string, kind Kind, fi os.FileInfo, opt DateOptions) (time.Time, DateSource) {
	mvhdFound := false
	switch kind {
	case Image, Raw:
		// Stills never run ffprobe: the native EXIF reader already covers
		// JPEG/TIFF/RAW/HEIC/CR3/RAF and ffprobe rarely has dates for them.
		if t, err := exifTime(path); err == nil && plausible(t) {
			return t, SourceEXIF
		}
	case Video:
		if isQuickTime(path) {
			t, err := quickTimeCreation(path)
			if err == nil && plausible(t) {
				return t.Local(), SourceVideo
			}
			if err == nil || errors.Is(err, errMvhdUnset) {
				// The atom exists but is unusable; ffprobe reads the same
				// atom, so asking it would only repeat this result.
				mvhdFound = true
			}
		}
	}
	if opt.UseFFprobe && FFprobeAvailable() && (kind == Video && !mvhdFound || kind == Audio) {
		if t, err := runFFprobe(path); err == nil && plausible(t) {
			return t.Local(), SourceFFprobe
		}
	}
	if t, ok := FilenameTime(filepath.Base(path)); ok {
		return t, SourceFilename
	}
	return fi.ModTime(), SourceModTime
}

func isQuickTime(path string) bool {
	switch Ext(path) {
	case "mp4", "m4v", "mov", "qt", "3gp", "3g2", "insv", "lrv", "m4a", "f4v":
		return true
	}
	return false
}

// mac epoch: 1904-01-01 UTC.
var macEpoch = time.Date(1904, 1, 1, 0, 0, 0, 0, time.UTC)

// quickTimeCreation reads the creation time from the moov/mvhd atom of an
// ISO-BMFF (MP4/MOV/3GP) file without reading the media payload.
func quickTimeCreation(path string) (time.Time, error) {
	f, err := os.Open(path)
	if err != nil {
		return time.Time{}, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return time.Time{}, err
	}
	return mvhdTime(f, 0, fi.Size(), 0)
}

func mvhdTime(r io.ReaderAt, start, end int64, depth int) (time.Time, error) {
	if depth > 4 {
		return time.Time{}, errors.New("atom nesting too deep")
	}
	var hdr [16]byte
	for off := start; off+8 <= end; {
		if _, err := r.ReadAt(hdr[:8], off); err != nil {
			return time.Time{}, err
		}
		size := int64(binary.BigEndian.Uint32(hdr[:4]))
		typ := string(hdr[4:8])
		hlen := int64(8)
		switch size {
		case 1: // 64-bit size follows
			if _, err := r.ReadAt(hdr[8:16], off+8); err != nil {
				return time.Time{}, err
			}
			size = int64(binary.BigEndian.Uint64(hdr[8:16]))
			hlen = 16
		case 0: // extends to end of file
			size = end - off
		}
		if size < hlen || off+size > end {
			return time.Time{}, errors.New("malformed atom")
		}
		switch typ {
		case "moov":
			return mvhdTime(r, off+hlen, off+size, depth+1)
		case "mvhd":
			var b [20]byte
			if _, err := r.ReadAt(b[:], off+hlen); err != nil {
				return time.Time{}, err
			}
			var secs uint64
			if b[0] == 1 { // version 1: 64-bit times
				secs = binary.BigEndian.Uint64(b[4:12])
			} else {
				secs = uint64(binary.BigEndian.Uint32(b[4:8]))
			}
			if secs == 0 {
				return time.Time{}, errMvhdUnset
			}
			return macEpoch.Add(time.Duration(secs) * time.Second), nil
		}
		off += size
	}
	return time.Time{}, errors.New("mvhd atom not found")
}

// runFFprobe is ffprobeTime; tests replace it to count spawns.
var runFFprobe = ffprobeTime

func ffprobeTime(path string) (time.Time, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, ffprobePath, "-v", "quiet", "-print_format", "json",
		"-show_entries", "format_tags=creation_time,date:stream_tags=creation_time", path).Output()
	if err != nil {
		return time.Time{}, err
	}
	var res struct {
		Format struct {
			Tags map[string]string `json:"tags"`
		} `json:"format"`
		Streams []struct {
			Tags map[string]string `json:"tags"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return time.Time{}, err
	}
	cands := []string{res.Format.Tags["creation_time"], res.Format.Tags["date"]}
	for _, s := range res.Streams {
		cands = append(cands, s.Tags["creation_time"])
	}
	for _, c := range cands {
		if t, ok := parseLooseTime(c); ok {
			return t, nil
		}
	}
	return time.Time{}, errors.New("no creation time tag")
}

func parseLooseTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05", "2006:01:02 15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// Matches e.g. IMG_20230514_183012, VID-20230514-WA0001, 2023-05-14 18.30.12,
// PXL_20230514_183012345, Screenshot_2023-05-14-18-30-12.
var filenameRe = regexp.MustCompile(
	`(?:^|[^0-9])((?:19|20)\d{2})[-_.]?(0[1-9]|1[0-2])[-_.]?(0[1-9]|[12]\d|3[01])` +
		`(?:[-_ T.]?([01]\d|2[0-3])[-_.:h]?([0-5]\d)[-_.:m]?([0-5]\d))?`)

// FilenameTime extracts a date (and optionally time) embedded in a filename.
func FilenameTime(name string) (time.Time, bool) {
	m := filenameRe.FindStringSubmatch(name)
	if m == nil {
		return time.Time{}, false
	}
	n := func(s string) int { v, _ := strconv.Atoi(s); return v }
	h, mi, se := 0, 0, 0
	if m[4] != "" {
		h, mi, se = n(m[4]), n(m[5]), n(m[6])
	}
	t := time.Date(n(m[1]), time.Month(n(m[2])), n(m[3]), h, mi, se, 0, time.Local)
	if t.Day() != n(m[3]) || !plausible(t) { // reject e.g. Feb 31
		return time.Time{}, false
	}
	return t, true
}
