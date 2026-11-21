// Package ui implements the Gio (native Wayland) user interface.
package ui

import (
	"context"
	"errors"
	"fmt"
	"image/color"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"gioui.org/app"
	"gioui.org/font/gofont"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/paint"
	"gioui.org/text"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"github.com/denislee/media-import/internal/devices"
	"github.com/denislee/media-import/internal/importer"
	"github.com/denislee/media-import/internal/logbuf"
	"github.com/denislee/media-import/internal/media"
	"github.com/denislee/media-import/internal/organize"
)

const customKey = "custom"

const maxLogLines = 5000

// speedWindow is how far back the "current speed" figure looks.
const speedWindow = 5 * time.Second

type speedSample struct {
	t time.Time
	b int64
}

// Version is set by main from the build's ldflags.
var Version = "dev"

type state struct {
	win *app.Window
	th  *material.Theme

	srcEd, dstEd, customDirEd, customNameEd widget.Editor
	srcBrowse, dstBrowse                    widget.Clickable
	startBtn, cancelBtn, clearLogBtn        widget.Clickable
	pauseBtn, resumeBtn, discardBtn         widget.Clickable
	dirEnum, nameEnum, dupEnum              widget.Enum
	dupAnyName                              widget.Bool
	kinds                                   map[media.Kind]*widget.Bool
	move, skipDups, verify, ffprobe         widget.Bool
	mtimeCap, dryRun, hidden                widget.Bool
	page, logList, devList, progList        widget.List
	tabs                                    widget.Enum
	otherBtn                                widget.Clickable
	pal                                     palette

	volMu     sync.Mutex
	vols      []devices.Volume
	volBtns   []widget.Clickable
	ejectBtns []widget.Clickable
	unmounted []devices.Unmounted
	mountBtns []widget.Clickable
	unm       devices.UnmountedCache // runs lsblk only when block devices or mounts changed

	// Mount/eject state (guarded by mu).
	opBusy      bool
	authOp      *privOp // waiting for a sudo password
	opErr       string
	mountedPath string // set by the mount goroutine, applied to srcEd on the UI goroutine
	pwEd        widget.Editor
	pwFocus     bool // focus pwEd on the next frame the prompt is shown
	pwOK        widget.Clickable
	pwCancel    widget.Clickable

	mu      sync.Mutex
	running bool
	cancel  context.CancelFunc
	prog    importer.Progress
	speed   float64 // recent transfer rate in bytes/s (rolling window)
	samples []speedSample
	runDest string
	titlePc int
	gate    *importer.Gate
	runDry  bool

	// Interrupted import that can be resumed (guarded by mu).
	resMeta  *resumeMeta
	resState *importer.ResumeState
	status   string
	logs     *logbuf.Buffer

	browser *browser
}

// Run opens the main window and blocks until it is closed.
func Run() {
	go func() {
		w := new(app.Window)
		w.Option(app.Title("Media Import"), app.Size(unit.Dp(1120), unit.Dp(820)), app.MinSize(unit.Dp(640), unit.Dp(480)))
		s := newState(w)
		if err := s.loop(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}()
	app.Main()
}

func newState(w *app.Window) *state {
	th := material.NewTheme()
	th.Shaper = text.NewShaper(text.WithCollection(gofont.Collection()))
	th.TextSize = 15
	pal := darkPalette()
	if os.Getenv("MEDIA_IMPORT_THEME") == "light" {
		pal = lightPalette()
	}
	th.Palette = material.Palette{Bg: pal.bg, Fg: pal.fg, ContrastBg: pal.accent, ContrastFg: pal.accentFg}

	s := &state{win: w, th: th, pal: pal, kinds: map[media.Kind]*widget.Bool{}}
	s.logs = logbuf.New(maxLogLines)
	s.tabs.Value = tabSetup
	s.devList.Axis, s.progList.Axis = layout.Vertical, layout.Vertical
	for _, e := range []*widget.Editor{&s.srcEd, &s.dstEd, &s.customDirEd, &s.customNameEd} {
		e.SingleLine = true
	}
	s.pwEd.SingleLine, s.pwEd.Submit, s.pwEd.Mask = true, true, '•'
	s.page.Axis = layout.Vertical
	s.logList.Axis = layout.Vertical
	s.logList.ScrollToEnd = true

	cfg := loadSettings()
	s.srcEd.SetText(cfg.Source)
	s.dstEd.SetText(cfg.Dest)
	s.customDirEd.SetText(cfg.CustomDir)
	s.customNameEd.SetText(cfg.CustomName)
	s.dirEnum.Value = cfg.DirPreset
	s.nameEnum.Value = cfg.NamePreset
	for _, k := range media.AllKinds {
		s.kinds[k] = &widget.Bool{Value: cfg.Kinds[k.String()]}
	}
	s.move.Value, s.skipDups.Value, s.verify.Value = cfg.Move, cfg.SkipDups, cfg.Verify
	s.dupEnum.Value, s.dupAnyName.Value = cfg.DupCheck, cfg.DupAnyName
	if s.dupEnum.Value != importer.DupCheckFull {
		s.dupEnum.Value = importer.DupCheckFast
	}
	s.ffprobe.Value, s.mtimeCap.Value = cfg.FFprobe && media.FFprobeAvailable(), cfg.MtimeCapture
	s.dryRun.Value, s.hidden.Value = cfg.DryRun, cfg.Hidden
	s.status = "Ready. Version " + Version
	s.resMeta, s.resState = loadResume()

	s.refreshVolumes()
	// Volumes are refreshed on kernel mount/block events (no idle lsblk forks);
	// the 10 s fallback covers gvfs mounts, which raise neither.
	go devices.Watch(context.Background(), 10*time.Second, func(block bool) {
		if block {
			s.unm.Invalidate()
		}
		if s.refreshVolumes() {
			w.Invalidate()
		}
	})
	return s
}

func (s *state) loop() error {
	var ops op.Ops
	for {
		switch e := s.win.Event().(type) {
		case app.DestroyEvent:
			s.mu.Lock()
			if s.cancel != nil {
				s.cancel()
			}
			s.mu.Unlock()
			return e.Err
		case app.FrameEvent:
			gtx := app.NewContext(&ops, e)
			paint.Fill(gtx.Ops, s.th.Bg)
			if s.browser != nil {
				s.browser.layout(gtx, s)
			} else {
				s.layout(gtx)
			}
			e.Frame(gtx.Ops)
		}
	}
}

// refreshVolumes reloads mounted volumes; reports whether the list changed.
func (s *state) refreshVolumes() bool {
	vols := devices.List()
	unm := s.unm.Get()
	s.volMu.Lock()
	defer s.volMu.Unlock()
	if slices.Equal(vols, s.vols) && slices.Equal(unm, s.unmounted) {
		return false
	}
	s.vols, s.unmounted = vols, unm
	if len(s.volBtns) < len(vols) {
		s.volBtns = append(s.volBtns, make([]widget.Clickable, len(vols)-len(s.volBtns))...)
		s.ejectBtns = append(s.ejectBtns, make([]widget.Clickable, len(vols)-len(s.ejectBtns))...)
	}
	if len(s.mountBtns) < len(unm) {
		s.mountBtns = append(s.mountBtns, make([]widget.Clickable, len(unm)-len(s.mountBtns))...)
	}
	return true
}

// privOp is a mount or eject that may need root.
type privOp struct {
	verb string // "Mount" / "Eject"
	what string // shown in the password prompt
	run  func(password string) (string, error)
	done func(result string) // called after success, off the UI goroutine
}

func mountOp(s *state, u devices.Unmounted) privOp {
	return privOp{
		verb: "Mount", what: fmt.Sprintf("%s (%s)", u.Name(), u.Dev),
		run: func(pw string) (string, error) { return devices.Mount(u, pw) },
		done: func(mp string) {
			s.mu.Lock()
			s.mountedPath = mp
			s.mu.Unlock()
			s.addLog(fmt.Sprintf("Mounted %s at %s", u.Dev, mp))
		},
	}
}

func ejectOp(s *state, v devices.Volume) privOp {
	return privOp{
		verb: "Eject", what: fmt.Sprintf("%s (%s)", v.Label, v.Path),
		run: func(pw string) (string, error) { return "", devices.Eject(v, pw) },
		done: func(string) {
			s.addLog(fmt.Sprintf("Ejected %s — safe to unplug", v.Label))
			notify(v.Label + " ejected — safe to unplug")
		},
	}
}

// runOp runs op in the background. An empty password tries udisks/gio and
// password-less sudo; if sudo needs a password the user is prompted.
func (s *state) runOp(op privOp, password string) {
	s.mu.Lock()
	if s.opBusy {
		s.mu.Unlock()
		return
	}
	s.opBusy, s.opErr = true, ""
	s.mu.Unlock()
	s.win.Invalidate()

	go func() {
		res, err := op.run(password)
		s.mu.Lock()
		s.opBusy = false
		switch {
		case err == nil:
			s.authOp = nil
		case errors.Is(err, devices.ErrPasswordRequired):
			s.authOp, s.pwFocus = &op, true
		case errors.Is(err, devices.ErrBadPassword):
			s.authOp, s.opErr, s.pwFocus = &op, "Incorrect password, try again.", true
		default:
			s.authOp, s.opErr = nil, err.Error()
		}
		s.mu.Unlock()
		if err == nil {
			op.done(res)
			s.unm.Invalidate()
			s.refreshVolumes()
		} else if s.authOp == nil {
			s.addLog(op.verb + " failed: " + err.Error())
		}
		s.win.Invalidate()
	}()
}

// submitPassword sends the typed password to sudo and wipes the editor.
func (s *state) submitPassword() {
	s.mu.Lock()
	op := s.authOp
	s.mu.Unlock()
	pw := s.pwEd.Text()
	s.pwEd.SetText("")
	if op == nil || pw == "" {
		return
	}
	s.runOp(*op, pw)
}

func (s *state) settings() settings {
	k := map[string]bool{}
	for kind, b := range s.kinds {
		k[kind.String()] = b.Value
	}
	return settings{
		Source: strings.TrimSpace(s.srcEd.Text()), Dest: strings.TrimSpace(s.dstEd.Text()),
		DirPreset: s.dirEnum.Value, CustomDir: s.customDirEd.Text(),
		NamePreset: s.nameEnum.Value, CustomName: s.customNameEd.Text(),
		Kinds: k, Move: s.move.Value, SkipDups: s.skipDups.Value, Verify: s.verify.Value,
		DupCheck: s.dupEnum.Value, DupAnyName: s.dupAnyName.Value,
		FFprobe: s.ffprobe.Value, MtimeCapture: s.mtimeCap.Value, DryRun: s.dryRun.Value, Hidden: s.hidden.Value,
	}
}

func (s *state) dirTemplate() string {
	if s.dirEnum.Value == customKey {
		return s.customDirEd.Text()
	}
	if i, err := strconv.Atoi(s.dirEnum.Value); err == nil && i >= 0 && i < len(organize.DirPresets) {
		return organize.DirPresets[i].Template
	}
	return organize.DirPresets[0].Template
}

func (s *state) nameTemplate() string {
	if s.nameEnum.Value == customKey {
		return s.customNameEd.Text()
	}
	if i, err := strconv.Atoi(s.nameEnum.Value); err == nil && i >= 0 && i < len(organize.NamePresets) {
		return organize.NamePresets[i].Template
	}
	return organize.NamePresets[0].Template
}

func (s *state) addLog(line string) {
	s.appendLog(line)
	s.win.Invalidate()
}

// appendLog records line without asking for a redraw. It is used by the
// importer's log callback, whose lines are drawn by the 100 ms progress tick's
// invalidation rather than one redraw per line.
func (s *state) appendLog(line string) {
	s.mu.Lock()
	s.logs.Append(time.Now().Format("15:04:05 ") + line)
	s.mu.Unlock()
}

// togglePause pauses or resumes the running import. It must not hold s.mu
// while logging: addLog takes s.mu itself.
func (s *state) togglePause() {
	s.mu.Lock()
	g := s.gate
	s.mu.Unlock()
	if g == nil {
		return
	}
	if g.Paused() {
		g.Resume()
		s.addLog("Resumed")
	} else {
		g.Pause()
		s.addLog("Paused")
	}
}

func (s *state) start() {
	cfg := s.settings()
	if err := saveSettings(cfg); err != nil {
		s.addLog("WARN could not save settings: " + err.Error())
	}
	s.launch(cfg, s.dirTemplate(), s.nameTemplate(), nil)
}

// resume continues the interrupted import recorded in the journal.
func (s *state) resume() {
	s.mu.Lock()
	m, rs := s.resMeta, s.resState
	s.mu.Unlock()
	if m == nil || rs == nil {
		return
	}
	src := m.Source
	if st, err := os.Stat(src); err != nil || !st.IsDir() {
		// The card may have been mounted somewhere else this time; accept the
		// folder currently in the Source field instead.
		cur, _ := filepath.Abs(expandHome(strings.TrimSpace(s.srcEd.Text())))
		if st, err := os.Stat(cur); err != nil || !st.IsDir() || cur == m.Source {
			s.addLog(fmt.Sprintf("ERROR cannot resume: source %s is not available. Connect and mount the media (or choose its new location under Source) and press Resume again.", m.Source))
			return
		}
		src = cur
	}
	for _, old := range append([]string{m.Source}, m.OldSources...) {
		rs.Rebase(old, src)
	}
	if src != m.Source && !slices.Contains(m.OldSources, src) {
		m.OldSources = append(m.OldSources, m.Source)
		m.Source = src
		_ = saveResumeMeta(*m)
	}
	cfg := m.Settings
	cfg.Source, cfg.DryRun = src, false
	s.srcEd.SetText(src)
	s.dstEd.SetText(cfg.Dest)
	s.launch(cfg, m.DirTemplate, m.NameTemplate, rs)
}

// launch starts an import (or resumes one when rs is non-nil).
func (s *state) launch(cfg settings, dirT, nameT string, rs *importer.ResumeState) {
	opt := importer.Options{
		Sources:       []string{expandHome(cfg.Source)},
		Dest:          expandHome(cfg.Dest),
		DirTemplate:   dirT,
		NameTemplate:  nameT,
		Kinds:         map[media.Kind]bool{},
		Move:          cfg.Move,
		DryRun:        cfg.DryRun,
		SkipDups:      cfg.SkipDups,
		DupCheck:      cfg.DupCheck,
		DupAnyName:    cfg.DupAnyName,
		Verify:        cfg.Verify,
		UseFFprobe:    cfg.FFprobe,
		MtimeCapture:  cfg.MtimeCapture,
		IncludeHidden: cfg.Hidden,
	}
	for _, k := range media.AllKinds {
		opt.Kinds[k] = cfg.Kinds[k.String()]
	}
	gate := &importer.Gate{}
	opt.Pause = gate
	if !opt.DryRun {
		opt.Journal, opt.Resume = journalPath(), rs
		if rs == nil {
			s.mu.Lock()
			hadResume := s.resMeta != nil
			s.mu.Unlock()
			if hadResume {
				s.addLog("Discarded the interrupted import (a new one was started)")
			}
			clearResume()
			abs, _ := filepath.Abs(opt.Sources[0])
			err := saveResumeMeta(resumeMeta{Settings: cfg, Source: abs, Dest: opt.Dest,
				DirTemplate: dirT, NameTemplate: nameT, Started: time.Now()})
			if err != nil {
				s.addLog("WARN cannot save resume information: " + err.Error())
			}
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.resMeta, s.resState = nil, nil
	s.gate, s.runDry = gate, opt.DryRun
	s.running, s.cancel = true, cancel
	s.prog = importer.Progress{Phase: "scanning"}
	s.speed, s.samples, s.runDest, s.titlePc = 0, nil, opt.Dest, -1
	s.status = "Scanning…"
	s.mu.Unlock()
	mode := "Import"
	switch {
	case opt.DryRun:
		mode = "Dry run"
	case rs != nil:
		mode = fmt.Sprintf("Resumed import (%d files already done)", rs.DoneCount())
	}
	s.addLog(fmt.Sprintf("%s started: %s -> %s (folders %q, names %q)", mode, opt.Sources[0], opt.Dest, opt.DirTemplate, opt.NameTemplate))

	go func() {
		sum, err := importer.Run(ctx, opt, func(p importer.Progress) {
			now := time.Now()
			s.mu.Lock()
			s.prog = p
			if p.Phase == "importing" {
				s.samples = append(s.samples, speedSample{now, p.Bytes})
				for len(s.samples) > 2 && now.Sub(s.samples[0].t) > speedWindow {
					s.samples = s.samples[1:]
				}
				if first := s.samples[0]; len(s.samples) > 1 && now.Sub(first.t) > 500*time.Millisecond {
					s.speed = float64(p.Bytes-first.b) / now.Sub(first.t).Seconds()
				}
			}
			title := ""
			if pc := percent(p); p.Phase == "importing" && pc != s.titlePc {
				s.titlePc = pc
				title = fmt.Sprintf("%d%% · Media Import", pc)
			}
			s.mu.Unlock()
			if title != "" {
				s.win.Option(app.Title(title))
			}
			s.win.Invalidate()
		}, s.appendLog)
		msg := fmt.Sprintf("%s finished: %d imported, %d skipped, %d failed, %s in %s",
			mode, sum.Copied, sum.Skipped, sum.Failed, humanBytes(sum.Bytes), sum.Elapsed.Round(time.Second))
		switch {
		case ctx.Err() != nil:
			msg = fmt.Sprintf("Cancelled after %d of %d files", sum.Done, sum.Total)
		case err != nil:
			msg = "Error: " + err.Error()
		}
		if line := dupSummary(sum.Progress); line != "" {
			s.addLog(line)
		}
		if sum.Verified > 0 {
			s.addLog(fmt.Sprintf("Integrity: %d copies re-read from disk and matched their source checksum", sum.Verified))
		}
		s.addLog(msg)
		var m *resumeMeta
		var left *importer.ResumeState
		if !opt.DryRun {
			if err == nil && ctx.Err() == nil && sum.Failed == 0 {
				clearResume()
			} else {
				// Keep the journal so the remaining files can be resumed.
				m, left = loadResume()
			}
		}
		s.mu.Lock()
		s.running, s.cancel, s.status, s.gate = false, nil, msg, nil
		s.resMeta, s.resState = m, left
		s.mu.Unlock()
		s.win.Option(app.Title("Media Import"))
		cancel()
		notify(msg)
		s.win.Invalidate()
	}()
}

// dupSummary describes how the duplicates of a finished run were recognised.
func dupSummary(p importer.Progress) string {
	var parts []string
	add := func(n int, what string) {
		if n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, what))
		}
	}
	add(p.DupIndexed, "from the destination index")
	add(p.DupQuick, "by quick check")
	add(p.DupFull, "by full compare")
	add(p.DupOtherName, "under another name")
	if len(parts) == 0 {
		return ""
	}
	return fmt.Sprintf("Duplicates: %s; %s read in %s", strings.Join(parts, ", "),
		humanBytes(p.DupCheckBytes), p.DupCheckTime.Round(10*time.Millisecond))
}

// activitySummary says what the workers are doing, e.g.
// "copying 2 · checking if already imported 2".
func activitySummary(active []importer.FileProgress) string {
	var order []string
	n := map[string]int{}
	for _, fp := range active {
		if n[fp.Stage] == 0 {
			order = append(order, fp.Stage)
		}
		n[fp.Stage]++
	}
	parts := make([]string, len(order))
	for i, st := range order {
		parts[i] = fmt.Sprintf("%s %d", st, n[st])
	}
	return strings.Join(parts, " · ")
}

// isDryRunning reports whether the current run is a dry run (nothing to pause).
func (s *state) isDryRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running && s.runDry
}

// percent is the overall byte-based completion, 0–100.
func percent(p importer.Progress) int {
	if p.Phase == "done" && p.Total > 0 && p.Done == p.Total {
		return 100
	}
	if p.TotalBytes <= 0 {
		if p.Total > 0 {
			return p.Done * 100 / p.Total
		}
		return 0
	}
	return int(min(100, p.Bytes*100/p.TotalBytes))
}
func fmtDur(d time.Duration) string {
	d = d.Round(time.Second)
	h, m, sec := int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh %02dm", h, m)
	case m > 0:
		return fmt.Sprintf("%dm %02ds", m, sec)
	default:
		return fmt.Sprintf("%ds", sec)
	}
}
func sampleFields() organize.Fields {
	return organize.Fields{
		Time: time.Date(2024, 7, 21, 18, 30, 5, 0, time.Local),
		Kind: media.Image.String(), Name: "IMG_1234", Ext: "jpg", Source: "CARD",
	}
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func notify(msg string) {
	if p, err := exec.LookPath("notify-send"); err == nil {
		go func() { _ = exec.Command(p, "-a", "Media Import", "Media Import", msg).Run() }()
	}
}

func rgb(c uint32) color.NRGBA {
	return color.NRGBA{R: uint8(c >> 16), G: uint8(c >> 8), B: uint8(c), A: 0xff}
}

func withAlpha(c color.NRGBA, a uint8) color.NRGBA {
	c.A = a
	return c
}
