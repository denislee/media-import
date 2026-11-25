package ui

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gioui.org/app"

	"github.com/denislee/media-import/internal/importer"
	"github.com/denislee/media-import/internal/logbuf"
)

// TestInstanceLockSecondFails: a second window must not share the resume
// journal and settings of the first one.
func TestInstanceLockSecondFails(t *testing.T) {
	dir := t.TempDir()
	f, err := instanceLock(dir)
	if err != nil {
		t.Fatalf("first instanceLock: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	if _, err := instanceLock(dir); !errors.Is(err, importer.ErrLocked) {
		t.Fatalf("second instanceLock: %v, want importer.ErrLocked", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// TestTornTmpDoesNotAffectLoad: a half-written settings.json.tmp or
// resume.json.tmp left by a crash is ignored; the previous good file loads.
func TestTornTmpDoesNotAffectLoad(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "cfg"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "state"))

	s := defaultSettings()
	s.Dest = "/photos/x"
	if err := saveSettings(s); err != nil {
		t.Fatalf("saveSettings: %v", err)
	}
	if _, err := os.Stat(configPath() + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("settings temp file left behind: %v", err)
	}
	if err := os.WriteFile(configPath()+".tmp", []byte("{garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := loadSettings().Dest; got != "/photos/x" {
		t.Errorf("loadSettings().Dest = %q, want %q", got, "/photos/x")
	}

	if err := saveResumeMeta(resumeMeta{Source: "/card", Dest: "/photos/x"}); err != nil {
		t.Fatalf("saveResumeMeta: %v", err)
	}
	if _, err := os.Stat(resumePath() + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("resume temp file left behind: %v", err)
	}
	if err := os.WriteFile(journalPath(), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(resumePath()+".tmp", []byte("{garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, rs := loadResume()
	if m == nil || m.Source != "/card" || rs == nil {
		t.Fatalf("loadResume() = %+v, %v; want the saved meta and its state", m, rs)
	}
}

// TestDiscardResumeRemovesPart: discarding an interrupted import removes the
// temp files its in-flight copies left, but nothing else.
func TestDiscardResumeRemovesPart(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "state"))

	if err := saveResumeMeta(resumeMeta{Source: "/card"}); err != nil {
		t.Fatalf("saveResumeMeta: %v", err)
	}
	if err := os.WriteFile(journalPath(), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(tmp, "dest")
	final := filepath.Join(dest, "a.jpg")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(importer.PartPath(final), []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(dest, "b.jpg")
	if err := os.WriteFile(keep, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}

	discardResume(&importer.ResumeState{Pending: map[string]string{"/card/a.jpg": final}})
	if _, err := os.Stat(importer.PartPath(final)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("temp file survived: %v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("an unrelated file was removed: %v", err)
	}
	if _, err := os.Stat(resumePath()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("resume.json survived: %v", err)
	}
	if _, err := os.Stat(journalPath()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("journal survived: %v", err)
	}
	discardResume(nil)
}

// tick is a progress snapshot whose percentage stays 0 while its byte count
// and elapsed time move.
func tick(i int) importer.Progress {
	p := snap(int64(i+1)*(1<<20)+int64(i)*1000, false, 3*time.Second+time.Duration(i)*100*time.Millisecond)
	p.TotalBytes = 1 << 40
	return p
}

// count returns how many of 20 ticks asked for a redraw on the given tab.
func count(t *testing.T, tab string, logEachTick bool) int {
	t.Helper()
	s := &state{win: new(app.Window), logs: logbuf.New(100), shownTab: tab}
	s.prog = tick(0)
	s.titlePc = 0
	now := time.Now()
	n := 0
	for i := 1; i <= 20; i++ {
		if logEachTick {
			s.appendLog("line")
		}
		if _, redraw := s.onProgress(tick(i), now.Add(time.Duration(i)*100*time.Millisecond)); redraw {
			n++
		}
	}
	return n
}

// TestBackgroundTabRedraws: away from the progress page, a tick redraws only
// when something that page shows changed.
func TestBackgroundTabRedraws(t *testing.T) {
	for _, c := range []struct {
		name    string
		tab     string
		logEach bool
		want    int
	}{
		{"setup ignores byte progress", tabSetup, false, 0},
		{"log ignores byte progress", tabLog, false, 0},
		{"log redraws for new lines", tabLog, true, 20},
		{"progress redraws every tick", tabProgress, false, 20},
		{"no tab known before the first frame", "", false, 20},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := count(t, c.tab, c.logEach); got != c.want {
				t.Errorf("%d redraws on tab %q (log each tick %v), want %d", got, c.tab, c.logEach, c.want)
			}
		})
	}

	t.Run("setup redraws for a pause", func(t *testing.T) {
		s := &state{win: new(app.Window), logs: logbuf.New(100), shownTab: tabSetup}
		s.prog = tick(1)
		s.titlePc = 0
		p := tick(1)
		p.Paused = true
		if _, redraw := s.onProgress(p, time.Now().Add(100*time.Millisecond)); !redraw {
			t.Error("setup tab did not redraw when the status line changed to paused")
		}
	})
}
