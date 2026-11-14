// Package ui implements the Gio (native Wayland) user interface.
package ui

import (
	"context"
	"errors"
	"fmt"
	"image"
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
	"gioui.org/font"
	"gioui.org/font/gofont"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/text"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"github.com/denislee/media-import/internal/devices"
	"github.com/denislee/media-import/internal/importer"
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
	dirEnum, nameEnum                       widget.Enum
	kinds                                   map[media.Kind]*widget.Bool
	move, skipDups, verify, ffprobe         widget.Bool
	mtimeCap, dryRun, hidden                widget.Bool
	page, logList                           widget.List

	volMu     sync.Mutex
	vols      []devices.Volume
	volBtns   []widget.Clickable
	ejectBtns []widget.Clickable
	unmounted []devices.Unmounted
	mountBtns []widget.Clickable

	// Mount/eject state (guarded by mu).
	opBusy      bool
	authOp      *privOp // waiting for a sudo password
	opErr       string
	mountedPath string // set by the mount goroutine, applied to srcEd on the UI goroutine
	pwEd        widget.Editor
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
	logs     []string

	browser *browser
}

// Run opens the main window and blocks until it is closed.
func Run() {
	go func() {
		w := new(app.Window)
		w.Option(app.Title("Media Import"), app.Size(unit.Dp(980), unit.Dp(860)), app.MinSize(unit.Dp(640), unit.Dp(480)))
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
	if os.Getenv("MEDIA_IMPORT_THEME") != "light" {
		th.Palette = material.Palette{
			Bg:         rgb(0x1c1d21),
			Fg:         rgb(0xe4e5e9),
			ContrastBg: rgb(0x5b8def),
			ContrastFg: rgb(0xffffff),
		}
	}

	s := &state{win: w, th: th, kinds: map[media.Kind]*widget.Bool{}}
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
	s.ffprobe.Value, s.mtimeCap.Value = cfg.FFprobe && media.FFprobeAvailable(), cfg.MtimeCapture
	s.dryRun.Value, s.hidden.Value = cfg.DryRun, cfg.Hidden
	s.status = "Ready. Version " + Version
	s.resMeta, s.resState = loadResume()

	s.refreshVolumes()
	go func() {
		for range time.Tick(2 * time.Second) {
			if s.refreshVolumes() {
				w.Invalidate()
			}
		}
	}()
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
	unm := devices.ListUnmounted()
	s.volMu.Lock()
	defer s.volMu.Unlock()
	if fmt.Sprint(vols) == fmt.Sprint(s.vols) && fmt.Sprint(unm) == fmt.Sprint(s.unmounted) {
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
			s.authOp = &op
		case errors.Is(err, devices.ErrBadPassword):
			s.authOp, s.opErr = &op, "Incorrect password, try again."
		default:
			s.authOp, s.opErr = nil, err.Error()
		}
		s.mu.Unlock()
		if err == nil {
			op.done(res)
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
	s.mu.Lock()
	s.logs = append(s.logs, time.Now().Format("15:04:05 ")+line)
	if len(s.logs) > maxLogLines {
		s.logs = append([]string(nil), s.logs[len(s.logs)-maxLogLines:]...)
	}
	s.mu.Unlock()
	s.win.Invalidate()
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
		}, s.addLog)
		msg := fmt.Sprintf("%s finished: %d imported, %d skipped, %d failed, %s in %s",
			mode, sum.Copied, sum.Skipped, sum.Failed, humanBytes(sum.Bytes), sum.Elapsed.Round(time.Second))
		switch {
		case ctx.Err() != nil:
			msg = fmt.Sprintf("Cancelled after %d of %d files", sum.Done, sum.Total)
		case err != nil:
			msg = "Error: " + err.Error()
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

// ---------- layout ----------

func (s *state) layout(gtx layout.Context) layout.Dimensions {
	s.mu.Lock()
	running := s.running
	s.mu.Unlock()

	if s.startBtn.Clicked(gtx) && !running {
		s.start()
		running = true
	}
	if s.cancelBtn.Clicked(gtx) {
		s.mu.Lock()
		if s.cancel != nil {
			s.cancel()
			s.status = "Cancelling…"
		}
		s.mu.Unlock()
	}
	if s.pauseBtn.Clicked(gtx) {
		s.mu.Lock()
		if g := s.gate; g != nil {
			if g.Paused() {
				g.Resume()
				s.addLog("Resumed")
			} else {
				g.Pause()
				s.addLog("Paused")
			}
		}
		s.mu.Unlock()
	}
	if s.resumeBtn.Clicked(gtx) && !running {
		s.resume()
		s.mu.Lock()
		running = s.running
		s.mu.Unlock()
	}
	if s.discardBtn.Clicked(gtx) && !running {
		clearResume()
		s.mu.Lock()
		s.resMeta, s.resState = nil, nil
		s.mu.Unlock()
		s.addLog("Discarded the interrupted import")
	}
	if s.clearLogBtn.Clicked(gtx) {
		s.mu.Lock()
		s.logs = nil
		s.mu.Unlock()
	}
	if s.srcBrowse.Clicked(gtx) {
		s.browser = newBrowser(&s.srcEd, "Choose source folder", false)
	}
	if s.dstBrowse.Clicked(gtx) {
		s.browser = newBrowser(&s.dstEd, "Choose destination folder", true)
	}
	s.volMu.Lock()
	var req *privOp
	for i := range s.vols {
		if s.volBtns[i].Clicked(gtx) {
			s.srcEd.SetText(s.vols[i].Path)
		}
		if s.ejectBtns[i].Clicked(gtx) {
			op := ejectOp(s, s.vols[i])
			req = &op
		}
	}
	for i := range s.unmounted {
		if s.mountBtns[i].Clicked(gtx) {
			op := mountOp(s, s.unmounted[i])
			req = &op
		}
	}
	s.volMu.Unlock()
	if req != nil {
		s.pwEd.SetText("")
		s.runOp(*req, "")
	}
	for {
		ev, ok := s.pwEd.Update(gtx)
		if !ok {
			break
		}
		if _, ok := ev.(widget.SubmitEvent); ok {
			s.submitPassword()
		}
	}
	if s.pwOK.Clicked(gtx) {
		s.submitPassword()
	}
	if s.pwCancel.Clicked(gtx) {
		s.pwEd.SetText("")
		s.mu.Lock()
		s.authOp, s.opErr = nil, ""
		s.mu.Unlock()
	}
	s.mu.Lock()
	if s.mountedPath != "" {
		s.srcEd.SetText(s.mountedPath)
		s.mountedPath = ""
	}
	s.mu.Unlock()

	th := s.th
	cfgGtx := gtx
	if running {
		cfgGtx = gtx.Disabled()
	}

	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			if running {
				gtx = cfgGtx
			}
			sections := []layout.Widget{
				s.header,
				s.resumeBanner,
				func(gtx layout.Context) layout.Dimensions { return s.card(gtx, "Source", s.sourceSection) },
				func(gtx layout.Context) layout.Dimensions { return s.card(gtx, "Destination", s.destSection) },
				func(gtx layout.Context) layout.Dimensions { return s.card(gtx, "Media types", s.kindsSection) },
				func(gtx layout.Context) layout.Dimensions { return s.card(gtx, "Folder structure", s.dirSection) },
				func(gtx layout.Context) layout.Dimensions { return s.card(gtx, "File names", s.nameSection) },
				func(gtx layout.Context) layout.Dimensions { return s.card(gtx, "Options", s.optionsSection) },
			}
			return material.List(th, &s.page).Layout(gtx, len(sections), func(gtx layout.Context, i int) layout.Dimensions {
				return layout.Inset{Left: 16, Right: 16, Top: 6, Bottom: 6}.Layout(gtx, sections[i])
			})
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Left: 16, Right: 16, Top: 6, Bottom: 12}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return s.runPanel(gtx, running)
			})
		}),
	)
}

// resumeBanner offers to continue an import that did not finish.
func (s *state) resumeBanner(gtx layout.Context) layout.Dimensions {
	s.mu.Lock()
	m, rs, running := s.resMeta, s.resState, s.running
	s.mu.Unlock()
	if m == nil || rs == nil || running {
		return layout.Dimensions{}
	}
	done := fmt.Sprintf("%d files done", rs.DoneCount())
	if rs.Total > 0 {
		done = fmt.Sprintf("%d of %d files done", rs.DoneCount(), rs.Total)
	}
	msg := fmt.Sprintf("%s → %s · started %s · %s", m.Source, m.Dest, m.Started.Format("Jan 2 15:04"), done)
	return s.card(gtx, "The last import did not finish", func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(s.muted(msg)),
			layout.Rigid(spacer(0, 4)),
			layout.Rigid(s.muted("Resume skips files that were already imported and cleans up any half-written copy.")),
			layout.Rigid(spacer(0, 8)),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{}.Layout(gtx,
					layout.Rigid(material.Button(s.th, &s.resumeBtn, "Resume import").Layout),
					layout.Rigid(spacer(8, 0)),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						b := material.Button(s.th, &s.discardBtn, "Discard")
						b.Background = rgb(0x50545e)
						return b.Layout(gtx)
					}),
				)
			}),
		)
	})
}

func (s *state) header(gtx layout.Context) layout.Dimensions {
	return layout.Inset{Top: 8, Bottom: 4}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Alignment: layout.Baseline}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				l := material.H5(s.th, "Media Import")
				l.Font.Weight = font.Bold
				return l.Layout(gtx)
			}),
			layout.Rigid(spacer(12, 0)),
			layout.Rigid(s.muted("Copy photos, videos and audio from cameras, cards and phones into organised folders")),
		)
	})
}

func (s *state) sourceSection(gtx layout.Context) layout.Dimensions {
	s.volMu.Lock()
	vols := append([]devices.Volume(nil), s.vols...)
	unm := append([]devices.Unmounted(nil), s.unmounted...)
	s.volMu.Unlock()
	s.mu.Lock()
	mounting, target, mountErr := s.opBusy, s.authOp, s.opErr
	s.mu.Unlock()

	children := []layout.FlexChild{
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return s.pathRow(gtx, &s.srcEd, "/run/media/you/CARD/DCIM", &s.srcBrowse)
		}),
		layout.Rigid(spacer(0, 8)),
	}
	if mounting {
		children = append(children, layout.Rigid(s.muted("Working…")), layout.Rigid(spacer(0, 4)))
	}
	if len(vols) == 0 && len(unm) == 0 {
		children = append(children, layout.Rigid(s.muted("No removable media detected. Plug in a device (it will appear here automatically).")))
	} else if len(vols) > 0 {
		children = append(children, layout.Rigid(s.muted("Connected media — click to use as source:")), layout.Rigid(spacer(0, 4)))
		children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return s.wrap(gtx, len(vols), func(gtx layout.Context, i int) layout.Dimensions {
				style := func(b material.ButtonStyle) material.ButtonStyle {
					b.Background = rgb(0x2e3440)
					if s.th.Bg == rgb(0xffffff) {
						b.Background = rgb(0xdde3ee)
						b.Color = rgb(0x111111)
					}
					b.Inset = layout.UniformInset(8)
					return b
				}
				b := style(material.Button(s.th, &s.volBtns[i], fmt.Sprintf("%s  (%s)", vols[i].Label, vols[i].FS)))
				e := style(material.Button(s.th, &s.ejectBtns[i], "Eject"))
				e.Background = withAlpha(rgb(0xe06c75), 0x50)
				return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
					layout.Rigid(b.Layout),
					layout.Rigid(spacer(2, 0)),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						if mounting {
							gtx = gtx.Disabled()
						}
						return e.Layout(gtx)
					}),
				)
			})
		}))
	}
	if len(unm) > 0 {
		if len(vols) > 0 {
			children = append(children, layout.Rigid(spacer(0, 8)))
		}
		hint := "Connected but not mounted — click to mount:"
		children = append(children, layout.Rigid(s.muted(hint)), layout.Rigid(spacer(0, 4)))
		children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			if mounting {
				gtx = gtx.Disabled()
			}
			return s.wrap(gtx, len(unm), func(gtx layout.Context, i int) layout.Dimensions {
				u := unm[i]
				b := material.Button(s.th, &s.mountBtns[i], fmt.Sprintf("Mount %s  (%s, %s)", u.Name(), u.FS, humanBytes(u.Size)))
				b.Background = withAlpha(s.th.ContrastBg, 0x60)
				if s.th.Bg == rgb(0xffffff) {
					b.Color = rgb(0x111111)
				}
				b.Inset = layout.UniformInset(8)
				return b.Layout(gtx)
			})
		}))
	}
	if target != nil {
		children = append(children,
			layout.Rigid(spacer(0, 8)),
			layout.Rigid(s.muted(fmt.Sprintf("%s %s needs administrator rights. Enter your sudo password:", target.verb, target.what))),
			layout.Rigid(spacer(0, 4)),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				if mounting {
					gtx = gtx.Disabled()
				}
				return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
					layout.Flexed(1, func(gtx layout.Context) layout.Dimensions { return s.editor(gtx, &s.pwEd, "Password") }),
					layout.Rigid(spacer(8, 0)),
					layout.Rigid(material.Button(s.th, &s.pwOK, target.verb).Layout),
					layout.Rigid(spacer(8, 0)),
					layout.Rigid(material.Button(s.th, &s.pwCancel, "Cancel").Layout),
				)
			}),
		)
	}
	if mountErr != "" {
		children = append(children, layout.Rigid(spacer(0, 6)), layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			l := material.Body2(s.th, mountErr)
			l.Color = rgb(0xe06c75)
			return l.Layout(gtx)
		}))
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
}

func (s *state) destSection(gtx layout.Context) layout.Dimensions {
	return s.pathRow(gtx, &s.dstEd, "~/Pictures/Imported", &s.dstBrowse)
}

func (s *state) kindsSection(gtx layout.Context) layout.Dimensions {
	desc := map[media.Kind]string{
		media.Image: "Photos (jpg, png, heic, webp…)",
		media.Raw:   "RAW (cr2, cr3, nef, arw, dng…)",
		media.Video: "Videos (mp4, mov, mkv, mts…)",
		media.Audio: "Audio (mp3, wav, flac, m4a…)",
	}
	return s.wrap(gtx, len(media.AllKinds), func(gtx layout.Context, i int) layout.Dimensions {
		k := media.AllKinds[i]
		return layout.Inset{Right: 16}.Layout(gtx, material.CheckBox(s.th, s.kinds[k], desc[k]).Layout)
	})
}

func (s *state) dirSection(gtx layout.Context) layout.Dimensions {
	return s.templateSection(gtx, &s.dirEnum, organize.DirPresets, &s.customDirEd, "{YYYY}/{MM}/{DD}", func() string {
		return filepath.Join(organize.Dir(s.dirTemplate(), sampleFields()), "IMG_1234.jpg")
	})
}

func (s *state) nameSection(gtx layout.Context) layout.Dimensions {
	return s.templateSection(gtx, &s.nameEnum, organize.NamePresets, &s.customNameEd, "{YYYY}{MM}{DD}_{name}", func() string {
		return organize.FileName(s.nameTemplate(), sampleFields(), ".jpg")
	})
}

func (s *state) templateSection(gtx layout.Context, enum *widget.Enum, presets []organize.Preset, custom *widget.Editor, hint string, preview func() string) layout.Dimensions {
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return s.wrap(gtx, len(presets)+1, func(gtx layout.Context, i int) layout.Dimensions {
				key, label := customKey, "Custom…"
				if i < len(presets) {
					key, label = strconv.Itoa(i), presets[i].Name
				}
				return layout.Inset{Right: 12}.Layout(gtx, material.RadioButton(s.th, enum, key, label).Layout)
			})
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			if enum.Value != customKey {
				return layout.Dimensions{}
			}
			return layout.Inset{Top: 6}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
					layout.Rigid(func(gtx layout.Context) layout.Dimensions { return s.editor(gtx, custom, hint) }),
					layout.Rigid(spacer(0, 4)),
					layout.Rigid(s.muted("Tokens: "+organize.Tokens+"   ( / separates folders )")),
				)
			})
		}),
		layout.Rigid(spacer(0, 6)),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			l := material.Body2(s.th, "Example:  "+preview())
			l.Font.Typeface = "Go Mono"
			l.Color = s.th.ContrastBg
			return l.Layout(gtx)
		}),
	)
}

func (s *state) optionsSection(gtx layout.Context) layout.Dimensions {
	ffLabel := "Use ffprobe for other formats (mkv, avi, heic…)"
	ffGtx := gtx
	if !media.FFprobeAvailable() {
		ffLabel = "Use ffprobe (not installed)"
		ffGtx = gtx.Disabled()
	}
	opts := []struct {
		b     *widget.Bool
		label string
		gtx   layout.Context
	}{
		{&s.skipDups, "Skip files already in destination (identical content)", gtx},
		{&s.move, "Move instead of copy (delete from source after success)", gtx},
		{&s.verify, "Verify each copy (checksum, slower)", gtx},
		{&s.mtimeCap, "Set file modified time to the capture date", gtx},
		{&s.ffprobe, ffLabel, ffGtx},
		{&s.hidden, "Include hidden files and folders", gtx},
		{&s.dryRun, "Dry run (show what would happen, change nothing)", gtx},
	}
	ch := make([]layout.FlexChild, len(opts))
	for i, o := range opts {
		ch[i] = layout.Rigid(func(layout.Context) layout.Dimensions {
			return material.CheckBox(s.th, o.b, o.label).Layout(o.gtx)
		})
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx, ch...)
}

func (s *state) runPanel(gtx layout.Context, running bool) layout.Dimensions {
	s.mu.Lock()
	p, status, speed, dest := s.prog, s.status, s.speed, s.runDest
	logs := s.logs
	s.mu.Unlock()

	line := status
	if running {
		switch p.Phase {
		case "scanning":
			line = "Scanning source for media files…"
		default:
			verb := "Importing"
			if s.dryRun.Value {
				verb = "Dry run"
			}
			line = fmt.Sprintf("%s… %d%%", verb, percent(p))
			if p.Paused {
				line = fmt.Sprintf("Paused at %d%%", percent(p))
			}
		}
	}

	return s.cardRaw(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						label := "Start import"
						if s.dryRun.Value {
							label = "Start dry run"
						}
						g := gtx
						if running {
							g = gtx.Disabled()
						}
						return material.Button(s.th, &s.startBtn, label).Layout(g)
					}),
					layout.Rigid(spacer(8, 0)),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						label := "Pause"
						if p.Paused {
							label = "Resume"
						}
						b := material.Button(s.th, &s.pauseBtn, label)
						b.Background = rgb(0x50545e)
						if !running || p.Phase != "importing" || s.isDryRunning() {
							gtx = gtx.Disabled()
						}
						return b.Layout(gtx)
					}),
					layout.Rigid(spacer(8, 0)),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						b := material.Button(s.th, &s.cancelBtn, "Cancel")
						b.Background = rgb(0xbf4040)
						if !running {
							gtx = gtx.Disabled()
						}
						return b.Layout(gtx)
					}),
					layout.Rigid(spacer(12, 0)),
					layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
						return material.Body2(s.th, line).Layout(gtx)
					}),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						b := material.Button(s.th, &s.clearLogBtn, "Clear log")
						b.Background = rgb(0x50545e)
						b.Inset = layout.UniformInset(6)
						return b.Layout(gtx)
					}),
				)
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				if p.Phase == "" {
					return layout.Dimensions{}
				}
				return layout.Inset{Top: 10}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return s.progressDetails(gtx, p, running, speed, dest)
				})
			}),
			layout.Rigid(spacer(0, 8)),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				h := min(gtx.Dp(170), gtx.Constraints.Max.Y/5)
				gtx.Constraints.Min.Y, gtx.Constraints.Max.Y = h, h
				return material.List(s.th, &s.logList).Layout(gtx, len(logs), func(gtx layout.Context, i int) layout.Dimensions {
					l := material.Caption(s.th, logs[i])
					l.Font.Typeface = "Go Mono"
					l.MaxLines = 1
					switch {
					case strings.Contains(logs[i], "ERROR"):
						l.Color = rgb(0xff6b6b)
					case strings.Contains(logs[i], " SKIP "), strings.Contains(logs[i], "WARN"):
						l.Color = rgb(0xd8b25a)
					}
					return l.Layout(gtx)
				})
			}),
		)
	})
}

// percent is the overall byte-based completion, 0–100.
// isDryRunning reports whether the current run is a dry run (nothing to pause).
func (s *state) isDryRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running && s.runDry
}

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

var (
	colOK   = rgb(0x5fb878)
	colWarn = rgb(0xd8b25a)
	colErr  = rgb(0xe06c75)
)

// progressDetails shows the overall bars, statistics and per-file activity.
func (s *state) progressDetails(gtx layout.Context, p importer.Progress, running bool, speed float64, dest string) layout.Dimensions {
	scanning := running && p.Phase == "scanning"
	frac := float32(percent(p)) / 100
	if p.TotalBytes > 0 && !scanning {
		frac = min(1, float32(p.Bytes)/float32(p.TotalBytes))
	}
	fill := s.th.ContrastBg
	switch {
	case running && p.Paused:
		fill = colWarn
	case !running && p.Failed > 0:
		fill = colWarn
	case !running && p.Phase == "done" && p.Done == p.Total:
		fill = colOK
	}

	var label string
	switch {
	case scanning:
		label = fmt.Sprintf("Scanning… %d media files found · %s", p.Found, humanBytes(p.TotalBytes))
	case p.Total == 0 && p.Phase == "done":
		label = "No media files found"
	default:
		label = fmt.Sprintf("%d%%  ·  %s of %s", percent(p), humanBytes(p.Bytes), humanBytes(p.TotalBytes))
		if running && p.Paused {
			label = "Paused  ·  " + label
		}
	}

	fileFrac := float32(0)
	if p.Total > 0 {
		fileFrac = float32(p.Done) / float32(p.Total)
	}

	avg := float64(p.Bytes) / max(p.Elapsed.Seconds(), 0.001)
	if !running || speed <= 0 {
		speed = avg
	}
	eta := "—"
	if running && !scanning && !p.Paused && speed > 0 && p.TotalBytes > p.Bytes {
		eta = fmtDur(time.Duration(float64(p.TotalBytes-p.Bytes) / speed * float64(time.Second)))
	}

	type chip struct {
		k, v string
		c    color.NRGBA
	}
	chips := []chip{
		{"Files", fmt.Sprintf("%d / %d", p.Done, p.Total), s.th.Fg},
		{"Imported", strconv.Itoa(p.Copied), colOK},
		{"Skipped", strconv.Itoa(p.Skipped), colWarn},
		{"Failed", strconv.Itoa(p.Failed), colErr},
	}
	if p.Verified > 0 {
		chips = append(chips, chip{"Verified", strconv.Itoa(p.Verified), colOK})
	}
	if p.Resumed > 0 {
		chips = append(chips, chip{"Already done", strconv.Itoa(p.Resumed), withAlpha(s.th.Fg, 0xc0)})
	}
	if scanning {
		chips = []chip{{"Found", strconv.Itoa(p.Found), s.th.Fg}, {"Size", humanBytes(p.TotalBytes), s.th.Fg}}
	} else {
		switch {
		case running && p.Paused:
			chips = append(chips, chip{"Speed", "paused", colWarn})
		case running:
			chips = append(chips, chip{"Speed", humanBytes(int64(speed)) + "/s", s.th.Fg})
		}
		chips = append(chips, chip{"Average", humanBytes(int64(avg)) + "/s", s.th.Fg})
	}
	chips = append(chips, chip{"Elapsed", fmtDur(p.Elapsed), s.th.Fg})
	if running && !scanning {
		chips = append(chips, chip{"Remaining", eta, s.th.Fg})
	}
	for _, k := range media.AllKinds {
		if n := p.ByKind[k.String()]; n > 0 {
			chips = append(chips, chip{k.String(), strconv.Itoa(n), withAlpha(s.th.Fg, 0xc0)})
		}
	}

	active := p.Active
	if !running {
		active = nil
	}

	children := []layout.FlexChild{
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return s.bar(gtx, 24, frac, fill, label, scanning)
		}),
	}
	if !scanning && p.Total > 0 {
		children = append(children,
			layout.Rigid(spacer(0, 4)),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return s.bar(gtx, 5, fileFrac, withAlpha(fill, 0xa0), "", false)
			}),
		)
	}
	children = append(children,
		layout.Rigid(spacer(0, 8)),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return s.wrap(gtx, len(chips), func(gtx layout.Context, i int) layout.Dimensions {
				c := chips[i]
				return s.chip(gtx, c.k, c.v, c.c)
			})
		}),
	)
	if len(active) > 0 {
		children = append(children, layout.Rigid(spacer(0, 8)))
		for _, fp := range active {
			children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Bottom: 3}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return s.activeRow(gtx, fp, dest)
				})
			}))
		}
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
}

// activeRow renders one in-flight file: mini bar, stage, name → folder, bytes.
func (s *state) activeRow(gtx layout.Context, fp importer.FileProgress, dest string) layout.Dimensions {
	frac := float32(0)
	if fp.Size > 0 {
		frac = min(1, float32(fp.Bytes)/float32(fp.Size))
	}
	name := filepath.Base(fp.Path)
	if fp.Dest != "" {
		dir := filepath.Dir(fp.Dest)
		if rel, err := filepath.Rel(dest, dir); err == nil {
			dir = rel
		}
		name += "  →  " + dir + string(filepath.Separator)
		if b := filepath.Base(fp.Dest); b != filepath.Base(fp.Path) {
			name += b
		}
	}
	return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			w := gtx.Dp(110)
			gtx.Constraints.Min.X, gtx.Constraints.Max.X = w, w
			return s.bar(gtx, 6, frac, s.th.ContrastBg, "", false)
		}),
		layout.Rigid(spacer(10, 0)),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			w := gtx.Dp(104)
			gtx.Constraints.Min.X, gtx.Constraints.Max.X = w, w
			l := material.Caption(s.th, fp.Stage)
			l.Color = withAlpha(s.th.Fg, 0xa0)
			l.MaxLines = 1
			return l.Layout(gtx)
		}),
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			gtx.Constraints.Min.X = gtx.Constraints.Max.X
			l := material.Caption(s.th, name)
			l.MaxLines = 1
			return l.Layout(gtx)
		}),
		layout.Rigid(spacer(10, 0)),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			l := material.Caption(s.th, fmt.Sprintf("%s / %s", humanBytes(fp.Bytes), humanBytes(fp.Size)))
			l.Color = withAlpha(s.th.Fg, 0xa0)
			l.Font.Typeface = "Go Mono"
			return l.Layout(gtx)
		}),
	)
}

// chip is a small "Label value" statistic.
func (s *state) chip(gtx layout.Context, k, v string, c color.NRGBA) layout.Dimensions {
	bg := withAlpha(s.th.Fg, 0x12)
	return layout.Background{}.Layout(gtx,
		func(gtx layout.Context) layout.Dimensions {
			r := clip.UniformRRect(image.Rectangle{Max: gtx.Constraints.Min}, gtx.Dp(4))
			paint.FillShape(gtx.Ops, bg, r.Op(gtx.Ops))
			return layout.Dimensions{Size: gtx.Constraints.Min}
		},
		func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Left: 8, Right: 8, Top: 4, Bottom: 4}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Alignment: layout.Baseline}.Layout(gtx,
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						l := material.Caption(s.th, k)
						l.Color = withAlpha(s.th.Fg, 0x90)
						return l.Layout(gtx)
					}),
					layout.Rigid(spacer(6, 0)),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						l := material.Body2(s.th, v)
						l.Color = c
						l.Font.Weight = font.SemiBold
						return l.Layout(gtx)
					}),
				)
			})
		},
	)
}

// bar draws a rounded progress bar of height h filling the available width,
// with an optional centred label. indeterminate animates a moving segment.
func (s *state) bar(gtx layout.Context, h unit.Dp, frac float32, fg color.NRGBA, label string, indeterminate bool) layout.Dimensions {
	size := image.Pt(gtx.Constraints.Max.X, gtx.Dp(h))
	rr := gtx.Dp(min(h/2, 6))
	track := clip.UniformRRect(image.Rectangle{Max: size}, rr).Push(gtx.Ops)
	paint.Fill(gtx.Ops, withAlpha(s.th.Fg, 0x1e))
	if indeterminate {
		const period = 1500 * time.Millisecond
		t := float32(time.Now().UnixNano()%int64(period)) / float32(period)
		seg := size.X / 4
		x := int(t*float32(size.X+seg)) - seg
		st := clip.Rect{Min: image.Pt(max(0, x), 0), Max: image.Pt(min(size.X, x+seg), size.Y)}.Push(gtx.Ops)
		paint.Fill(gtx.Ops, fg)
		st.Pop()
		gtx.Execute(op.InvalidateCmd{At: gtx.Now.Add(16 * time.Millisecond)})
	} else if w := int(float32(size.X) * max(0, min(1, frac))); w > 0 {
		st := clip.Rect{Max: image.Pt(w, size.Y)}.Push(gtx.Ops)
		paint.Fill(gtx.Ops, fg)
		st.Pop()
	}
	track.Pop()
	if label != "" {
		lg := gtx
		lg.Constraints = layout.Exact(size)
		layout.Center.Layout(lg, func(gtx layout.Context) layout.Dimensions {
			l := material.Body2(s.th, label)
			l.Font.Weight = font.SemiBold
			l.MaxLines = 1
			return l.Layout(gtx)
		})
	}
	return layout.Dimensions{Size: size}
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

// ---------- helpers ----------

func (s *state) pathRow(gtx layout.Context, ed *widget.Editor, hint string, browse *widget.Clickable) layout.Dimensions {
	return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions { return s.editor(gtx, ed, hint) }),
		layout.Rigid(spacer(8, 0)),
		layout.Rigid(material.Button(s.th, browse, "Browse…").Layout),
	)
}

func (s *state) editor(gtx layout.Context, ed *widget.Editor, hint string) layout.Dimensions {
	return widget.Border{Color: withAlpha(s.th.Fg, 0x50), CornerRadius: 4, Width: 1}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.UniformInset(8).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			e := material.Editor(s.th, ed, hint)
			e.HintColor = withAlpha(s.th.Fg, 0x70)
			return e.Layout(gtx)
		})
	})
}

func (s *state) card(gtx layout.Context, title string, body layout.Widget) layout.Dimensions {
	return s.cardRaw(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				l := material.Subtitle1(s.th, title)
				l.Font.Weight = font.SemiBold
				return l.Layout(gtx)
			}),
			layout.Rigid(spacer(0, 8)),
			layout.Rigid(body),
		)
	})
}

func (s *state) cardRaw(gtx layout.Context, body layout.Widget) layout.Dimensions {
	bg := lighten(s.th.Bg)
	return layout.Background{}.Layout(gtx,
		func(gtx layout.Context) layout.Dimensions {
			r := clip.UniformRRect(image.Rectangle{Max: gtx.Constraints.Min}, gtx.Dp(8))
			paint.FillShape(gtx.Ops, bg, r.Op(gtx.Ops))
			return layout.Dimensions{Size: gtx.Constraints.Min}
		},
		func(gtx layout.Context) layout.Dimensions {
			gtx.Constraints.Min.X = gtx.Constraints.Max.X
			return layout.UniformInset(14).Layout(gtx, body)
		},
	)
}

func (s *state) muted(txt string) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		l := material.Body2(s.th, txt)
		l.Color = withAlpha(s.th.Fg, 0xa0)
		return l.Layout(gtx)
	}
}

// wrap lays out n items left-to-right, wrapping onto new rows.
func (s *state) wrap(gtx layout.Context, n int, item func(layout.Context, int) layout.Dimensions) layout.Dimensions {
	maxW := gtx.Constraints.Max.X
	gap := gtx.Dp(6)
	x, y, rowH, width := 0, 0, 0, 0
	cgtx := gtx
	cgtx.Constraints.Min = image.Point{}
	for i := range n {
		m := op.Record(gtx.Ops)
		d := item(cgtx, i)
		call := m.Stop()
		if x > 0 && x+d.Size.X > maxW {
			x, y, rowH = 0, y+rowH+gap, 0
		}
		st := op.Offset(image.Pt(x, y)).Push(gtx.Ops)
		call.Add(gtx.Ops)
		st.Pop()
		x += d.Size.X + gap
		rowH = max(rowH, d.Size.Y)
		width = max(width, x-gap)
	}
	return layout.Dimensions{Size: image.Pt(width, y+rowH)}
}

func spacer(w, h unit.Dp) layout.Widget {
	return layout.Spacer{Width: w, Height: h}.Layout
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

func lighten(c color.NRGBA) color.NRGBA {
	if c.R > 0x80 { // light theme: darken slightly instead
		return color.NRGBA{R: c.R - 10, G: c.G - 10, B: c.B - 8, A: c.A}
	}
	return color.NRGBA{R: c.R + 14, G: c.G + 14, B: c.B + 16, A: c.A}
}
