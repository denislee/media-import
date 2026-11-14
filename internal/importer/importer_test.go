package importer

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/denislee/media-import/internal/media"
)

func write(t *testing.T, p, data string, mt time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mt, mt); err != nil {
		t.Fatal(err)
	}
}

func baseOpts(src, dst string) Options {
	return Options{
		Sources:     []string{src},
		Dest:        dst,
		DirTemplate: "{YYYY}/{MM}/{DD}",
		Kinds:       map[media.Kind]bool{media.Image: true, media.Video: true, media.Raw: true},
		SkipDups:    true,
		Verify:      true,
	}
}

var (
	t1 = time.Date(2021, 6, 7, 8, 9, 10, 0, time.Local)
	t2 = time.Date(2022, 12, 1, 0, 0, 0, 0, time.Local)
)

func setup(t *testing.T) (src, dst string) {
	root := t.TempDir()
	src, dst = filepath.Join(root, "card"), filepath.Join(root, "out")
	write(t, filepath.Join(src, "DCIM", "100CANON", "A.JPG"), "aaa", t1)
	write(t, filepath.Join(src, "DCIM", "101CANON", "A.JPG"), "different", t1) // name clash
	write(t, filepath.Join(src, "IMG_20200102_030405.png"), "png", t2)         // filename date wins
	write(t, filepath.Join(src, "readme.txt"), "ignored", t1)
	write(t, filepath.Join(src, ".hidden", "B.jpg"), "hidden", t1)
	return
}

func exists(t *testing.T, p string) os.FileInfo {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatalf("expected %s: %v", p, err)
	}
	return fi
}

func TestImportCopy(t *testing.T) {
	src, dst := setup(t)
	sum, err := Run(context.Background(), baseOpts(src, dst), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Copied != 3 || sum.Failed != 0 || sum.Skipped != 0 {
		t.Fatalf("summary %+v errs=%v", sum.Progress, sum.Errors)
	}
	a := exists(t, filepath.Join(dst, "2021/06/07/A.jpg"))
	exists(t, filepath.Join(dst, "2021/06/07/A_1.jpg"))
	exists(t, filepath.Join(dst, "2020/01/02/IMG_20200102_030405.png"))
	if !a.ModTime().Equal(t1) {
		t.Errorf("mtime not preserved: %v", a.ModTime())
	}
	if _, err := os.Stat(filepath.Join(src, "DCIM", "100CANON", "A.JPG")); err != nil {
		t.Error("copy removed source")
	}

	// Second run: everything is a duplicate.
	sum, err = Run(context.Background(), baseOpts(src, dst), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Copied != 0 || sum.Skipped != 3 {
		t.Fatalf("rerun summary %+v", sum.Progress)
	}
}

func TestImportDryRun(t *testing.T) {
	src, dst := setup(t)
	o := baseOpts(src, dst)
	o.DryRun = true
	var lines []string
	sum, err := Run(context.Background(), o, nil, func(s string) { lines = append(lines, s) })
	if err != nil || sum.Copied != 3 {
		t.Fatalf("%v %+v", err, sum.Progress)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatal("dry run created destination")
	}
	if len(lines) < 3 {
		t.Fatalf("expected log lines, got %v", lines)
	}
}

func TestImportMoveAndMtimeCapture(t *testing.T) {
	src, dst := setup(t)
	o := baseOpts(src, dst)
	o.Move, o.MtimeCapture, o.NameTemplate = true, true, "{YYYY}{MM}{DD}_{name}"
	sum, err := Run(context.Background(), o, nil, nil)
	if err != nil || sum.Copied != 3 {
		t.Fatalf("%v %+v %v", err, sum.Progress, sum.Errors)
	}
	fi := exists(t, filepath.Join(dst, "2020/01/02/20200102_IMG_20200102_030405.png"))
	if want := time.Date(2020, 1, 2, 3, 4, 5, 0, time.Local); !fi.ModTime().Equal(want) {
		t.Errorf("mtime %v want capture time %v", fi.ModTime(), want)
	}
	if _, err := os.Stat(filepath.Join(src, "IMG_20200102_030405.png")); !os.IsNotExist(err) {
		t.Error("move left source behind")
	}
	exists(t, filepath.Join(src, "readme.txt")) // non-media untouched
}

func TestImportHidden(t *testing.T) {
	src, dst := setup(t)
	o := baseOpts(src, dst)
	o.IncludeHidden = true
	sum, err := Run(context.Background(), o, nil, nil)
	if err != nil || sum.Copied != 4 {
		t.Fatalf("%v %+v", err, sum.Progress)
	}
}

func TestDestInsideSourceIsSkipped(t *testing.T) {
	src, _ := setup(t)
	dst := filepath.Join(src, "sorted")
	if _, err := Run(context.Background(), baseOpts(src, dst), nil, nil); err != nil {
		t.Fatal(err)
	}
	sum, err := Run(context.Background(), baseOpts(src, dst), nil, nil)
	if err != nil || sum.Total != 3 || sum.Skipped != 3 {
		t.Fatalf("%v %+v", err, sum.Progress)
	}
}

func TestValidation(t *testing.T) {
	src, dst := setup(t)
	o := baseOpts(src, dst)
	o.Kinds = map[media.Kind]bool{}
	if _, err := Run(context.Background(), o, nil, nil); err == nil {
		t.Error("expected error for no kinds")
	}
	if _, err := Run(context.Background(), baseOpts(filepath.Join(src, "nope"), dst), nil, nil); err == nil {
		t.Error("expected error for missing source")
	}
	if _, err := Run(context.Background(), baseOpts(src, src), nil, nil); err == nil {
		t.Error("expected error for src == dst")
	}
}

func TestCancel(t *testing.T) {
	src, dst := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Run(ctx, baseOpts(src, dst), nil, nil); err == nil {
		t.Error("expected context error")
	}
}
