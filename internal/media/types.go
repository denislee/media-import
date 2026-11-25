// Package media classifies media files and extracts their capture date.
package media

import (
	"path/filepath"
	"strings"
)

// Kind is a broad media category.
type Kind int

// The media kinds; Unknown is a file that is not imported.
const (
	Unknown Kind = iota
	Image
	Raw
	Video
	Audio
)

func (k Kind) String() string {
	switch k {
	case Image:
		return "Photos"
	case Raw:
		return "RAW"
	case Video:
		return "Videos"
	case Audio:
		return "Audio"
	default:
		return "Other"
	}
}

// AllKinds lists the importable kinds in display order.
var AllKinds = []Kind{Image, Raw, Video, Audio}

var extKinds = map[string]Kind{}

func register(k Kind, exts ...string) {
	for _, e := range exts {
		extKinds[e] = k
	}
}

func init() {
	register(Image, "jpg", "jpeg", "jpe", "jfif", "png", "gif", "bmp", "tif", "tiff",
		"webp", "heic", "heif", "avif", "jxl", "svg", "ico", "psd", "xcf")
	register(Raw, "cr2", "cr3", "crw", "nef", "nrw", "arw", "srf", "sr2", "dng",
		"orf", "rw2", "raf", "srw", "pef", "x3f", "3fr", "iiq", "erf", "kdc", "mrw", "mos", "rwl")
	register(Video, "mp4", "m4v", "mov", "qt", "avi", "mkv", "webm", "mts", "m2ts", "ts",
		"3gp", "3g2", "wmv", "asf", "mpg", "mpeg", "mpe", "vob", "flv", "f4v", "ogv",
		"insv", "lrv", "dv", "mxf")
	register(Audio, "mp3", "wav", "flac", "aac", "m4a", "ogg", "oga", "opus", "wma",
		"aif", "aiff", "amr", "alac", "ape", "mka", "wv", "mid", "midi")
}

// Ext returns the lower-case extension of path without the dot.
func Ext(path string) string {
	return strings.ToLower(strings.TrimPrefix(filepath.Ext(path), "."))
}

// Classify returns the media kind of a file based on its extension.
func Classify(path string) Kind {
	return extKinds[Ext(path)]
}
