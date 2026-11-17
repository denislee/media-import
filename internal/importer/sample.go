package importer

import (
	"crypto/sha256"
	"hash"
	"io"
	"os"
)

// digests are the checksums known for one file's content.
type digests struct {
	sha    []byte // SHA-256 of the whole file
	sample []byte // SHA-256 of the sampled blocks (see sampleRanges)
}

// sampleRanges are the blocks sampledSame compares (first, middle, last
// sampleSize bytes), or the whole file when it is not larger than they are.
// For larger files the ranges are disjoint and in file order.
func sampleRanges(size int64) [][2]int64 {
	if size <= 3*sampleSize {
		return [][2]int64{{0, size}}
	}
	mid := size/2 - sampleSize/2
	return [][2]int64{{0, sampleSize}, {mid, mid + sampleSize}, {size - sampleSize, size}}
}

// sampleHasher computes the sample digest of a size-byte stream written to
// it in order, so a copy gets it without reading anything again.
type sampleHasher struct {
	h      hash.Hash
	pos    int64
	size   int64
	ranges [][2]int64
}

func newSampleHasher(size int64) *sampleHasher {
	return &sampleHasher{h: sha256.New(), size: size, ranges: sampleRanges(size)}
}

func (s *sampleHasher) Write(p []byte) (int, error) {
	start, end := s.pos, s.pos+int64(len(p))
	for _, r := range s.ranges {
		if lo, hi := max(start, r[0]), min(end, r[1]); lo < hi {
			s.h.Write(p[lo-start : hi-start])
		}
	}
	s.pos = end
	return len(p), nil
}

// sum returns the digest, or nil if the stream was not size bytes long (the
// file changed while it was read).
func (s *sampleHasher) sum() []byte {
	if s.pos != s.size {
		return nil
	}
	return s.h.Sum(nil)
}

// sampleDigest reads only the sampled blocks of p and returns their digest
// and the number of bytes read.
func sampleDigest(p string, size int64) ([]byte, int64, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	h := sha256.New()
	pb := getBuf()
	defer putBuf(pb)
	var read int64
	for _, r := range sampleRanges(size) {
		for off := r[0]; off < r[1]; {
			buf := (*pb)[:min(int64(len(*pb)), r[1]-off)]
			n, err := f.ReadAt(buf, off)
			read += int64(n)
			h.Write(buf[:n])
			off += int64(n)
			if err != nil && (err != io.EOF || off < r[1]) {
				return nil, read, err
			}
		}
	}
	return h.Sum(nil), read, nil
}
