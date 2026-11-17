package ui

import (
	"image"
	"image/color"
	"time"

	"gioui.org/font"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"
	"golang.org/x/exp/shiny/materialdesign/icons"
)

// palette holds every colour the UI paints with.
type palette struct {
	bg, sidebar, surface, surfaceHi, field, border color.NRGBA
	fg, muted, faint                               color.NRGBA
	accent, accentFg, accentSoft                   color.NRGBA
	ok, warn, err, btn, knob                       color.NRGBA
}

func darkPalette() palette {
	accent := rgb(0x5b8def)
	return palette{
		bg: rgb(0x17181c), sidebar: rgb(0x1e1f25), surface: rgb(0x23252c), surfaceHi: rgb(0x2b2e37),
		field: rgb(0x1a1b20), border: rgb(0x343743),
		fg: rgb(0xe6e7eb), muted: rgb(0x9a9eab), faint: rgb(0x6b6f7b),
		accent: accent, accentFg: rgb(0xffffff), accentSoft: withAlpha(accent, 0x2c),
		ok: rgb(0x5fb878), warn: rgb(0xd8b25a), err: rgb(0xe06c75), btn: rgb(0x383c48), knob: rgb(0xffffff),
	}
}

func lightPalette() palette {
	accent := rgb(0x2f6fdf)
	return palette{
		bg: rgb(0xf3f4f7), sidebar: rgb(0xe8eaef), surface: rgb(0xffffff), surfaceHi: rgb(0xeef0f4),
		field: rgb(0xfbfbfc), border: rgb(0xd5d8df),
		fg: rgb(0x1d1f24), muted: rgb(0x5d626e), faint: rgb(0x8b909b),
		accent: accent, accentFg: rgb(0xffffff), accentSoft: withAlpha(accent, 0x1e),
		ok: rgb(0x2e9b52), warn: rgb(0xb5831a), err: rgb(0xc8434f), btn: rgb(0xdde1e8), knob: rgb(0xffffff),
	}
}

func mustIcon(data []byte) *widget.Icon {
	ic, err := widget.NewIcon(data)
	if err != nil {
		panic(err)
	}
	return ic
}

var (
	icCard     = mustIcon(icons.DeviceSDStorage)
	icPhone    = mustIcon(icons.HardwarePhoneAndroid)
	icUSB      = mustIcon(icons.DeviceUSB)
	icFolder   = mustIcon(icons.FileFolder)
	icFolderOp = mustIcon(icons.FileFolderOpen)
	icEject    = mustIcon(icons.ActionEject)
	icPhoto    = mustIcon(icons.ImagePhotoCamera)
	icRaw      = mustIcon(icons.ImageCameraRoll)
	icVideo    = mustIcon(icons.AVMovie)
	icAudio    = mustIcon(icons.ImageMusicNote)
	icPlay     = mustIcon(icons.AVPlayArrow)
	icPause    = mustIcon(icons.AVPause)
	icStop     = mustIcon(icons.AVStop)
	icHistory  = mustIcon(icons.ActionHistory)
	icCheck    = mustIcon(icons.ActionCheckCircle)
	icWarn     = mustIcon(icons.AlertWarning)
	icError    = mustIcon(icons.AlertError)
	icInfo     = mustIcon(icons.ActionInfo)
	icSettings = mustIcon(icons.ActionSettings)
	icList     = mustIcon(icons.ActionList)
	icDownload = mustIcon(icons.FileFileDownload)
	icFile     = mustIcon(icons.EditorInsertDriveFile)
	icNewDir   = mustIcon(icons.FileCreateNewFolder)
	icUp       = mustIcon(icons.NavigationArrowUpward)
	icHome     = mustIcon(icons.ActionHome)
	icStorage  = mustIcon(icons.DeviceStorage)
	icClear    = mustIcon(icons.ContentClear)
	icOrganize = mustIcon(icons.ImagePhotoLibrary)
)

// icon draws ic as a size×size square in colour c.
func icon(ic *widget.Icon, size unit.Dp, c color.NRGBA) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		sz := gtx.Dp(size)
		gtx.Constraints = layout.Exact(image.Pt(sz, sz))
		return ic.Layout(gtx, c)
	}
}

// box lays out w inside inset and paints a rounded background (and an
// optional 1 dp border) behind it. Zero-alpha colours are skipped.
func box(gtx layout.Context, bg, border color.NRGBA, radius unit.Dp, inset layout.Inset, w layout.Widget) layout.Dimensions {
	m := op.Record(gtx.Ops)
	dims := inset.Layout(gtx, w)
	call := m.Stop()
	r := image.Rectangle{Max: dims.Size}
	rr := gtx.Dp(radius)
	if bg.A > 0 {
		paint.FillShape(gtx.Ops, bg, clip.UniformRRect(r, rr).Op(gtx.Ops))
	}
	if border.A > 0 {
		paint.FillShape(gtx.Ops, border, clip.Stroke{Path: clip.UniformRRect(r, rr).Path(gtx.Ops), Width: float32(gtx.Dp(1))}.Op())
	}
	call.Add(gtx.Ops)
	return dims
}

// mix blends a toward b by t (0..1), keeping a's alpha.
func mix(a, b color.NRGBA, t float32) color.NRGBA {
	f := func(x, y uint8) uint8 { return uint8(float32(x) + (float32(y)-float32(x))*t) }
	return color.NRGBA{R: f(a.R, b.R), G: f(a.G, b.G), B: f(a.B, b.B), A: a.A}
}

func (s *state) dim(c color.NRGBA, gtx layout.Context) color.NRGBA {
	if gtx.Enabled() {
		return c
	}
	return withAlpha(c, uint8(int(c.A)*45/100))
}

type btnKind int

const (
	btnSecondary btnKind = iota
	btnPrimary
	btnDanger
	btnGhost
)

// button is a rounded button with an optional leading icon.
func (s *state) button(gtx layout.Context, c *widget.Clickable, ic *widget.Icon, label string, k btnKind) layout.Dimensions {
	gtx.Constraints.Min = image.Point{}
	bg, fg := s.pal.btn, s.pal.fg
	inset := layout.Inset{Top: 8, Bottom: 8, Left: 14, Right: 14}
	switch k {
	case btnPrimary:
		bg, fg = s.pal.accent, s.pal.accentFg
		inset = layout.Inset{Top: 10, Bottom: 10, Left: 18, Right: 20}
	case btnDanger:
		bg, fg = s.pal.err, rgb(0xffffff)
	case btnGhost:
		bg, fg = color.NRGBA{}, s.pal.muted
		inset = layout.UniformInset(7)
	}
	if label == "" && k != btnGhost {
		inset = layout.UniformInset(8)
	}
	if gtx.Enabled() && c.Hovered() {
		if k == btnGhost {
			bg, fg = withAlpha(s.pal.fg, 0x18), s.pal.fg
		} else {
			bg = mix(bg, rgb(0xffffff), 0.12)
		}
	}
	bg, fg = s.dim(bg, gtx), s.dim(fg, gtx)
	return c.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return box(gtx, bg, color.NRGBA{}, 7, inset, func(gtx layout.Context) layout.Dimensions {
			var ch []layout.FlexChild
			if ic != nil {
				ch = append(ch, layout.Rigid(icon(ic, 18, fg)))
				if label != "" {
					ch = append(ch, layout.Rigid(spacer(7, 0)))
				}
			}
			if label != "" {
				ch = append(ch, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					l := material.Body1(s.th, label)
					l.Color, l.Font.Weight, l.MaxLines = fg, font.Medium, 1
					return l.Layout(gtx)
				}))
			}
			return layout.Flex{Alignment: layout.Middle}.Layout(gtx, ch...)
		})
	})
}

// pill is one choice of an Enum drawn as a rounded chip.
func (s *state) pill(gtx layout.Context, e *widget.Enum, key, label string) layout.Dimensions {
	sel := e.Value == key
	hk, hov := e.Hovered()
	hov = hov && hk == key && gtx.Enabled()
	bg, border, fg := s.pal.surfaceHi, s.pal.border, s.pal.fg
	switch {
	case sel:
		bg, border = s.pal.accentSoft, s.pal.accent
	case hov:
		bg = mix(bg, s.pal.fg, 0.08)
	}
	bg, border, fg = s.dim(bg, gtx), s.dim(border, gtx), s.dim(fg, gtx)
	return e.Layout(gtx, key, func(gtx layout.Context) layout.Dimensions {
		gtx.Constraints.Min = image.Point{}
		return box(gtx, bg, border, 16, layout.Inset{Top: 6, Bottom: 6, Left: 12, Right: 12}, func(gtx layout.Context) layout.Dimensions {
			l := material.Body2(s.th, label)
			l.Color, l.MaxLines = fg, 1
			if sel {
				l.Font.Weight = font.SemiBold
			}
			return l.Layout(gtx)
		})
	})
}

// toggleSwitch draws an on/off switch (purely visual; the caller handles input).
func (s *state) toggleSwitch(on bool) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		w, h := gtx.Dp(38), gtx.Dp(22)
		track := s.pal.btn
		if on {
			track = s.pal.accent
		}
		paint.FillShape(gtx.Ops, s.dim(track, gtx), clip.UniformRRect(image.Rect(0, 0, w, h), h/2).Op(gtx.Ops))
		d := gtx.Dp(16)
		pad := (h - d) / 2
		x := pad
		if on {
			x = w - pad - d
		}
		paint.FillShape(gtx.Ops, s.dim(s.pal.knob, gtx), clip.Ellipse(image.Rect(x, pad, x+d, pad+d)).Op(gtx.Ops))
		return layout.Dimensions{Size: image.Pt(w, h)}
	}
}

// optionRow is a full-width clickable row: title, description and a switch.
func (s *state) optionRow(gtx layout.Context, b *widget.Bool, title, desc string) layout.Dimensions {
	b.Update(gtx)
	return b.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		gtx.Constraints.Min.X = gtx.Constraints.Max.X
		bg := color.NRGBA{}
		if b.Hovered() && gtx.Enabled() {
			bg = withAlpha(s.pal.fg, 0x0c)
		}
		return box(gtx, bg, color.NRGBA{}, 6, layout.Inset{Top: 7, Bottom: 7, Left: 8, Right: 8}, func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
				layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
					return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							l := material.Body1(s.th, title)
							l.Color = s.dim(s.pal.fg, gtx)
							return l.Layout(gtx)
						}),
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							if desc == "" {
								return layout.Dimensions{}
							}
							l := material.Caption(s.th, desc)
							l.Color = s.dim(s.pal.muted, gtx)
							return l.Layout(gtx)
						}),
					)
				}),
				layout.Rigid(spacer(12, 0)),
				layout.Rigid(s.toggleSwitch(b.Value)),
			)
		})
	})
}

// card is a titled surface panel. ic may be nil; an empty title drops the header.
func (s *state) card(gtx layout.Context, title string, ic *widget.Icon, body layout.Widget) layout.Dimensions {
	return s.cardRaw(gtx, func(gtx layout.Context) layout.Dimensions {
		if title == "" {
			return body(gtx)
		}
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				var ch []layout.FlexChild
				if ic != nil {
					ch = append(ch, layout.Rigid(icon(ic, 20, s.pal.accent)), layout.Rigid(spacer(10, 0)))
				}
				ch = append(ch, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					l := material.Subtitle1(s.th, title)
					l.Font.Weight = font.SemiBold
					return l.Layout(gtx)
				}))
				return layout.Flex{Alignment: layout.Middle}.Layout(gtx, ch...)
			}),
			layout.Rigid(spacer(0, 14)),
			layout.Rigid(body),
		)
	})
}

func (s *state) cardRaw(gtx layout.Context, body layout.Widget) layout.Dimensions {
	gtx.Constraints.Min.X = gtx.Constraints.Max.X
	return box(gtx, s.pal.surface, s.pal.border, 10, layout.UniformInset(18), body)
}

// banner is a tinted notice with an icon, a title, optional text and actions.
func (s *state) banner(gtx layout.Context, ic *widget.Icon, c color.NRGBA, title, body string, actions layout.Widget) layout.Dimensions {
	gtx.Constraints.Min.X = gtx.Constraints.Max.X
	return box(gtx, mix(s.pal.surface, c, 0.12), withAlpha(c, 0x70), 8, layout.UniformInset(14), func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{}.Layout(gtx,
			layout.Rigid(icon(ic, 22, c)),
			layout.Rigid(spacer(12, 0)),
			layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						l := material.Body1(s.th, title)
						l.Font.Weight = font.SemiBold
						return l.Layout(gtx)
					}),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						if body == "" {
							return layout.Dimensions{}
						}
						return layout.Inset{Top: 3}.Layout(gtx, s.muted(body))
					}),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						if actions == nil {
							return layout.Dimensions{}
						}
						return layout.Inset{Top: 10}.Layout(gtx, actions)
					}),
				)
			}),
		)
	})
}

// fieldLabel is the small heading above an input or group.
func (s *state) fieldLabel(txt string) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		return layout.Inset{Bottom: 6}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			l := material.Body2(s.th, txt)
			l.Color, l.Font.Weight = s.dim(s.pal.muted, gtx), font.SemiBold
			return l.Layout(gtx)
		})
	}
}

// stat is a statistic tile: small label above a large value.
func (s *state) stat(gtx layout.Context, k, v string, c color.NRGBA) layout.Dimensions {
	gtx.Constraints.Min.X = gtx.Constraints.Max.X
	return box(gtx, s.pal.surfaceHi, color.NRGBA{}, 8, layout.Inset{Top: 10, Bottom: 10, Left: 12, Right: 12}, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				l := material.Caption(s.th, k)
				l.Color, l.MaxLines = s.pal.muted, 1
				return l.Layout(gtx)
			}),
			layout.Rigid(spacer(0, 2)),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				l := material.H6(s.th, v)
				l.Color, l.Font.Weight, l.MaxLines = c, font.SemiBold, 1
				return l.Layout(gtx)
			}),
		)
	})
}

// bar draws a rounded progress bar of height h filling the available width,
// with an optional centred label. indeterminate animates a moving segment.
func (s *state) bar(gtx layout.Context, h unit.Dp, frac float32, fg color.NRGBA, label string, indeterminate bool) layout.Dimensions {
	size := image.Pt(gtx.Constraints.Max.X, gtx.Dp(h))
	rr := gtx.Dp(min(h/2, 8))
	track := clip.UniformRRect(image.Rectangle{Max: size}, rr).Push(gtx.Ops)
	paint.Fill(gtx.Ops, withAlpha(s.pal.fg, 0x1a))
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

func (s *state) pathRow(gtx layout.Context, ed *widget.Editor, hint string, browse *widget.Clickable) layout.Dimensions {
	return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions { return s.editor(gtx, ed, hint) }),
		layout.Rigid(spacer(8, 0)),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return s.button(gtx, browse, icFolderOp, "Browse…", btnSecondary)
		}),
	)
}

func (s *state) editor(gtx layout.Context, ed *widget.Editor, hint string) layout.Dimensions {
	gtx.Constraints.Min.X = gtx.Constraints.Max.X
	border := s.pal.border
	if gtx.Focused(ed) {
		border = s.pal.accent
	}
	return box(gtx, s.pal.field, s.dim(border, gtx), 7, layout.Inset{Top: 9, Bottom: 9, Left: 11, Right: 11}, func(gtx layout.Context) layout.Dimensions {
		e := material.Editor(s.th, ed, hint)
		e.HintColor = s.pal.faint
		e.Color = s.dim(s.pal.fg, gtx)
		return e.Layout(gtx)
	})
}

func (s *state) muted(txt string) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		l := material.Body2(s.th, txt)
		l.Color = s.dim(s.pal.muted, gtx)
		return l.Layout(gtx)
	}
}

// hline is a 1 dp full-width divider.
func (s *state) hline(gtx layout.Context) layout.Dimensions {
	size := image.Pt(gtx.Constraints.Max.X, max(1, gtx.Dp(1)))
	paint.FillShape(gtx.Ops, s.pal.border, clip.Rect{Max: size}.Op())
	return layout.Dimensions{Size: size}
}

// wrap lays out n items left-to-right, wrapping onto new rows.
func (s *state) wrap(gtx layout.Context, n int, item func(layout.Context, int) layout.Dimensions) layout.Dimensions {
	maxW := gtx.Constraints.Max.X
	gap := gtx.Dp(8)
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

// grid lays out n equal-width cells, as many per row as fit at minW each.
func grid(gtx layout.Context, n int, minW, gap unit.Dp, item func(layout.Context, int) layout.Dimensions) layout.Dimensions {
	if n == 0 {
		return layout.Dimensions{}
	}
	g := gtx.Dp(gap)
	cols := max(1, min(n, (gtx.Constraints.Max.X+g)/(gtx.Dp(minW)+g)))
	var rows []layout.FlexChild
	for start := 0; start < n; start += cols {
		if start > 0 {
			rows = append(rows, layout.Rigid(spacer(0, gap)))
		}
		rows = append(rows, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			var ch []layout.FlexChild
			for c := range cols {
				if c > 0 {
					ch = append(ch, layout.Rigid(spacer(gap, 0)))
				}
				i := start + c
				ch = append(ch, layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
					if i >= n {
						return layout.Dimensions{}
					}
					gtx.Constraints.Min.X = gtx.Constraints.Max.X
					return item(gtx, i)
				}))
			}
			return layout.Flex{}.Layout(gtx, ch...)
		}))
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx, rows...)
}

// centered limits w to maxW and centres it in the available width.
func centered(gtx layout.Context, maxW unit.Dp, w layout.Widget) layout.Dimensions {
	full := gtx.Constraints.Max.X
	mw := gtx.Dp(maxW)
	if full <= mw {
		return w(gtx)
	}
	off := (full - mw) / 2
	gtx.Constraints.Max.X = mw
	gtx.Constraints.Min.X = min(gtx.Constraints.Min.X, mw)
	st := op.Offset(image.Pt(off, 0)).Push(gtx.Ops)
	d := w(gtx)
	st.Pop()
	d.Size.X = full
	return d
}

func spacer(w, h unit.Dp) layout.Widget {
	return layout.Spacer{Width: w, Height: h}.Layout
}
