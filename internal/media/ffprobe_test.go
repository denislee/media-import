package media

import (
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// countFFprobe replaces runFFprobe with a stub that counts spawns, forces
// FFprobeAvailable to report true, and restores both when the test ends.
func countFFprobe(t *testing.T) *atomic.Int64 {
	t.Helper()
	var calls atomic.Int64
	// Resolve ffprobeOnce first: once it has run, patching ffprobePath is
	// enough to make FFprobeAvailable report true.
	FFprobeAvailable()
	oldPath := ffprobePath
	if ffprobePath == "" {
		ffprobePath = "/bin/true"
	}
	old := runFFprobe
	runFFprobe = func(string) (time.Time, error) {
		calls.Add(1)
		return time.Time{}, errors.New("stub")
	}
	t.Cleanup(func() {
		runFFprobe = old
		ffprobePath = oldPath
	})
	return &calls
}

func writeZeroFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, make([]byte, 100), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestNoFFprobeForImages(t *testing.T) {
	calls := countFFprobe(t)
	dir := t.TempDir()

	names := []string{"IMG-20230514-WA0001.png", "Screenshot_2023-05-14-18-30-12.png", "photo.heic"}
	for _, name := range names {
		p := filepath.Join(dir, name)
		writeZeroFile(t, p)
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		got, src := CaptureTime(p, Classify(p), fi, DateOptions{UseFFprobe: true})
		if src == SourceFFprobe {
			t.Errorf("%s: got source %s, want anything but ffprobe", name, src)
		}
		if name == names[0] {
			want := time.Date(2023, 5, 14, 0, 0, 0, 0, time.Local)
			if src != SourceFilename || !got.Equal(want) {
				t.Errorf("%s: got %v (%s), want %v (%s)", name, got, src, want, SourceFilename)
			}
		}
	}

	raw := filepath.Join(dir, "x.cr3")
	writeZeroFile(t, raw)
	fi, err := os.Stat(raw)
	if err != nil {
		t.Fatal(err)
	}
	CaptureTime(raw, Raw, fi, DateOptions{UseFFprobe: true})

	if n := calls.Load(); n != 0 {
		t.Errorf("ffprobe called %d times for stills, want 0", n)
	}
}

func TestFFprobeStillUsedForVideo(t *testing.T) {
	calls := countFFprobe(t)
	p := filepath.Join(t.TempDir(), "clip.mkv")
	writeZeroFile(t, p)
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}

	CaptureTime(p, Video, fi, DateOptions{UseFFprobe: true})
	if n := calls.Load(); n != 1 {
		t.Fatalf("ffprobe called %d times, want 1", n)
	}

	CaptureTime(p, Video, fi, DateOptions{UseFFprobe: false})
	if n := calls.Load(); n != 1 {
		t.Errorf("ffprobe called %d times with UseFFprobe false, want 1", n)
	}
}

func TestNoFFprobeWhenMvhdUnset(t *testing.T) {
	calls := countFFprobe(t)
	// mvhd with version 0 and creation time 0, wrapped in a moov box.
	mvhd := atom("mvhd", make([]byte, 100))
	p := filepath.Join(t.TempDir(), "v.mp4")
	if err := os.WriteFile(p, atom("moov", mvhd), 0o644); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}

	got, src := CaptureTime(p, Video, fi, DateOptions{UseFFprobe: true})
	if n := calls.Load(); n != 0 {
		t.Errorf("ffprobe called %d times with unset mvhd, want 0", n)
	}
	if src != SourceModTime {
		t.Errorf("got %v (%s), want source %s", got, src, SourceModTime)
	}
}
