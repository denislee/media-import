package importer

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestResumeAfterInterruption(t *testing.T) {
	src, dst := setup(t)
	jpath := filepath.Join(t.TempDir(), "journal")
	o := baseOpts(src, dst)
	o.Journal = jpath
	// One worker: the two A.JPG sources must get A.jpg and A_1.jpg in walk
	// order, since the test below assumes 100CANON's copy is A.jpg.
	o.Workers = 1
	if _, err := Run(context.Background(), o, nil, nil); err != nil {
		t.Fatal(err)
	}

	// Simulate a crash: one file only "begun" (its copy finished but the D
	// record was lost), one begun with a leftover .part, rest done.
	rs, err := ReadJournal(jpath)
	if err != nil || rs.Total != 3 || rs.DoneCount() != 3 {
		t.Fatalf("journal %+v %v", rs, err)
	}
	png := filepath.Join(src, "IMG_20200102_030405.png")
	pngDst := filepath.Join(dst, "2020/01/02/IMG_20200102_030405.png")
	delete(rs.Done, png)
	rs.Pending[png] = pngDst

	jpg := filepath.Join(src, "DCIM", "100CANON", "A.JPG")
	jpgDst := filepath.Join(dst, "2021/06/07/A.jpg")
	delete(rs.Done, jpg)
	rs.Pending[jpg] = jpgDst
	os.Remove(jpgDst)
	part := filepath.Join(filepath.Dir(jpgDst), ".A.jpg.part")
	write(t, part, "aa", time.Now())

	o = baseOpts(src, dst)
	o.Journal, o.Resume = jpath, rs
	sum, err := Run(context.Background(), o, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Resumed != 2 || sum.Copied != 1 || sum.Failed != 0 || sum.Verified != 1 {
		t.Fatalf("resume summary %+v", sum)
	}
	if _, err := os.Stat(part); !os.IsNotExist(err) {
		t.Error("leftover .part not removed")
	}
	exists(t, jpgDst)
	if sum.Bytes != sum.TotalBytes {
		t.Errorf("bytes %d of %d", sum.Bytes, sum.TotalBytes)
	}
}

func TestPause(t *testing.T) {
	src, dst := setup(t)
	g := &Gate{}
	g.Pause()
	done := make(chan Progress)
	go func() {
		o := baseOpts(src, dst)
		o.Pause = g
		s, _ := Run(context.Background(), o, nil, nil)
		done <- s
	}()
	select {
	case <-done:
		t.Fatal("import finished while paused")
	case <-time.After(200 * time.Millisecond):
	}
	g.Resume()
	select {
	case s := <-done:
		if s.Copied != 3 {
			t.Fatalf("after resume %+v", s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("did not resume")
	}
}

func TestVerifyDetectsCorruption(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	write(t, p, "hello", time.Now())
	nop := func() error { return nil }
	if ok, err := verifyFile(p, []byte("not the hash"), nop, func(int64) {}); ok || err != nil {
		t.Fatalf("mismatch not detected: %v %v", ok, err)
	}
	want, _ := fileHash(p)
	if ok, err := verifyFile(p, want, nop, func(int64) {}); !ok || err != nil {
		t.Fatalf("match not detected: %v %v", ok, err)
	}
}

func TestRebase(t *testing.T) {
	rs := &ResumeState{
		Done:    map[string]bool{"/run/media/u/CARD/DCIM/a.jpg": true, "/elsewhere/b.jpg": true},
		Pending: map[string]string{"/run/media/u/CARD/c.jpg": "/out/c.jpg"},
	}
	rs.Rebase("/run/media/u/CARD", "/run/media/u/CARD_1")
	if !rs.Done["/run/media/u/CARD_1/DCIM/a.jpg"] || !rs.Done["/elsewhere/b.jpg"] || rs.Pending["/run/media/u/CARD_1/c.jpg"] != "/out/c.jpg" {
		t.Fatalf("%+v", rs)
	}
}

func TestSameContent(t *testing.T) {
	d := t.TempDir()
	big := make([]byte, 3<<20+5)
	for i := range big {
		big[i] = byte(i)
	}
	a, b, c, e := filepath.Join(d, "a"), filepath.Join(d, "b"), filepath.Join(d, "c"), filepath.Join(d, "e")
	os.WriteFile(a, big, 0o644)
	os.WriteFile(b, big, 0o644)
	big[len(big)-1]++
	os.WriteFile(c, big, 0o644)
	os.WriteFile(e, big[:10], 0o644)
	nop := func() error { return nil }
	var seen int64
	if ok, err := sameContent(a, b, nop, func(n int64) { seen += n }); !ok || err != nil || seen != int64(len(big)) {
		t.Fatalf("identical: %v %v seen=%d", ok, err, seen)
	}
	if ok, _ := sameContent(a, c, nop, func(int64) {}); ok {
		t.Fatal("last byte differs but reported same")
	}
	if ok, _ := sameContent(a, e, nop, func(int64) {}); ok {
		t.Fatal("different length reported same")
	}
}

func fileHash(p string) ([]byte, error) {
	return hashFile(p, func() error { return nil }, func(int64) {}, false)
}
