package ui

import (
	"fmt"
	"image"
	"image/color"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gioui.org/font"
	"gioui.org/io/key"
	"gioui.org/layout"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"github.com/denislee/media-import/internal/importer"
	"github.com/denislee/media-import/internal/media"
	"github.com/denislee/media-import/internal/organize"
)

const (
	tabSetup    = "setup"
	tabProgress = "progress"
	tabLog      = "log"
)

// wideMin is the window width from which devices get their own sidebar.
const wideMin = 820

// contentMax caps the width of the setup and progress pages.
const contentMax = 940

// ---------- frame ----------

func (s *state) layout(gtx layout.Context) layout.Dimensions {
	running := s.update(gtx)
	wide := gtx.Constraints.Max.X >= gtx.Dp(wideMin)

	main := func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions { return s.topBar(gtx, wide, running) }),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions { return s.banners(gtx, running) }),
			layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
				switch s.tabs.Value {
				case tabProgress:
					return s.progressPage(gtx, running)
				case tabLog:
					return s.logPage(gtx)
				default:
					return s.setupPage(gtx, running, wide)
				}
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions { return s.actionBar(gtx, running) }),
		)
	}
	if !wide {
		return main(gtx)
	}
	return layout.Flex{}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions { return s.sidebar(gtx, running) }),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			size := image.Pt(max(1, gtx.Dp(1)), gtx.Constraints.Max.Y)
			paint.FillShape(gtx.Ops, s.pal.border, clip.Rect{Max: size}.Op())
			return layout.Dimensions{Size: size}
		}),
		layout.Flexed(1, main),
	)
}

// update handles clicks and other input for this frame; it reports whether
// an import is running afterwards.
func (s *state) update(gtx layout.Context) bool {
	s.mu.Lock()
	running := s.running
	s.mu.Unlock()

	s.tabs.Update(gtx)
	if s.startBtn.Clicked(gtx) && !running {
		s.start()
		running = true
		s.tabs.Value = tabProgress
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
		s.togglePause()
	}
	if s.resumeBtn.Clicked(gtx) && !running {
		s.resume()
		s.mu.Lock()
		running = s.running
		s.mu.Unlock()
		if running {
			s.tabs.Value = tabProgress
		}
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
		s.logs.Reset()
		s.mu.Unlock()
	}
	if s.srcBrowse.Clicked(gtx) || s.otherBtn.Clicked(gtx) {
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
	return running
}

// ---------- chrome ----------

func (s *state) brand(gtx layout.Context) layout.Dimensions {
	return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return box(gtx, s.pal.accent, s.pal.accent, 9, layout.UniformInset(7), icon(icDownload, 20, s.pal.accentFg))
		}),
		layout.Rigid(spacer(10, 0)),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					l := material.Subtitle1(s.th, "Media Import")
					l.Font.Weight = font.Bold
					return l.Layout(gtx)
				}),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					l := material.Caption(s.th, "Version "+Version)
					l.Color, l.MaxLines = s.pal.faint, 1
					return l.Layout(gtx)
				}),
			)
		}),
	)
}

func (s *state) topBar(gtx layout.Context, wide, running bool) layout.Dimensions {
	s.mu.Lock()
	p := s.prog
	s.mu.Unlock()
	progLabel := "Progress"
	switch {
	case running && p.Phase == "scanning":
		progLabel = "Progress · scanning"
	case running:
		progLabel = fmt.Sprintf("Progress · %d%%", percent(p))
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Left: 20, Right: 20, Top: 10}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				var ch []layout.FlexChild
				if !wide {
					ch = append(ch, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return layout.Inset{Bottom: 8}.Layout(gtx, s.brand)
					}), layout.Rigid(spacer(24, 0)))
				}
				ch = append(ch,
					layout.Rigid(func(gtx layout.Context) layout.Dimensions { return s.tab(gtx, tabSetup, icSettings, "Setup") }),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return s.tab(gtx, tabProgress, icDownload, progLabel)
					}),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions { return s.tab(gtx, tabLog, icList, "Log") }),
				)
				return layout.Flex{Alignment: layout.End}.Layout(gtx, ch...)
			})
		}),
		layout.Rigid(s.hline),
	)
}

// tab is one entry of the top navigation, underlined when selected.
func (s *state) tab(gtx layout.Context, key string, ic *widget.Icon, label string) layout.Dimensions {
	sel := s.tabs.Value == key
	hk, hov := s.tabs.Hovered()
	fg := s.pal.muted
	if sel || (hov && hk == key) {
		fg = s.pal.fg
	}
	if sel {
		fg = s.pal.accent
	}
	return s.tabs.Layout(gtx, key, func(gtx layout.Context) layout.Dimensions {
		gtx.Constraints.Min = image.Point{}
		d := layout.Inset{Top: 8, Bottom: 11, Left: 12, Right: 14}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
				layout.Rigid(icon(ic, 18, fg)),
				layout.Rigid(spacer(7, 0)),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					l := material.Body1(s.th, label)
					l.Color, l.Font.Weight, l.MaxLines = fg, font.Medium, 1
					return l.Layout(gtx)
				}),
			)
		})
		if sel {
			h := gtx.Dp(3)
			r := clip.UniformRRect(image.Rect(0, d.Size.Y-h, d.Size.X, d.Size.Y+h), h)
			paint.FillShape(gtx.Ops, s.pal.accent, r.Op(gtx.Ops))
		}
		return d
	})
}

// banners shows notices that apply whatever tab is open: the sudo password
// prompt, mount/eject errors and the interrupted-import offer.
func (s *state) banners(gtx layout.Context, running bool) layout.Dimensions {
	s.mu.Lock()
	busy, target, opErr := s.opBusy, s.authOp, s.opErr
	focusPw := s.pwFocus && target != nil && !busy
	if focusPw {
		s.pwFocus = false
	}
	hasResume := s.resMeta != nil && s.resState != nil && !s.running
	s.mu.Unlock()

	var ch []layout.FlexChild
	add := func(w layout.Widget) {
		ch = append(ch, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: 12}.Layout(gtx, w)
		}))
	}
	if target != nil {
		add(func(gtx layout.Context) layout.Dimensions {
			title := fmt.Sprintf("%s %s needs administrator rights", target.verb, target.what)
			body := "Enter your sudo password. It is passed to sudo and not stored."
			if opErr != "" {
				body = opErr
			}
			return s.banner(gtx, icWarn, s.pal.warn, title, body, func(gtx layout.Context) layout.Dimensions {
				if busy {
					gtx = gtx.Disabled()
				}
				return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
					layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
						gtx.Constraints.Max.X = min(gtx.Constraints.Max.X, gtx.Dp(360))
						d := s.editor(gtx, &s.pwEd, "Password")
						if focusPw {
							gtx.Execute(key.FocusCmd{Tag: &s.pwEd})
						}
						return d
					}),
					layout.Rigid(spacer(8, 0)),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return s.button(gtx, &s.pwOK, nil, target.verb, btnPrimary)
					}),
					layout.Rigid(spacer(8, 0)),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return s.button(gtx, &s.pwCancel, nil, "Cancel", btnSecondary)
					}),
				)
			})
		})
	} else if opErr != "" {
		add(func(gtx layout.Context) layout.Dimensions {
			return s.banner(gtx, icError, s.pal.err, opErr, "", nil)
		})
	}
	if busy && target == nil {
		add(func(gtx layout.Context) layout.Dimensions {
			return s.banner(gtx, icInfo, s.pal.accent, "Working…", "", nil)
		})
	}
	if hasResume && !running {
		add(s.resumeBanner)
	}
	if len(ch) == 0 {
		return layout.Dimensions{}
	}
	return layout.Inset{Left: 20, Right: 20}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return centered(gtx, contentMax, func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Vertical}.Layout(gtx, ch...)
		})
	})
}

// resumeBanner offers to continue an import that did not finish.
func (s *state) resumeBanner(gtx layout.Context) layout.Dimensions {
	s.mu.Lock()
	m, rs := s.resMeta, s.resState
	s.mu.Unlock()
	if m == nil || rs == nil {
		return layout.Dimensions{}
	}
	done := fmt.Sprintf("%d files done", rs.DoneCount())
	if rs.Total > 0 {
		done = fmt.Sprintf("%d of %d files done", rs.DoneCount(), rs.Total)
	}
	msg := fmt.Sprintf("%s → %s · started %s · %s. Resume skips files that were already imported and cleans up any half-written copy.",
		m.Source, m.Dest, m.Started.Format("Jan 2 15:04"), done)
	return s.banner(gtx, icHistory, s.pal.warn, "The last import did not finish", msg, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return s.button(gtx, &s.resumeBtn, icPlay, "Resume import", btnPrimary)
			}),
			layout.Rigid(spacer(8, 0)),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return s.button(gtx, &s.discardBtn, nil, "Discard", btnSecondary)
			}),
		)
	})
}

// actionBar is the fixed bottom strip with the status and the main buttons.
func (s *state) actionBar(gtx layout.Context, running bool) layout.Dimensions {
	s.mu.Lock()
	p, status, dry := s.prog, s.status, s.runDry
	s.mu.Unlock()
	if !running {
		dry = s.dryRun.Value
	}
	line := statusLine(p, running, dry, status)

	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(s.hline),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			gtx.Constraints.Min.X = gtx.Constraints.Max.X
			return box(gtx, s.pal.surface, s.pal.surface, 0, layout.Inset{Left: 20, Right: 20, Top: 12, Bottom: 12}, func(gtx layout.Context) layout.Dimensions {
				var ch []layout.FlexChild
				ch = append(ch, layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
					gtx.Constraints.Min.X = gtx.Constraints.Max.X
					return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							l := material.Body1(s.th, line)
							l.MaxLines = 1
							if !running {
								l = material.Body2(s.th, line)
								l.Color, l.MaxLines = s.pal.muted, 2
							}
							return l.Layout(gtx)
						}),
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							if !running {
								return layout.Dimensions{}
							}
							return layout.Inset{Top: 6}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
								gtx.Constraints.Max.X = min(gtx.Constraints.Max.X, gtx.Dp(420))
								fill := s.pal.accent
								if p.Paused {
									fill = s.pal.warn
								}
								return s.bar(gtx, 6, float32(percent(p))/100, fill, "", p.Phase == "scanning")
							})
						}),
					)
				}))
				ch = append(ch, layout.Rigid(spacer(16, 0)))
				if running {
					ch = append(ch,
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							ic, label := icPause, "Pause"
							if p.Paused {
								ic, label = icPlay, "Resume"
							}
							if p.Phase != "importing" || s.isDryRunning() {
								gtx = gtx.Disabled()
							}
							return s.button(gtx, &s.pauseBtn, ic, label, btnSecondary)
						}),
						layout.Rigid(spacer(8, 0)),
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							return s.button(gtx, &s.cancelBtn, icStop, "Cancel", btnDanger)
						}),
					)
				} else {
					ch = append(ch, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						label := "Start import"
						if s.dryRun.Value {
							label = "Start dry run"
						}
						return s.button(gtx, &s.startBtn, icPlay, label, btnPrimary)
					}))
				}
				return layout.Flex{Alignment: layout.Middle}.Layout(gtx, ch...)
			})
		}),
	)
}

// statusLine is the one-line summary shown in the action bar.
func statusLine(p importer.Progress, running, dry bool, status string) string {
	if !running {
		return status
	}
	if p.Phase == "scanning" {
		return "Scanning source for media files…"
	}
	if p.Paused {
		return fmt.Sprintf("Paused at %d%%", percent(p))
	}
	verb := "Importing"
	if dry {
		verb = "Dry run"
	}
	line := fmt.Sprintf("%s… %d%%", verb, percent(p))
	if act := activitySummary(p.Active); act != "" {
		line += "  ·  " + act
	}
	return line
}

// ---------- devices ----------

// sidebar lists connected media (wide windows only).
func (s *state) sidebar(gtx layout.Context, running bool) layout.Dimensions {
	w := gtx.Dp(272)
	gtx.Constraints.Min.X, gtx.Constraints.Max.X = w, w
	gtx.Constraints.Min.Y = gtx.Constraints.Max.Y
	paint.FillShape(gtx.Ops, s.pal.sidebar, clip.Rect{Max: gtx.Constraints.Max}.Op())
	if running {
		gtx = gtx.Disabled()
	}
	items := s.deviceItems()
	return layout.Inset{Left: 14, Right: 14, Top: 16, Bottom: 14}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Left: 4}.Layout(gtx, s.brand)
			}),
			layout.Rigid(spacer(0, 22)),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Left: 6, Bottom: 8}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					l := material.Caption(s.th, "IMPORT FROM")
					l.Color, l.Font.Weight = s.pal.faint, font.Bold
					return l.Layout(gtx)
				})
			}),
			layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
				if len(items) == 0 {
					return layout.Inset{Left: 6, Right: 6, Top: 4}.Layout(gtx,
						s.muted("No removable media detected. Plug in a card, camera or phone and it appears here."))
				}
				return material.List(s.th, &s.devList).Layout(gtx, len(items), func(gtx layout.Context, i int) layout.Dimensions {
					return layout.Inset{Bottom: 4}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						return s.deviceRow(gtx, items[i])
					})
				})
			}),
			layout.Rigid(spacer(0, 8)),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return s.deviceRow(gtx, devItem{clk: &s.otherBtn, ic: icFolder, title: "Other folder…", sub: "Pick any folder on this computer"})
			}),
		)
	})
}

type devItem struct {
	clk        *widget.Clickable
	ic         *widget.Icon
	title, sub string
	selected   bool
	trailing   string            // text shown at the right (e.g. "Mount")
	action     *widget.Clickable // optional separate icon button (eject)
	busy       bool
}

// deviceItems lists mounted volumes, then plugged-in but unmounted ones.
func (s *state) deviceItems() []devItem {
	src := filepath.Clean(expandHome(strings.TrimSpace(s.srcEd.Text())))
	s.mu.Lock()
	busy := s.opBusy
	s.mu.Unlock()
	s.volMu.Lock()
	defer s.volMu.Unlock()
	var items []devItem
	for i, v := range s.vols {
		ic := icCard
		if strings.Contains(v.Path, "/gvfs/") || strings.Contains(v.FS, "mtp") || strings.Contains(v.FS, "gvfs") {
			ic = icPhone
		}
		sel := src == v.Path || strings.HasPrefix(src, strings.TrimSuffix(v.Path, "/")+"/")
		sub := v.Path
		if v.FS != "" {
			sub = v.FS + " · " + v.Path
		}
		items = append(items, devItem{clk: &s.volBtns[i], ic: ic, title: v.Label, sub: sub, selected: sel,
			action: &s.ejectBtns[i], busy: busy})
	}
	for i, u := range s.unmounted {
		items = append(items, devItem{clk: &s.mountBtns[i], ic: icUSB, title: u.Name(),
			sub: fmt.Sprintf("Not mounted · %s · %s", u.FS, humanBytes(u.Size)), trailing: "Mount", busy: busy})
	}
	return items
}

func (s *state) deviceRow(gtx layout.Context, it devItem) layout.Dimensions {
	gtx.Constraints.Min.X = gtx.Constraints.Max.X
	bg, border := withAlpha(s.pal.fg, 0), withAlpha(s.pal.fg, 0)
	switch {
	case it.selected:
		bg, border = s.pal.accentSoft, withAlpha(s.pal.accent, 0x90)
	case it.clk.Hovered() && gtx.Enabled():
		bg = withAlpha(s.pal.fg, 0x10)
	}
	icCol := s.pal.muted
	if it.selected {
		icCol = s.pal.accent
	}
	rowGtx := gtx
	if it.busy && it.trailing != "" {
		rowGtx = gtx.Disabled()
	}
	return box(gtx, bg, border, 8, layout.Inset{Right: 4}, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
			layout.Flexed(1, func(layout.Context) layout.Dimensions {
				gtx := rowGtx
				gtx.Constraints.Max.X -= gtx.Dp(4)
				if it.action != nil {
					gtx.Constraints.Max.X -= gtx.Dp(36)
				}
				gtx.Constraints.Min.X = gtx.Constraints.Max.X
				return it.clk.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return layout.Inset{Top: 9, Bottom: 9, Left: 10, Right: 6}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
							layout.Rigid(icon(it.ic, 22, s.dim(icCol, gtx))),
							layout.Rigid(spacer(10, 0)),
							layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
								return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
									layout.Rigid(func(gtx layout.Context) layout.Dimensions {
										l := material.Body1(s.th, it.title)
										l.Color, l.Font.Weight, l.MaxLines = s.dim(s.pal.fg, gtx), font.Medium, 1
										return l.Layout(gtx)
									}),
									layout.Rigid(func(gtx layout.Context) layout.Dimensions {
										l := material.Caption(s.th, it.sub)
										l.Color, l.MaxLines = s.dim(s.pal.muted, gtx), 1
										return l.Layout(gtx)
									}),
								)
							}),
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								if it.trailing == "" {
									return layout.Dimensions{}
								}
								l := material.Body2(s.th, it.trailing)
								l.Color, l.Font.Weight = s.dim(s.pal.accent, gtx), font.SemiBold
								return layout.Inset{Left: 8}.Layout(gtx, l.Layout)
							}),
						)
					})
				})
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				if it.action == nil {
					return layout.Dimensions{}
				}
				if it.busy {
					gtx = gtx.Disabled()
				}
				return s.button(gtx, it.action, icEject, "", btnGhost)
			}),
		)
	})
}

// ---------- setup page ----------

func (s *state) setupPage(gtx layout.Context, running, wide bool) layout.Dimensions {
	if running {
		gtx = gtx.Disabled()
	}
	var sections []layout.Widget
	if !wide {
		sections = append(sections, func(gtx layout.Context) layout.Dimensions {
			return s.card(gtx, "Connected media", icCard, func(gtx layout.Context) layout.Dimensions {
				items := s.deviceItems()
				if len(items) == 0 {
					return s.muted("No removable media detected. Plug in a card, camera or phone and it appears here.")(gtx)
				}
				return grid(gtx, len(items), 240, 8, func(gtx layout.Context, i int) layout.Dimensions {
					return s.deviceRow(gtx, items[i])
				})
			})
		})
	}
	sections = append(sections,
		func(gtx layout.Context) layout.Dimensions {
			return s.card(gtx, "Locations", icFolder, s.locationsSection)
		},
		func(gtx layout.Context) layout.Dimensions {
			return s.card(gtx, "What to import", icPhoto, s.kindsSection)
		},
		func(gtx layout.Context) layout.Dimensions {
			return s.card(gtx, "Organize", icOrganize, s.organizeSection)
		},
		func(gtx layout.Context) layout.Dimensions {
			return s.card(gtx, "Options", icSettings, s.optionsSection)
		},
	)
	return material.List(s.th, &s.page).Layout(gtx, len(sections), func(gtx layout.Context, i int) layout.Dimensions {
		in := layout.Inset{Left: 20, Right: 20, Top: 8, Bottom: 8}
		if i == 0 {
			in.Top = 18
		}
		if i == len(sections)-1 {
			in.Bottom = 20
		}
		return in.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return centered(gtx, contentMax, sections[i])
		})
	})
}

func (s *state) locationsSection(gtx layout.Context) layout.Dimensions {
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(s.fieldLabel("Source — the card, camera or folder to import from")),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return s.pathRow(gtx, &s.srcEd, "/run/media/you/CARD/DCIM", &s.srcBrowse)
		}),
		layout.Rigid(spacer(0, 14)),
		layout.Rigid(s.fieldLabel("Destination — where the organised copies go")),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return s.pathRow(gtx, &s.dstEd, "~/Pictures/Imported", &s.dstBrowse)
		}),
	)
}

func (s *state) kindsSection(gtx layout.Context) layout.Dimensions {
	type kd struct {
		ic          *widget.Icon
		title, exts string
	}
	desc := map[media.Kind]kd{
		media.Image: {icPhoto, "Photos", "jpg, png, heic, webp…"},
		media.Raw:   {icRaw, "RAW", "cr2, cr3, nef, arw, dng…"},
		media.Video: {icVideo, "Videos", "mp4, mov, mkv, mts…"},
		media.Audio: {icAudio, "Audio", "mp3, wav, flac, m4a…"},
	}
	return grid(gtx, len(media.AllKinds), 230, 10, func(gtx layout.Context, i int) layout.Dimensions {
		k := media.AllKinds[i]
		d := desc[k]
		return s.kindTile(gtx, s.kinds[k], d.ic, d.title, d.exts)
	})
}

// kindTile is a toggleable tile for one media category.
func (s *state) kindTile(gtx layout.Context, b *widget.Bool, ic *widget.Icon, title, sub string) layout.Dimensions {
	b.Update(gtx)
	on := b.Value
	bg, border, icCol := s.pal.surfaceHi, s.pal.border, s.pal.muted
	if on {
		bg, border, icCol = s.pal.accentSoft, s.pal.accent, s.pal.accent
	} else if b.Hovered() && gtx.Enabled() {
		bg = mix(bg, s.pal.fg, 0.06)
	}
	return b.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		gtx.Constraints.Min.X = gtx.Constraints.Max.X
		return box(gtx, s.dim(bg, gtx), s.dim(border, gtx), 9, layout.UniformInset(12), func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
				layout.Rigid(icon(ic, 28, s.dim(icCol, gtx))),
				layout.Rigid(spacer(12, 0)),
				layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
					return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							l := material.Body1(s.th, title)
							l.Color, l.Font.Weight = s.dim(s.pal.fg, gtx), font.SemiBold
							return l.Layout(gtx)
						}),
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							l := material.Caption(s.th, sub)
							l.Color, l.MaxLines = s.dim(s.pal.muted, gtx), 1
							return l.Layout(gtx)
						}),
					)
				}),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					sz := gtx.Dp(20)
					if on {
						return icon(icCheck, 20, s.dim(s.pal.accent, gtx))(gtx)
					}
					r := image.Rect(gtx.Dp(2), gtx.Dp(2), sz-gtx.Dp(2), sz-gtx.Dp(2))
					paint.FillShape(gtx.Ops, s.dim(s.pal.faint, gtx), clip.Stroke{Path: clip.Ellipse(r).Path(gtx.Ops), Width: float32(gtx.Dp(1.5))}.Op())
					return layout.Dimensions{Size: image.Pt(sz, sz)}
				}),
			)
		})
	})
}

func (s *state) organizeSection(gtx layout.Context) layout.Dimensions {
	s.dirEnum.Update(gtx)
	s.nameEnum.Update(gtx)
	folders := func(gtx layout.Context) layout.Dimensions {
		return s.templateSection(gtx, "Folders", &s.dirEnum, organize.DirPresets, &s.customDirEd, "{YYYY}/{MM}/{DD}")
	}
	names := func(gtx layout.Context) layout.Dimensions {
		return s.templateSection(gtx, "File names", &s.nameEnum, organize.NamePresets, &s.customNameEd, "{YYYY}{MM}{DD}_{name}")
	}
	var body layout.Widget
	if gtx.Constraints.Max.X >= gtx.Dp(640) {
		body = func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{}.Layout(gtx,
				layout.Flexed(1, folders),
				layout.Rigid(spacer(24, 0)),
				layout.Flexed(1, names),
			)
		}
	} else {
		body = func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
				layout.Rigid(folders), layout.Rigid(spacer(0, 16)), layout.Rigid(names))
		}
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(body),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			if s.dirEnum.Value != customKey && s.nameEnum.Value != customKey {
				return layout.Dimensions{}
			}
			return layout.Inset{Top: 10}.Layout(gtx, s.muted("Tokens: "+organize.Tokens+"   ( / separates folders )"))
		}),
		layout.Rigid(spacer(0, 14)),
		layout.Rigid(s.previewBox),
	)
}

// previewBox shows where a sample photo would end up.
func (s *state) previewBox(gtx layout.Context) layout.Dimensions {
	dest := strings.TrimSpace(s.dstEd.Text())
	if dest == "" {
		dest = "<destination>"
	}
	f := sampleFields()
	path := filepath.Join(dest, organize.Dir(s.dirTemplate(), f), organize.FileName(s.nameTemplate(), f, ".jpg"))
	gtx.Constraints.Min.X = gtx.Constraints.Max.X
	return box(gtx, s.pal.field, s.pal.border, 7, layout.Inset{Top: 10, Bottom: 10, Left: 12, Right: 12}, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
			layout.Rigid(icon(icFile, 18, s.dim(s.pal.muted, gtx))),
			layout.Rigid(spacer(10, 0)),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				l := material.Caption(s.th, "Example")
				l.Color = s.dim(s.pal.muted, gtx)
				return l.Layout(gtx)
			}),
			layout.Rigid(spacer(10, 0)),
			layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
				l := material.Body2(s.th, path)
				l.Font.Typeface = "Go Mono"
				l.Color = s.dim(s.pal.accent, gtx)
				return l.Layout(gtx)
			}),
		)
	})
}

func (s *state) templateSection(gtx layout.Context, title string, enum *widget.Enum, presets []organize.Preset, custom *widget.Editor, hint string) layout.Dimensions {
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(s.fieldLabel(title)),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return s.wrap(gtx, len(presets)+1, func(gtx layout.Context, i int) layout.Dimensions {
				key, label := customKey, "Custom…"
				if i < len(presets) {
					key, label = strconv.Itoa(i), presets[i].Name
				}
				return s.pill(gtx, enum, key, label)
			})
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			if enum.Value != customKey {
				return layout.Dimensions{}
			}
			return layout.Inset{Top: 10}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return s.editor(gtx, custom, hint)
			})
		}),
	)
}

func (s *state) optionsSection(gtx layout.Context) layout.Dimensions {
	type group struct {
		title string
		rows  layout.Widget
	}
	row := func(b *widget.Bool, title, desc string) layout.FlexChild {
		return layout.Rigid(func(gtx layout.Context) layout.Dimensions { return s.optionRow(gtx, b, title, desc) })
	}
	ffDesc := "Reads dates from mkv, avi, mts and other video and audio formats"
	ffDisabled := !media.FFprobeAvailable()
	if ffDisabled {
		ffDesc = "ffprobe is not installed"
	}
	groups := []group{
		{"Duplicates", func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
				row(&s.skipDups, "Skip files already imported", "Identical files in the destination are not copied again"),
				layout.Rigid(s.dupOptions),
			)
		}},
		{"Safety", func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
				row(&s.verify, "Verify each copy", "Re-read every copy from disk and compare checksums (slower)"),
				row(&s.move, "Move instead of copy", "Delete from the source after a successful copy"),
				row(&s.dryRun, "Dry run", "Show what would happen without changing anything"),
			)
		}},
		{"Dates", func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
				row(&s.mtimeCap, "Set modified time to capture date", "Otherwise the source file's time is kept"),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					if ffDisabled {
						gtx = gtx.Disabled()
					}
					return s.optionRow(gtx, &s.ffprobe, "Use ffprobe", ffDesc)
				}),
			)
		}},
		{"Scanning", func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
				row(&s.hidden, "Include hidden files and folders", "Also look inside dot-folders on the source"),
			)
		}},
	}
	return grid(gtx, len(groups), 330, 20, func(gtx layout.Context, i int) layout.Dimensions {
		g := groups[i]
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Left: 8, Bottom: 2}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					l := material.Caption(s.th, strings.ToUpper(g.title))
					l.Color, l.Font.Weight = s.dim(s.pal.faint, gtx), font.Bold
					return l.Layout(gtx)
				})
			}),
			layout.Rigid(g.rows),
		)
	})
}

// dupOptions is the duplicate-check method choice under "Skip files already
// imported"; it is disabled while that option is off.
func (s *state) dupOptions(gtx layout.Context) layout.Dimensions {
	if !s.skipDups.Value {
		gtx = gtx.Disabled()
	}
	s.dupEnum.Update(gtx)
	hint := "Size, date and sampled blocks must match"
	if s.dupEnum.Value == importer.DupCheckFull {
		hint = "Every byte is compared (slower)"
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Left: 8, Top: 2, Bottom: 4}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return s.pill(gtx, &s.dupEnum, importer.DupCheckFast, "Quick")
					}),
					layout.Rigid(spacer(6, 0)),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return s.pill(gtx, &s.dupEnum, importer.DupCheckFull, "Full")
					}),
					layout.Rigid(spacer(10, 0)),
					layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
						l := material.Caption(s.th, hint)
						l.Color = s.dim(s.pal.muted, gtx)
						return l.Layout(gtx)
					}),
				)
			})
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return s.optionRow(gtx, &s.dupAnyName, "Also match other names", "Finds copies saved under a different name (compares sampled blocks, then checksums)")
		}),
	)
}

// ---------- progress page ----------

func (s *state) progressPage(gtx layout.Context, running bool) layout.Dimensions {
	s.mu.Lock()
	p, status, speed, dest := s.prog, s.status, s.speed, s.runDest
	s.mu.Unlock()

	if p.Phase == "" {
		return layout.Center.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Vertical, Alignment: layout.Middle}.Layout(gtx,
				layout.Rigid(icon(icDownload, 56, s.pal.faint)),
				layout.Rigid(spacer(0, 12)),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					l := material.H6(s.th, "Nothing imported yet")
					return l.Layout(gtx)
				}),
				layout.Rigid(spacer(0, 4)),
				layout.Rigid(s.muted("Choose a source and destination, then press Start import.")),
			)
		})
	}
	items := []layout.Widget{
		func(gtx layout.Context) layout.Dimensions { return s.progressSummary(gtx, p, running, speed, status) },
	}
	active := p.Active
	if running && len(active) > 0 {
		items = append(items, func(gtx layout.Context) layout.Dimensions {
			return s.card(gtx, "Working on", icFile, func(gtx layout.Context) layout.Dimensions {
				ch := make([]layout.FlexChild, 0, len(active))
				for _, fp := range active {
					ch = append(ch, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return layout.Inset{Bottom: 6}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
							return s.activeRow(gtx, fp, dest)
						})
					}))
				}
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx, ch...)
			})
		})
	}
	return material.List(s.th, &s.progList).Layout(gtx, len(items), func(gtx layout.Context, i int) layout.Dimensions {
		in := layout.Inset{Left: 20, Right: 20, Top: 8, Bottom: 8}
		if i == 0 {
			in.Top = 18
		}
		return in.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return centered(gtx, contentMax, items[i])
		})
	})
}

// progressSummary is the headline, overall bars and statistics of a run.
func (s *state) progressSummary(gtx layout.Context, p importer.Progress, running bool, speed float64, status string) layout.Dimensions {
	scanning := running && p.Phase == "scanning"
	frac := float32(percent(p)) / 100
	if p.TotalBytes > 0 && !scanning {
		frac = min(1, float32(p.Bytes)/float32(p.TotalBytes))
	}
	fill, headIc, headCol := s.pal.accent, icDownload, s.pal.accent
	head := "Importing"
	switch {
	case scanning:
		head = "Scanning source…"
	case running && p.Paused:
		fill, headIc, headCol, head = s.pal.warn, icPause, s.pal.warn, "Paused"
	case running:
		if s.isDryRunning() {
			head = "Dry run in progress"
		}
	case p.Failed > 0:
		fill, headIc, headCol, head = s.pal.warn, icWarn, s.pal.warn, "Finished with problems"
	case p.Phase == "done" && p.Done == p.Total:
		fill, headIc, headCol, head = s.pal.ok, icCheck, s.pal.ok, "Import complete"
	default:
		fill, headIc, headCol, head = s.pal.warn, icWarn, s.pal.warn, "Import stopped"
	}

	var label string
	switch {
	case scanning:
		label = fmt.Sprintf("%d media files found · %s", p.Found, humanBytes(p.TotalBytes))
	case p.Total == 0 && p.Phase == "done":
		label = "No media files found"
	default:
		label = fmt.Sprintf("%d%%  ·  %s of %s", percent(p), humanBytes(p.Bytes), humanBytes(p.TotalBytes))
	}

	avg := float64(p.Bytes) / max(p.Elapsed.Seconds(), 0.001)
	if !running || speed <= 0 {
		speed = avg
	}
	eta := "—"
	if running && !scanning && !p.Paused && speed > 0 && p.TotalBytes > p.Bytes {
		eta = fmtDur(time.Duration(float64(p.TotalBytes-p.Bytes) / speed * float64(time.Second)))
	}

	type st struct {
		k, v string
		c    color.NRGBA
	}
	fg := s.pal.fg
	stats := []st{
		{"Files", fmt.Sprintf("%d / %d", p.Done, p.Total), fg},
		{"Imported", strconv.Itoa(p.Copied), s.pal.ok},
		{"Already imported", strconv.Itoa(p.Skipped), s.pal.warn},
		{"Failed", strconv.Itoa(p.Failed), s.pal.err},
	}
	if p.Verified > 0 {
		stats = append(stats, st{"Verified", strconv.Itoa(p.Verified), s.pal.ok})
	}
	if p.Resumed > 0 {
		stats = append(stats, st{"Done earlier", strconv.Itoa(p.Resumed), s.pal.muted})
	}
	if scanning {
		stats = []st{{"Found", strconv.Itoa(p.Found), fg}, {"Size", humanBytes(p.TotalBytes), fg}}
	} else {
		switch {
		case running && p.Paused:
			stats = append(stats, st{"Speed", "paused", s.pal.warn})
		case running:
			stats = append(stats, st{"Speed", humanBytes(int64(speed)) + "/s", fg})
		}
		stats = append(stats, st{"Average", humanBytes(int64(avg)) + "/s", fg})
	}
	stats = append(stats, st{"Elapsed", fmtDur(p.Elapsed), fg})
	if running && !scanning {
		stats = append(stats, st{"Remaining", eta, fg})
	}
	for _, k := range media.AllKinds {
		if n := p.ByKind[k.String()]; n > 0 {
			stats = append(stats, st{k.String(), strconv.Itoa(n), s.pal.muted})
		}
	}

	sub := ""
	if !running {
		sub = status
		if d := dupSummary(p); d != "" {
			sub += "\n" + d
		}
	}

	return s.cardRaw(gtx, func(gtx layout.Context) layout.Dimensions {
		ch := []layout.FlexChild{
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
					layout.Rigid(icon(headIc, 26, headCol)),
					layout.Rigid(spacer(10, 0)),
					layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
						l := material.H6(s.th, head)
						l.Font.Weight = font.SemiBold
						return l.Layout(gtx)
					}),
				)
			}),
		}
		if sub != "" {
			ch = append(ch, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Top: 4, Left: 36}.Layout(gtx, s.muted(sub))
			}))
		}
		ch = append(ch,
			layout.Rigid(spacer(0, 16)),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return s.bar(gtx, 28, frac, fill, label, scanning)
			}),
		)
		if !scanning && p.Total > 0 {
			fileFrac := float32(p.Done) / float32(p.Total)
			ch = append(ch,
				layout.Rigid(spacer(0, 6)),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return s.bar(gtx, 5, fileFrac, withAlpha(fill, 0xa0), "", false)
				}),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					l := material.Caption(s.th, fmt.Sprintf("%d of %d files", p.Done, p.Total))
					l.Color = s.pal.muted
					return layout.Inset{Top: 4}.Layout(gtx, l.Layout)
				}),
			)
		}
		ch = append(ch,
			layout.Rigid(spacer(0, 14)),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return grid(gtx, len(stats), 140, 8, func(gtx layout.Context, i int) layout.Dimensions {
					return s.stat(gtx, stats[i].k, stats[i].v, stats[i].c)
				})
			}),
		)
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx, ch...)
	})
}

// activeRow renders one in-flight file: name → folder, stage, mini bar, bytes.
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
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Alignment: layout.Baseline}.Layout(gtx,
				layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
					gtx.Constraints.Min.X = gtx.Constraints.Max.X
					l := material.Body2(s.th, name)
					l.MaxLines = 1
					return l.Layout(gtx)
				}),
				layout.Rigid(spacer(10, 0)),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					l := material.Caption(s.th, fmt.Sprintf("%s  ·  %s / %s", fp.Stage, humanBytes(fp.Bytes), humanBytes(fp.Size)))
					l.Color, l.MaxLines = s.pal.muted, 1
					return l.Layout(gtx)
				}),
			)
		}),
		layout.Rigid(spacer(0, 4)),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return s.bar(gtx, 4, frac, s.pal.accent, "", false)
		}),
	)
}

// ---------- log page ----------

func (s *state) logPage(gtx layout.Context) layout.Dimensions {
	s.mu.Lock()
	logs := s.logs.Lines()
	s.mu.Unlock()
	return layout.Inset{Left: 20, Right: 20, Top: 14, Bottom: 14}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
					layout.Flexed(1, s.muted(fmt.Sprintf("%d entries", len(logs)))),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return s.button(gtx, &s.clearLogBtn, icClear, "Clear log", btnSecondary)
					}),
				)
			}),
			layout.Rigid(spacer(0, 10)),
			layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
				gtx.Constraints.Min = gtx.Constraints.Max
				return box(gtx, s.pal.field, s.pal.border, 8, layout.UniformInset(12), func(gtx layout.Context) layout.Dimensions {
					gtx.Constraints.Min = gtx.Constraints.Max
					if len(logs) == 0 {
						return s.muted("Nothing logged yet.")(gtx)
					}
					return material.List(s.th, &s.logList).Layout(gtx, len(logs), func(gtx layout.Context, i int) layout.Dimensions {
						l := material.Caption(s.th, logs[i])
						l.Font.Typeface = "Go Mono"
						l.Color = s.pal.fg
						switch {
						case strings.Contains(logs[i], "ERROR"):
							l.Color = s.pal.err
						case strings.Contains(logs[i], " SKIP "), strings.Contains(logs[i], "WARN"):
							l.Color = s.pal.warn
						}
						return layout.Inset{Bottom: 2}.Layout(gtx, l.Layout)
					})
				})
			}),
		)
	})
}
