package media

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"strings"
	"time"
)

// A small EXIF reader that finds the capture date with a handful of ReadAt
// calls of a few KB each, instead of goexif's whole-file reads. It knows:
//
//   - JPEG: the APP1 "Exif" segment, found by walking segment headers;
//   - TIFF and TIFF-based RAW (CR2, NEF, ARW, DNG, PEF, …), including the
//     ORF (IIRO/IIRS/MMOR) and RW2 (IIU) header variants;
//   - Fujifilm RAF: the embedded JPEG preview;
//   - ISO-BMFF: the Exif item of HEIC/HEIF/AVIF and Canon CR3's CMT boxes.
//
// Anything else is reported as errNoContainer and not read further.

var (
	errNoContainer = errors.New("format has no EXIF container")
	errNoEXIFDate  = errors.New("no EXIF date")
	errMalformed   = errors.New("malformed EXIF")
)

type exifFormat int

const (
	fmtNone exifFormat = iota
	fmtJPEG
	fmtTIFF
	fmtRAF
	fmtBMFF
)

const (
	jpegScanLimit = 1 << 20 // stop walking JPEG segments past this offset
	metaBoxLimit  = 4 << 20 // largest HEIF meta box read into memory
	maxIFDEntries = 1024    // sanity bound for one IFD
	maxASCII      = 64      // longest date string read
	exifLayout    = "2006:01:02 15:04:05"
	exifLayoutAlt = "2006-01-02 15:04:05"
)

// exifTime returns DateTimeOriginal (falling back to DateTime) of the size-byte
// file at path, using the native reader only.
func exifTime(path string, size int64) (time.Time, error) {
	f, err := os.Open(path)
	if err != nil {
		return time.Time{}, err
	}
	defer f.Close()
	t, _, err := exifTimeAt(f, size)
	return t, err
}

// exifTimeAt sniffs the container format of r (size bytes) and reads its
// EXIF capture date.
func exifTimeAt(r io.ReaderAt, size int64) (time.Time, exifFormat, error) {
	var hdr [16]byte
	n, _ := r.ReadAt(hdr[:], 0)
	h := hdr[:n]
	switch {
	case len(h) >= 3 && h[0] == 0xFF && h[1] == 0xD8 && h[2] == 0xFF:
		t, err := jpegDate(r, size)
		return t, fmtJPEG, err
	case isTIFFHeader(h):
		t, err := tiffDate(r, size)
		return t, fmtTIFF, err
	case bytes.HasPrefix(h, []byte("FUJIFILMCCD-RAW")):
		t, err := rafDate(r, size)
		return t, fmtRAF, err
	case len(h) >= 12 && string(h[4:8]) == "ftyp":
		t, err := bmffDate(r, size)
		return t, fmtBMFF, err
	}
	return time.Time{}, fmtNone, errNoContainer
}

func tiffOrder(h []byte) binary.ByteOrder {
	switch {
	case len(h) < 4:
		return nil
	case h[0] == 'I' && h[1] == 'I':
		return binary.LittleEndian
	case h[0] == 'M' && h[1] == 'M':
		return binary.BigEndian
	}
	return nil
}

func isTIFFHeader(h []byte) bool {
	bo := tiffOrder(h)
	if bo == nil {
		return false
	}
	switch bo.Uint16(h[2:4]) {
	case 42, // TIFF and most RAW formats
		0x4f52, 0x5352, // Olympus ORF: IIRO / IIRS / MMOR
		0x0055: // Panasonic RW2: IIU\0
		return true
	}
	return false
}

// jpegDate walks the JPEG segment headers up to the first scan and parses
// every APP1 Exif segment it meets.
func jpegDate(r io.ReaderAt, size int64) (time.Time, error) {
	var b [10]byte
	for off := int64(2); off+4 <= size && off < jpegScanLimit; {
		if _, err := r.ReadAt(b[:4], off); err != nil {
			return time.Time{}, err
		}
		if b[0] != 0xFF {
			return time.Time{}, errMalformed
		}
		m := b[1]
		switch {
		case m == 0xFF: // fill byte before a marker
			off++
			continue
		case m == 0x01 || m == 0xD8 || (m >= 0xD0 && m <= 0xD7): // no length field
			off += 2
			continue
		case m == 0xDA || m == 0xD9: // start of scan / end of image: no EXIF after this
			return time.Time{}, errNoEXIFDate
		}
		l := int64(binary.BigEndian.Uint16(b[2:4]))
		if l < 2 {
			return time.Time{}, errMalformed
		}
		if m == 0xE1 && l >= 2+6+8 {
			if _, err := r.ReadAt(b[4:10], off+4); err == nil && string(b[4:10]) == "Exif\x00\x00" {
				if t, err := tiffDate(io.NewSectionReader(r, off+10, l-8), l-8); err == nil {
					return t, nil
				}
			}
		}
		off += 2 + l
	}
	return time.Time{}, errNoEXIFDate
}

type tiffReader struct {
	r    io.ReaderAt
	size int64
	bo   binary.ByteOrder
}

type ifdEntry struct {
	tag, typ uint16
	count    uint32
	val      [4]byte
}

const (
	tagDateTime         = 0x0132
	tagExifIFD          = 0x8769
	tagDateTimeOriginal = 0x9003
	typeASCII           = 2
	typeLong            = 4
	typeIFD             = 13
)

// tiffDate reads a TIFF structure (header at offset 0 of r): IFD0's
// DateTime, and DateTimeOriginal from the Exif IFD (or from IFD0 itself, as
// in CR3's CMT2 box). DateTimeOriginal wins when it parses.
func tiffDate(r io.ReaderAt, size int64) (time.Time, error) {
	var h [8]byte
	if _, err := r.ReadAt(h[:], 0); err != nil {
		return time.Time{}, err
	}
	if !isTIFFHeader(h[:]) {
		return time.Time{}, errMalformed
	}
	t := tiffReader{r: r, size: size, bo: tiffOrder(h[:])}
	ifd0, err := t.ifd(int64(t.bo.Uint32(h[4:8])))
	if err != nil {
		return time.Time{}, err
	}
	var dt, dto string
	exifOff := int64(-1)
	for _, e := range ifd0 {
		switch e.tag {
		case tagDateTime:
			dt = t.ascii(e)
		case tagDateTimeOriginal:
			dto = t.ascii(e)
		case tagExifIFD:
			if e.typ == typeLong || e.typ == typeIFD {
				exifOff = int64(t.bo.Uint32(e.val[:]))
			}
		}
	}
	if exifOff > 0 && dto == "" {
		if ents, err := t.ifd(exifOff); err == nil {
			for _, e := range ents {
				if e.tag == tagDateTimeOriginal {
					dto = t.ascii(e)
				}
			}
		}
	}
	for _, s := range []string{dto, dt} {
		if tm, ok := parseEXIFTime(s); ok {
			return tm, nil
		}
	}
	return time.Time{}, errNoEXIFDate
}

func (t *tiffReader) ifd(off int64) ([]ifdEntry, error) {
	if off < 8 || off+2 > t.size {
		return nil, errMalformed
	}
	var c [2]byte
	if _, err := t.r.ReadAt(c[:], off); err != nil {
		return nil, err
	}
	n := int(t.bo.Uint16(c[:]))
	if n == 0 || n > maxIFDEntries || off+2+int64(n)*12 > t.size {
		return nil, errMalformed
	}
	buf := make([]byte, n*12)
	if _, err := t.r.ReadAt(buf, off+2); err != nil {
		return nil, err
	}
	ents := make([]ifdEntry, n)
	for i := range ents {
		b := buf[i*12:]
		ents[i] = ifdEntry{tag: t.bo.Uint16(b[0:2]), typ: t.bo.Uint16(b[2:4]), count: t.bo.Uint32(b[4:8])}
		copy(ents[i].val[:], b[8:12])
	}
	return ents, nil
}

// ascii returns an ASCII tag's value up to its first NUL ("" if unreadable).
func (t *tiffReader) ascii(e ifdEntry) string {
	if e.typ != typeASCII || e.count == 0 {
		return ""
	}
	n := int64(min(e.count, maxASCII))
	var b []byte
	if e.count <= 4 {
		b = e.val[:n]
	} else {
		off := int64(t.bo.Uint32(e.val[:]))
		if off+n > t.size {
			return ""
		}
		b = make([]byte, n)
		if _, err := t.r.ReadAt(b, off); err != nil {
			return ""
		}
	}
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}

// parseEXIFTime parses an EXIF date in local time, like goexif does.
func parseEXIFTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{exifLayout, exifLayoutAlt} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// rafDate reads the EXIF of the JPEG preview a Fujifilm RAF embeds; its
// offset and length are big-endian words at byte 84 of the header.
func rafDate(r io.ReaderAt, size int64) (time.Time, error) {
	var b [8]byte
	if _, err := r.ReadAt(b[:], 84); err != nil {
		return time.Time{}, err
	}
	off, n := int64(binary.BigEndian.Uint32(b[:4])), int64(binary.BigEndian.Uint32(b[4:]))
	if off <= 0 || n <= 0 || off+n > size {
		return time.Time{}, errMalformed
	}
	return jpegDate(io.NewSectionReader(r, off, n), n)
}

// box is one ISO-BMFF box: its type, where its payload starts and ends.
type box struct {
	typ        string
	start, end int64 // payload
}

// walkBoxes calls fn for each box in [start, end) until fn returns false.
func walkBoxes(r io.ReaderAt, start, end int64, fn func(box) bool) error {
	var h [16]byte
	for off := start; off+8 <= end; {
		if _, err := r.ReadAt(h[:8], off); err != nil {
			return err
		}
		size, hlen := int64(binary.BigEndian.Uint32(h[:4])), int64(8)
		switch size {
		case 1:
			if _, err := r.ReadAt(h[8:16], off+8); err != nil {
				return err
			}
			size, hlen = int64(binary.BigEndian.Uint64(h[8:16])), 16
		case 0:
			size = end - off
		}
		if size < hlen || size > end-off {
			return errMalformed
		}
		if !fn(box{typ: string(h[4:8]), start: off + hlen, end: off + size}) {
			return nil
		}
		off += size
	}
	return nil
}

// cr3UUID is the Canon box inside moov that holds the CMT1..CMT4 TIFF blocks.
var cr3UUID = []byte{0x85, 0xc0, 0xb6, 0x87, 0x82, 0x0f, 0x11, 0xe0, 0x81, 0x11, 0xf4, 0xce, 0x46, 0x2b, 0x6a, 0x48}

// bmffDate handles HEIF/AVIF (meta → Exif item) and CR3 (moov → Canon uuid
// → CMT2, then CMT1).
func bmffDate(r io.ReaderAt, size int64) (time.Time, error) {
	var (
		t     time.Time
		found bool
		ferr  = errNoEXIFDate
	)
	err := walkBoxes(r, 0, size, func(b box) bool {
		switch b.typ {
		case "meta":
			t, ferr = heifDate(r, b, size)
		case "moov":
			t, ferr = cr3Date(r, b)
		default:
			return true
		}
		found = ferr == nil
		return !found
	})
	if found {
		return t, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	return time.Time{}, ferr
}

func cr3Date(r io.ReaderAt, moov box) (time.Time, error) {
	cmt := map[string]box{}
	err := walkBoxes(r, moov.start, moov.end, func(b box) bool {
		if b.typ != "uuid" || b.end-b.start < 16 {
			return true
		}
		var id [16]byte
		if _, err := r.ReadAt(id[:], b.start); err != nil || !bytes.Equal(id[:], cr3UUID) {
			return true
		}
		_ = walkBoxes(r, b.start+16, b.end, func(c box) bool {
			if strings.HasPrefix(c.typ, "CMT") {
				cmt[c.typ] = c
			}
			return true
		})
		return false
	})
	if err != nil {
		return time.Time{}, err
	}
	for _, name := range []string{"CMT2", "CMT1"} { // Exif IFD, then IFD0
		if c, ok := cmt[name]; ok {
			if t, err := tiffDate(io.NewSectionReader(r, c.start, c.end-c.start), c.end-c.start); err == nil {
				return t, nil
			}
		}
	}
	return time.Time{}, errNoEXIFDate
}

// heifDate finds the item of type "Exif" in the meta box and parses it. The
// item data starts with a 4-byte offset to the TIFF header.
func heifDate(r io.ReaderAt, meta box, fileSize int64) (time.Time, error) {
	n := meta.end - meta.start
	if n < 4 || n > metaBoxLimit {
		return time.Time{}, errMalformed
	}
	buf := make([]byte, n)
	if _, err := r.ReadAt(buf, meta.start); err != nil {
		return time.Time{}, err
	}
	br := bytes.NewReader(buf)
	var iinf, iloc []byte
	_ = walkBoxes(br, 4, n, func(b box) bool { // skip meta's version/flags
		switch b.typ {
		case "iinf":
			iinf = buf[b.start:b.end]
		case "iloc":
			iloc = buf[b.start:b.end]
		}
		return true
	})
	id, ok := heifExifItem(iinf)
	if !ok {
		return time.Time{}, errNoEXIFDate
	}
	off, length, ok := heifItemExtent(iloc, id)
	if !ok || length < 8 || off+length > fileSize {
		return time.Time{}, errMalformed
	}
	var hb [4]byte
	if _, err := r.ReadAt(hb[:], off); err != nil {
		return time.Time{}, err
	}
	skip := 4 + int64(binary.BigEndian.Uint32(hb[:]))
	if skip >= length {
		return time.Time{}, errMalformed
	}
	return tiffDate(io.NewSectionReader(r, off+skip, length-skip), length-skip)
}

// heifExifItem returns the ID of the first "Exif" item in an iinf payload.
func heifExifItem(iinf []byte) (uint32, bool) {
	if len(iinf) < 6 {
		return 0, false
	}
	start := int64(6) // version/flags + u16 entry count
	if iinf[0] != 0 {
		start = 8 // u32 entry count
	}
	var (
		id    uint32
		found bool
	)
	_ = walkBoxes(bytes.NewReader(iinf), start, int64(len(iinf)), func(b box) bool {
		if b.typ != "infe" {
			return true
		}
		p := iinf[b.start:b.end]
		if len(p) < 4 {
			return true
		}
		var typ []byte
		var itemID uint32
		switch v := p[0]; {
		case v == 2 && len(p) >= 12:
			itemID, typ = uint32(binary.BigEndian.Uint16(p[4:6])), p[8:12]
		case v >= 3 && len(p) >= 14:
			itemID, typ = binary.BigEndian.Uint32(p[4:8]), p[10:14]
		default:
			return true
		}
		if string(typ) == "Exif" {
			id, found = itemID, true
			return false
		}
		return true
	})
	return id, found
}

// heifItemExtent returns the file offset and length of item id's first
// extent from an iloc payload (construction method 0, file offsets, only).
func heifItemExtent(p []byte, id uint32) (int64, int64, bool) {
	if len(p) < 8 {
		return 0, 0, false
	}
	ver := p[0]
	offSize, lenSize := int(p[4]>>4), int(p[4]&0xf)
	baseSize, idxSize := int(p[5]>>4), 0
	if ver == 1 || ver == 2 {
		idxSize = int(p[5] & 0xf)
	}
	pos := 6
	need := func(n int) bool { return pos+n <= len(p) }
	rd := func(n int) uint64 {
		var v uint64
		for i := range n {
			v = v<<8 | uint64(p[pos+i])
		}
		pos += n
		return v
	}
	var count uint64
	if ver < 2 {
		if !need(2) {
			return 0, 0, false
		}
		count = rd(2)
	} else {
		if !need(4) {
			return 0, 0, false
		}
		count = rd(4)
	}
	for range count {
		idLen := 2
		if ver == 2 {
			idLen = 4
		}
		hdr := idLen + 2 + baseSize + 2
		if ver == 1 || ver == 2 {
			hdr += 2
		}
		if !need(hdr) {
			return 0, 0, false
		}
		itemID := uint32(rd(idLen))
		method := uint64(0)
		if ver == 1 || ver == 2 {
			method = rd(2) & 0xf
		}
		pos += 2 // data_reference_index
		base := rd(baseSize)
		extents := int(rd(2))
		ext := idxSize + offSize + lenSize
		if !need(extents * ext) {
			return 0, 0, false
		}
		if itemID == id {
			if method != 0 || extents == 0 {
				return 0, 0, false
			}
			pos += idxSize
			off := rd(offSize)
			length := rd(lenSize)
			return int64(base + off), int64(length), true
		}
		pos += extents * ext
	}
	return 0, 0, false
}
