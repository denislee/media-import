// Package ui implements the Gio (native Wayland) user interface.
package ui

import (
	"context"
	"fmt"
	"image"
	"image/color"
	"os"
	"os/exec"
	"path/filepath"
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

// Version is set by main from the build's ldflags.
var Version = "dev"

type state struct {
	win *app.Window
	th  *material.Theme

	srcEd, dstEd, customDirEd, customNameEd widget.Editor
	srcBrowse, dstBrowse                    widget.Clickable
	startBtn, cancelBtn, clearLogBtn        widget.Clickable
	dirEnum, nameEnum                       widget.Enum
	kinds                                   map[media.Kind]*widget.Bool
	move, skipDups, verify, ffprobe         widget.Bool
	mtimeCap, dryRun, hidden                widget.Bool
	page, logList                           widget.List

	volMu   sync.Mutex
	vols    []devices.Volume
	volBtns []widget.Clickable

	mu      sync.Mutex
	running bool
	cancel  context.CancelFunc
	prog    importer.Progress
	status  string
	logs    []string

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
	s.volMu.Lock()
	defer s.volMu.Unlock()
	if fmt.Sprint(vols) == fmt.Sprint(s.vols) {
		return false
	}
	s.vols = vols
	if len(s.volBtns) < len(vols) {
		s.volBtns = append(s.volBtns, make([]widget.Clickable, len(vols)-len(s.volBtns))...)
	}
	return true
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
	opt := importer.Options{
		Sources:       []string{expandHome(cfg.Source)},
		Dest:          expandHome(cfg.Dest),
		DirTemplate:   s.dirTemplate(),
		NameTemplate:  s.nameTemplate(),
		Kinds:         map[media.Kind]bool{},
		Move:          cfg.Move,
		DryRun:        cfg.DryRun,
		SkipDups:      cfg.SkipDups,
		Verify:        cfg.Verify,
		UseFFprobe:    cfg.FFprobe,
		MtimeCapture:  cfg.MtimeCapture,
		IncludeHidden: cfg.Hidden,
	}
	for k, b := range s.kinds {
		opt.Kinds[k] = b.Value
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.running, s.cancel = true, cancel
	s.prog = importer.Progress{Phase: "scanning"}
	s.status = "Scanning…"
	s.mu.Unlock()
	mode := "Import"
	if opt.DryRun {
		mode = "Dry run"
	}
	s.addLog(fmt.Sprintf("%s started: %s -> %s (folders %q, names %q)", mode, opt.Sources[0], opt.Dest, opt.DirTemplate, opt.NameTemplate))

	go func() {
		sum, err := importer.Run(ctx, opt, func(p importer.Progress) {
			s.mu.Lock()
			s.prog = p
			s.mu.Unlock()
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
		s.addLog(msg)
		s.mu.Lock()
		s.running, s.cancel, s.status = false, nil, msg
		s.mu.Unlock()
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
	for i := range s.vols {
		if s.volBtns[i].Clicked(gtx) {
			s.srcEd.SetText(s.vols[i].Path)
		}
	}
	s.volMu.Unlock()

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
	s.volMu.Unlock()

	children := []layout.FlexChild{
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return s.pathRow(gtx, &s.srcEd, "/run/media/you/CARD/DCIM", &s.srcBrowse)
		}),
		layout.Rigid(spacer(0, 8)),
	}
	if len(vols) == 0 {
		children = append(children, layout.Rigid(s.muted("No removable media detected. Plug in a device and mount it (it will appear here automatically).")))
	} else {
		children = append(children, layout.Rigid(s.muted("Connected media — click to use as source:")), layout.Rigid(spacer(0, 4)))
		children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return s.wrap(gtx, len(vols), func(gtx layout.Context, i int) layout.Dimensions {
				b := material.Button(s.th, &s.volBtns[i], fmt.Sprintf("%s  (%s)", vols[i].Label, vols[i].FS))
				b.Background = rgb(0x2e3440)
				if s.th.Bg == rgb(0xffffff) {
					b.Background = rgb(0xdde3ee)
					b.Color = rgb(0x111111)
				}
				b.Inset = layout.UniformInset(8)
				return b.Layout(gtx)
			})
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
	p, status := s.prog, s.status
	logs := s.logs
	s.mu.Unlock()

	frac := float32(0)
	if p.TotalBytes > 0 {
		frac = float32(p.Bytes) / float32(p.TotalBytes)
	}
	line := status
	if running {
		switch p.Phase {
		case "scanning":
			line = fmt.Sprintf("Scanning… %d media files found (%s)", p.Found, humanBytes(p.TotalBytes))
		default:
			speed := float64(p.Bytes) / max(p.Elapsed.Seconds(), 0.001)
			eta := ""
			if speed > 0 && p.TotalBytes > p.Bytes {
				eta = " · ETA " + (time.Duration(float64(p.TotalBytes-p.Bytes)/speed) * time.Second).Round(time.Second).String()
			}
			line = fmt.Sprintf("%d / %d files · %s / %s · %s/s%s · %d skipped · %d failed",
				p.Done, p.Total, humanBytes(p.Bytes), humanBytes(p.TotalBytes), humanBytes(int64(speed)), eta, p.Skipped, p.Failed)
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
			layout.Rigid(spacer(0, 8)),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				pb := material.ProgressBar(s.th, frac)
				return pb.Layout(gtx)
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
