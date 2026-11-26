package media

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Fixed geometry of the synthetic TIFF: header, IFD0, ExifIFD, then the two
// ASCII date strings.
const (
	tiffHeaderLen = 8
	ifd0Len       = 2 + 2*12 + 4
	exifIFDLen    = 2 + 12 + 4

	ifd0Off = tiffHeaderLen
	exifOff = ifd0Off + ifd0Len
	s1Off   = exifOff + exifIFDLen
	s2Off   = s1Off + 20
)

const (
	syntheticDate  = "2020:01:01 00:00:00\x00"
	syntheticDate2 = "2021:06:07 08:09:10\x00"
)

var wantSynthetic = time.Date(2021, 6, 7, 8, 9, 10, 0, time.Local)

// padByte is the filler used after the headers: deterministic, non-repeating
// bytes with 0xFF avoided so parsers cannot mistake it for padding runs.
func padByte(k int) byte {
	c := byte(k*31) ^ byte(k>>9)
	if c == 0xFF {
		c = 0xFE
	}
	return c
}

// writeSyntheticTIFF writes a little-endian TIFF whose ExifIFD carries
// DateTimeOriginal, followed by padding filler bytes.
func writeSyntheticTIFF(t testing.TB, path string, padding int) {
	t.Helper()
	var buf bytes.Buffer
	le := binary.LittleEndian

	put16 := func(v uint16) {
		var b [2]byte
		le.PutUint16(b[:], v)
		buf.Write(b[:])
	}
	put32 := func(v uint32) {
		var b [4]byte
		le.PutUint32(b[:], v)
		buf.Write(b[:])
	}
	entry := func(tag, typ uint16, count, value uint32) {
		put16(tag)
		put16(typ)
		put32(count)
		put32(value)
	}

	// Header: byte order, magic 42, IFD0 offset.
	buf.WriteString("II")
	put16(42)
	put32(ifd0Off)

	// IFD0: DateTime (0x0132) and the ExifIFD pointer (0x8769).
	put16(2)
	entry(0x0132, 2, 20, s1Off)
	entry(0x8769, 4, 1, exifOff)
	put32(0)

	// ExifIFD: DateTimeOriginal (0x9003).
	put16(1)
	entry(0x9003, 2, 20, s2Off)
	put32(0)

	buf.WriteString(syntheticDate)
	buf.WriteString(syntheticDate2)

	for k := range padding {
		buf.WriteByte(padByte(k))
	}

	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writePseudoPNG writes a file of exactly size bytes that starts with a PNG
// signature and continues with filler bytes (no EXIF anywhere).
func writePseudoPNG(t testing.TB, path string, size int) {
	t.Helper()
	b := make([]byte, size)
	copy(b, "\x89PNG\r\n\x1a\n")
	for k := 8; k < len(b); k++ {
		b[k] = padByte(k - 8)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSyntheticTIFFDate(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.cr2")
	writeSyntheticTIFF(t, p, 1<<20)
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	got, src := CaptureTime(p, Raw, fi, DateOptions{})
	if src != SourceEXIF || !got.Equal(wantSynthetic) {
		t.Fatalf("CaptureTime = %v (%s), want %v (%s)", got, src, wantSynthetic, SourceEXIF)
	}
}

func BenchmarkCaptureTimeRAW(b *testing.B) {
	p := filepath.Join(b.TempDir(), "x.cr2")
	writeSyntheticTIFF(b, p, 30<<20)
	fi, err := os.Stat(p)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		CaptureTime(p, Raw, fi, DateOptions{})
	}
}

func BenchmarkCaptureTimePNG(b *testing.B) {
	p := filepath.Join(b.TempDir(), "shot.png")
	writePseudoPNG(b, p, 30<<20)
	fi, err := os.Stat(p)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		CaptureTime(p, Image, fi, DateOptions{})
	}
}
