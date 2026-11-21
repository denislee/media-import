package ui

import (
	"strings"
	"testing"
	"time"

	"gioui.org/app"

	"github.com/denislee/media-import/internal/importer"
	"github.com/denislee/media-import/internal/logbuf"
)

// TestTogglePauseDoesNotDeadlock checks that toggling the pause button logs
// without holding s.mu: addLog takes s.mu itself, so holding it would deadlock.
func TestTogglePauseDoesNotDeadlock(t *testing.T) {
	s := &state{win: new(app.Window), logs: logbuf.New(100), gate: &importer.Gate{}}

	toggle := func() {
		done := make(chan struct{})
		go func() {
			s.togglePause()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("togglePause deadlocked")
		}
	}

	toggle()
	if !s.gate.Paused() {
		t.Fatal("gate not paused after first toggle")
	}
	toggle()
	if s.gate.Paused() {
		t.Fatal("gate still paused after second toggle")
	}

	lines := s.logs.Lines()
	if len(lines) != 2 {
		t.Fatalf("log lines = %v, want 2", lines)
	}
	if !strings.HasSuffix(lines[0], "Paused") {
		t.Errorf("first line = %q, want it to end in %q", lines[0], "Paused")
	}
	if !strings.HasSuffix(lines[1], "Resumed") {
		t.Errorf("second line = %q, want it to end in %q", lines[1], "Resumed")
	}
}
