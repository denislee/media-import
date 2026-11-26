package media

import (
	"bytes"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rwcarlsen/goexif/exif"
)

func goexifTime(r io.Reader) (time.Time, error) {
	x, err := exif.Decode(r)
	if err != nil && x == nil {
		return time.Time{}, err
	}
	return x.DateTime()
}

// tiffBlock builds a TIFF structure: IFD0 with DateTime (and, if exifIFD,
// a pointer to an Exif IFD holding DateTimeOriginal; otherwise
// DateTimeOriginal directly in IFD0, as CR3's CMT2 does). magic is 42 for
// plain TIFF. Empty strings omit the tag.
func tiffBlock(bo binary.ByteOrder, magic uint16, dt, dto string, exifIFD bool) []byte {
	var b bytes.Buffer
	if bo == binary.LittleEndian {
		b.WriteString("II")
	} else {
		b.WriteString("MM")
	}
	w := func(v any) { _ = binary.Write(&b, bo, v) } // a bytes.Buffer never fails
	w(magic)
	w(uint32(8))
	type ent struct {
		tag, typ uint16
		val      []byte // ASCII payload, or nil for a pointer
	}
	var ifd0, ifdE []ent
	if dt != "" {
		ifd0 = append(ifd0, ent{tagDateTime, typeASCII, append([]byte(dt), 0)})
	}
	if dto != "" {
		if exifIFD {
			ifdE = append(ifdE, ent{tagDateTimeOriginal, typeASCII, append([]byte(dto), 0)})
		} else {
			ifd0 = append(ifd0, ent{tagDateTimeOriginal, typeASCII, append([]byte(dto), 0)})
		}
	}
	if len(ifdE) > 0 {
		ifd0 = append(ifd0, ent{tagExifIFD, typeLong, nil})
	}
	if len(ifd0) == 0 {
		ifd0 = append(ifd0, ent{0x0100, 4, nil}) // ImageWidth, so the IFD is not empty
	}
	ifdLen := func(n int) int { return 2 + 12*n + 4 }
	ifd0Off := 8
	exifOff := ifd0Off + ifdLen(len(ifd0))
	dataOff := exifOff
	if len(ifdE) > 0 {
		dataOff += ifdLen(len(ifdE))
	}
	var data bytes.Buffer
	writeIFD := func(ents []ent) {
		w(uint16(len(ents)))
		for _, e := range ents {
			w(e.tag)
			w(e.typ)
			switch {
			case e.tag == tagExifIFD:
				w(uint32(1))
				w(uint32(exifOff))
			case e.val == nil:
				w(uint32(1))
				w(uint32(640))
			default:
				w(uint32(len(e.val)))
				w(uint32(dataOff + data.Len()))
				data.Write(e.val)
			}
		}
		w(uint32(0))
	}
	writeIFD(ifd0)
	if len(ifdE) > 0 {
		writeIFD(ifdE)
	}
	b.Write(data.Bytes())
	return b.Bytes()
}

func jpegSeg(marker byte, payload []byte) []byte {
	s := make([]byte, 4, 4+len(payload))
	s[0], s[1] = 0xFF, marker
	binary.BigEndian.PutUint16(s[2:], uint16(len(payload)+2))
	return append(s, payload...)
}

// jpegFile builds SOI, the given segments, a scan and EOI, plus padding.
func jpegFile(pad int, segs ...[]byte) []byte {
	b := []byte{0xFF, 0xD8}
	for _, s := range segs {
		b = append(b, s...)
	}
	b = append(b, jpegSeg(0xDA, []byte{1, 2, 3})...)
	b = append(b, make([]byte, pad)...)
	return append(b, 0xFF, 0xD9)
}

func exifSeg(tiff []byte) []byte { return jpegSeg(0xE1, append([]byte("Exif\x00\x00"), tiff...)) }

func bmffBox(typ string, payload ...[]byte) []byte {
	p := bytes.Join(payload, nil)
	b := make([]byte, 8, 8+len(p))
	binary.BigEndian.PutUint32(b, uint32(8+len(p)))
	copy(b[4:], typ)
	return append(b, p...)
}

func be16(v int) []byte { return binary.BigEndian.AppendUint16(nil, uint16(v)) }
func be32(v int) []byte { return binary.BigEndian.AppendUint32(nil, uint32(v)) }

// heifFile builds ftyp, a meta box (iinf with an hvc1 item and an Exif
// item, iloc version ilocVer pointing at the Exif data in mdat) and mdat.
func heifFile(tiff []byte, ilocVer byte) []byte {
	ftyp := bmffBox("ftyp", []byte("heic"), be32(0), []byte("mif1heic"))
	infe := func(id int, typ string) []byte {
		return bmffBox("infe", []byte{2, 0, 0, 0}, be16(id), be16(0), []byte(typ), []byte{0})
	}
	iinf := bmffBox("iinf", []byte{0, 0, 0, 0}, be16(2), infe(1, "hvc1"), infe(2, "Exif"))
	exifData := append(be32(0), tiff...) // 4-byte offset to the TIFF header, then TIFF
	mkMeta := func(dataOff int) []byte {
		item := func(id, off, length int) []byte {
			var x []byte
			if ilocVer == 2 {
				x = be32(id)
			} else {
				x = be16(id)
			}
			if ilocVer >= 1 {
				x = append(x, be16(0)...) // construction method 0
			}
			x = append(x, be16(0)...)         // data reference index
			x = append(x, be16(1)...)         // extent count
			x = append(x, be32(off)...)       // extent offset (4 bytes)
			return append(x, be32(length)...) // extent length (4 bytes)
		}
		items := append(item(1, 0, 0), item(2, dataOff, len(exifData))...)
		hdr := []byte{ilocVer, 0, 0, 0, 0x44, 0x00}
		if ilocVer == 2 {
			hdr = append(hdr, be32(2)...)
		} else {
			hdr = append(hdr, be16(2)...)
		}
		iloc := bmffBox("iloc", hdr, items)
		return bmffBox("meta", []byte{0, 0, 0, 0}, bmffBox("hdlr", make([]byte, 24)), iinf, iloc)
	}
	meta := mkMeta(0)
	dataOff := len(ftyp) + len(meta) + 8
	meta = mkMeta(dataOff)
	return bytes.Join([][]byte{ftyp, meta, bmffBox("mdat", exifData, make([]byte, 4096))}, nil)
}

func cr3File(cmt1, cmt2 []byte) []byte {
	ftyp := bmffBox("ftyp", []byte("crx "), be32(1), []byte("crx isom"))
	uuid := bmffBox("uuid", cr3UUID, bmffBox("CNCV", []byte("CanonCR3_001/00.09.00")), bmffBox("CMT1", cmt1), bmffBox("CMT2", cmt2))
	moov := bmffBox("moov", uuid, bmffBox("trak", make([]byte, 64)))
	return bytes.Join([][]byte{ftyp, moov, bmffBox("mdat", make([]byte, 8192))}, nil)
}

func rafFile(jpeg []byte) []byte {
	h := make([]byte, 160, 160+len(jpeg)+4096)
	copy(h, "FUJIFILMCCD-RAW 0201FF383501")
	binary.BigEndian.PutUint32(h[84:], uint32(len(h)))
	binary.BigEndian.PutUint32(h[88:], uint32(len(jpeg)))
	return append(append(h, jpeg...), make([]byte, 4096)...)
}

const (
	dOrig = "2021:06:07 08:09:10"
	dMod  = "2020:01:01 00:00:00"
)

var (
	tOrig = time.Date(2021, 6, 7, 8, 9, 10, 0, time.Local)
	tMod  = time.Date(2020, 1, 1, 0, 0, 0, 0, time.Local)
)

type countingReaderAt struct {
	r     io.ReaderAt
	n     atomic.Int64
	calls atomic.Int64
}

func (c *countingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	n, err := c.r.ReadAt(p, off)
	c.n.Add(int64(n))
	c.calls.Add(1)
	return n, err
}

func TestNativeEXIFFormats(t *testing.T) {
	le, be := binary.LittleEndian, binary.BigEndian
	app0 := jpegSeg(0xE0, []byte("JFIF\x00\x01\x01\x00\x00\x01\x00\x01\x00\x00"))
	xmp := jpegSeg(0xE1, []byte("http://ns.adobe.com/xap/1.0/\x00<x:xmpmeta/>"))
	phone := jpegFile(1<<16, app0, exifSeg(tiffBlock(be, 42, dMod, dOrig, true)))
	for _, tc := range []struct {
		name   string
		data   []byte
		want   time.Time
		oracle bool // goexif must agree
	}{
		{"jpeg-phone", phone, tOrig, true},
		{"jpeg-le", jpegFile(100, exifSeg(tiffBlock(le, 42, dMod, dOrig, true))), tOrig, true},
		{"jpeg-xmp-first", jpegFile(100, app0, xmp, exifSeg(tiffBlock(le, 42, dMod, dOrig, true))), tOrig, false},
		{"jpeg-fill-bytes", append([]byte{0xFF, 0xD8, 0xFF}, phone[2:]...), tOrig, true},
		{"jpeg-datetime-only", jpegFile(100, exifSeg(tiffBlock(le, 42, dMod, "", false))), tMod, true},
		{"jpeg-bad-original", jpegFile(100, exifSeg(tiffBlock(le, 42, dMod, "0000:00:00 00:00:00", true))), tMod, false},
		{"tiff-cr2-le", tiffBlock(le, 42, dMod, dOrig, true), tOrig, true},
		{"tiff-nef-be", tiffBlock(be, 42, dMod, dOrig, true), tOrig, true},
		{"orf-IIRO", tiffBlock(le, 0x4f52, dMod, dOrig, true), tOrig, false},
		{"orf-MMOR", tiffBlock(be, 0x4f52, dMod, dOrig, true), tOrig, false},
		{"rw2-IIU", tiffBlock(le, 0x0055, dMod, dOrig, true), tOrig, false},
		{"raf", rafFile(jpegFile(100, exifSeg(tiffBlock(be, 42, dMod, dOrig, true)))), tOrig, false},
		{"heic-iloc0", heifFile(tiffBlock(be, 42, dMod, dOrig, true), 0), tOrig, false},
		{"heic-iloc1", heifFile(tiffBlock(le, 42, dMod, dOrig, true), 1), tOrig, false},
		{"heic-iloc2", heifFile(tiffBlock(be, 42, "", dOrig, true), 2), tOrig, false},
		{"cr3", cr3File(tiffBlock(le, 42, dMod, "", false), tiffBlock(le, 42, "", dOrig, false)), tOrig, false},
		{"cr3-cmt1-only", cr3File(tiffBlock(le, 42, dMod, "", false), tiffBlock(le, 42, "", "", false)), tMod, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := exifTimeAt(bytes.NewReader(tc.data), int64(len(tc.data)))
			if err != nil || !got.Equal(tc.want) {
				t.Fatalf("native: %v %v, want %v", got, err, tc.want)
			}
			if tc.oracle {
				o, err := goexifTime(bytes.NewReader(tc.data))
				if err != nil || !o.Equal(got) {
					t.Fatalf("goexif disagrees: %v %v", o, err)
				}
			}
		})
	}
}

func TestNativeEXIFNoDate(t *testing.T) {
	le := binary.LittleEndian
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"png", append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 4096)...)},
		{"jpeg-no-exif", jpegFile(100, jpegSeg(0xE0, []byte("JFIF\x00")))},
		{"jpeg-truncated", jpegFile(0, exifSeg(tiffBlock(le, 42, dMod, dOrig, true)))[:30]},
		{"tiff-no-dates", tiffBlock(le, 42, "", "", false)},
		{"tiff-ifd-loop-out-of-range", []byte("II*\x00\xff\xff\xff\x7f")},
		{"heic-no-exif-item", bmffBox("ftyp", []byte("heic"), be32(0))},
		{"empty", nil},
	} {
		if _, err := exifTimeAt(bytes.NewReader(tc.data), int64(len(tc.data))); err == nil {
			t.Errorf("%s: expected an error", tc.name)
		}
	}
}

// The point of the native reader: a RAW file with its EXIF near the start is
// read in a few small pieces, however large the file is.
func TestNativeEXIFReadsLittle(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.cr2")
	writeSyntheticTIFF(t, p, 30<<20)
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fi, _ := f.Stat()
	c := &countingReaderAt{r: f}
	got, err := exifTimeAt(c, fi.Size())
	if err != nil || !got.Equal(wantSynthetic) {
		t.Fatal(got, err)
	}
	if c.n.Load() > 64<<10 {
		t.Errorf("read %d bytes in %d calls", c.n.Load(), c.calls.Load())
	}
	allocs := testing.Benchmark(func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_, _ = exifTime(p, fi.Size())
		}
	}).AllocedBytesPerOp()
	if allocs > 1<<20 {
		t.Errorf("allocated %d bytes per parse", allocs)
	}
}

// Golden check against goexif on real photos: MEDIA_GOLDEN_DIR=~/Pictures go test ./internal/media -run Golden -v
func TestGoldenAgainstGoexif(t *testing.T) {
	dir := os.Getenv("MEDIA_GOLDEN_DIR")
	if dir == "" {
		t.Skip("MEDIA_GOLDEN_DIR not set")
	}
	n := 0
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || (Classify(p) != Image && Classify(p) != Raw) {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return nil
		}
		defer f.Close()
		fi, err := f.Stat()
		if err != nil {
			return nil
		}
		want, werr := goexifTime(f)
		got, gerr := exifTime(p, fi.Size())
		n++
		switch {
		case werr == nil && (gerr != nil || !got.Equal(want)):
			t.Errorf("%s: native %v %v, goexif %v", p, got, gerr, want)
		case werr != nil && gerr == nil:
			t.Logf("%s: native found %v where goexif did not (%v)", p, got, werr)
		}
		return nil
	})
	t.Logf("%d files compared", n)
}

// rchar returns the process's total bytes read via read(2), from /proc/self/io.
func rchar(t *testing.T) int64 {
	t.Helper()
	data, err := os.ReadFile("/proc/self/io")
	if err != nil {
		t.Skipf("/proc/self/io not readable: %v", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "rchar:") {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(line, "rchar:")), 10, 64)
		if err != nil {
			t.Skipf("cannot parse %q: %v", line, err)
		}
		return n
	}
	t.Skip("/proc/self/io has no rchar line")
	return 0
}

// A JPEG with no EXIF and a multi-megabyte scan must not be read to the end:
// the segment walk stops at the start of scan.
func TestEXIFLessJPEGReadsLittle(t *testing.T) {
	var b bytes.Buffer
	b.Write([]byte{0xFF, 0xD8})
	b.Write(jpegSeg(0xE0, append([]byte("JFIF\x00"), make([]byte, 9)...))) // APP0, length 16
	b.Write(jpegSeg(0xDB, make([]byte, 65)))                               // DQT, length 67
	b.Write(jpegSeg(0xDA, make([]byte, 6)))                                // SOS, length 8
	b.Write(make([]byte, 3<<20))                                           // scan data
	b.Write([]byte{0xFF, 0xD9})                                            // EOI
	p := filepath.Join(t.TempDir(), "nodate.jpg")
	if err := os.WriteFile(p, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}

	before := rchar(t)
	if _, err := exifTime(p, fi.Size()); err == nil {
		t.Fatal("expected no EXIF date")
	}
	delta := rchar(t) - before
	if delta >= 8<<10 {
		t.Errorf("read %d bytes, want < %d", delta, 8<<10)
	}
}
