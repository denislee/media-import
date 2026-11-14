package media

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestClassify(t *testing.T) {
	cases := map[string]Kind{
		"a/IMG_0001.JPG": Image, "x.heic": Image, "x.CR3": Raw, "x.nef": Raw,
		"clip.MOV": Video, "x.mts": Video, "song.flac": Audio, "notes.txt": Unknown, "noext": Unknown,
	}
	for p, want := range cases {
		if got := Classify(p); got != want {
			t.Errorf("Classify(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestFilenameTime(t *testing.T) {
	cases := []struct {
		name string
		want time.Time
		ok   bool
	}{
		{"IMG_20230514_183012.jpg", time.Date(2023, 5, 14, 18, 30, 12, 0, time.Local), true},
		{"PXL_20230514_183012345.mp4", time.Date(2023, 5, 14, 18, 30, 12, 0, time.Local), true},
		{"VID-20230514-WA0001.mp4", time.Date(2023, 5, 14, 0, 0, 0, 0, time.Local), true},
		{"2021-12-31 23.59.58.jpg", time.Date(2021, 12, 31, 23, 59, 58, 0, time.Local), true},
		{"Screenshot_2022-01-02-03-04-05.png", time.Date(2022, 1, 2, 3, 4, 5, 0, time.Local), true},
		{"IMG_20230231_000000.jpg", time.Time{}, false}, // Feb 31
		{"DSC_1234.JPG", time.Time{}, false},
	}
	for _, c := range cases {
		got, ok := FilenameTime(c.name)
		if ok != c.ok || (ok && !got.Equal(c.want)) {
			t.Errorf("FilenameTime(%q) = %v,%v want %v,%v", c.name, got, ok, c.want, c.ok)
		}
	}
}

func atom(typ string, body []byte) []byte {
	b := make([]byte, 8, 8+len(body))
	binary.BigEndian.PutUint32(b, uint32(8+len(body)))
	copy(b[4:], typ)
	return append(b, body...)
}

// WriteTestMP4 writes a minimal ISO-BMFF file whose mvhd has creation time t.
func writeTestMP4(t *testing.T, path string, when time.Time) {
	t.Helper()
	mvhd := make([]byte, 100)
	binary.BigEndian.PutUint32(mvhd[4:8], uint32(when.Sub(macEpoch)/time.Second))
	var buf bytes.Buffer
	buf.Write(atom("ftyp", []byte("isom\x00\x00\x02\x00isomiso2mp41")))
	buf.Write(atom("free", make([]byte, 32)))
	buf.Write(atom("moov", append(atom("mvhd", mvhd), atom("trak", make([]byte, 16))...)))
	buf.Write(atom("mdat", make([]byte, 64)))
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestQuickTimeCreation(t *testing.T) {
	when := time.Date(2022, 8, 9, 10, 11, 12, 0, time.UTC)
	p := filepath.Join(t.TempDir(), "clip.mp4")
	writeTestMP4(t, p, when)
	got, err := quickTimeCreation(p)
	if err != nil || !got.Equal(when) {
		t.Fatalf("got %v, %v; want %v", got, err, when)
	}
	fi, _ := os.Stat(p)
	ct, src := CaptureTime(p, Video, fi, DateOptions{})
	if src != SourceVideo || !ct.Equal(when) {
		t.Fatalf("CaptureTime = %v (%s)", ct, src)
	}
}

func TestCaptureTimeFallsBackToMtime(t *testing.T) {
	p := filepath.Join(t.TempDir(), "DSC_0001.jpg")
	os.WriteFile(p, []byte("not a real jpeg"), 0o644)
	mt := time.Date(2019, 3, 4, 5, 6, 7, 0, time.Local)
	os.Chtimes(p, mt, mt)
	fi, _ := os.Stat(p)
	got, src := CaptureTime(p, Image, fi, DateOptions{})
	if src != SourceModTime || !got.Equal(mt) {
		t.Fatalf("got %v (%s)", got, src)
	}
}
