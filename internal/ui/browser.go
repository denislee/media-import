package ui

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"gioui.org/font"
	"gioui.org/layout"
	"gioui.org/widget"
	"gioui.org/widget/material"
)

// browser is an in-app folder picker (Sway has no portal file chooser by
// default, and Gio has no native dialog for directories).
type browser struct {
	target   *widget.Editor
	title    string
	allowNew bool

	dir     string
	entries []string
	clicks  []widget.Clickable
	err     string
	list    widget.List

	pathEd                                     widget.Editor
	newEd                                      widget.Editor
	upBtn, homeBtn, mediaBtn, rootBtn, goBtn   widget.Clickable
	chooseBtn, cancelBtn, newBtn, hiddenToggle widget.Clickable
	showHidden                                 bool
}

func newBrowser(target *widget.Editor, title string, allowNew bool) *browser {
	b := &browser{target: target, title: title, allowNew: allowNew}
	b.list.Axis = layout.Vertical
	b.pathEd.SingleLine, b.pathEd.Submit = true, true
	b.newEd.SingleLine, b.newEd.Submit = true, true

	start := expandHome(strings.TrimSpace(target.Text()))
	for start != "" && start != "/" {
		if st, err := os.Stat(start); err == nil && st.IsDir() {
			break
		}
		start = filepath.Dir(start)
	}
	if start == "" {
		start, _ = os.UserHomeDir()
	}
	b.open(start)
	return b
}

func (b *browser) open(dir string) {
	dir = filepath.Clean(dir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		b.err = err.Error()
		return
	}
	b.dir, b.err = dir, ""
	b.pathEd.SetText(dir)
	b.entries = b.entries[:0]
	for _, e := range entries {
		if !b.showHidden && strings.HasPrefix(e.Name(), ".") {
			continue
		}
		isDir := e.IsDir()
		if e.Type()&os.ModeSymlink != 0 {
			if st, err := os.Stat(filepath.Join(dir, e.Name())); err == nil {
				isDir = st.IsDir()
			}
		}
		if isDir {
			b.entries = append(b.entries, e.Name())
		}
	}
	sort.Slice(b.entries, func(i, j int) bool { return strings.ToLower(b.entries[i]) < strings.ToLower(b.entries[j]) })
	b.clicks = make([]widget.Clickable, len(b.entries))
	b.list.Position = layout.Position{}
}

func (b *browser) layout(gtx layout.Context, s *state) layout.Dimensions {
	for i := range b.clicks {
		if b.clicks[i].Clicked(gtx) {
			b.open(filepath.Join(b.dir, b.entries[i]))
			break
		}
	}
	if b.upBtn.Clicked(gtx) {
		b.open(filepath.Dir(b.dir))
	}
	if b.homeBtn.Clicked(gtx) {
		if h, err := os.UserHomeDir(); err == nil {
			b.open(h)
		}
	}
	if b.mediaBtn.Clicked(gtx) {
		p := filepath.Join("/run/media", os.Getenv("USER"))
		if _, err := os.Stat(p); err != nil {
			p = "/run/user/" + strconv.Itoa(os.Getuid()) + "/gvfs"
		}
		b.open(p)
	}
	if b.rootBtn.Clicked(gtx) {
		b.open("/")
	}
	if b.hiddenToggle.Clicked(gtx) {
		b.showHidden = !b.showHidden
		b.open(b.dir)
	}
	submitted := func(ed *widget.Editor) bool {
		for {
			ev, ok := ed.Update(gtx)
			if !ok {
				return false
			}
			if _, ok := ev.(widget.SubmitEvent); ok {
				return true
			}
		}
	}
	if b.goBtn.Clicked(gtx) || submitted(&b.pathEd) {
		b.open(expandHome(strings.TrimSpace(b.pathEd.Text())))
	}
	if b.newBtn.Clicked(gtx) || submitted(&b.newEd) {
		name := strings.TrimSpace(b.newEd.Text())
		if name != "" && !strings.ContainsAny(name, "/\x00") && name != "." && name != ".." {
			p := filepath.Join(b.dir, name)
			if err := os.MkdirAll(p, 0o755); err != nil {
				b.err = err.Error()
			} else {
				b.newEd.SetText("")
				b.open(p)
			}
		}
	}
	if b.chooseBtn.Clicked(gtx) {
		b.target.SetText(b.dir)
		s.browser = nil
	}
	if b.cancelBtn.Clicked(gtx) {
		s.browser = nil
	}

	th := s.th
	btn := func(c *widget.Clickable, label string) layout.FlexChild {
		return layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			bs := material.Button(th, c, label)
			bs.Background = rgb(0x50545e)
			bs.Inset = layout.UniformInset(8)
			return layout.Inset{Right: 6}.Layout(gtx, bs.Layout)
		})
	}
	hiddenLabel := "Show hidden"
	if b.showHidden {
		hiddenLabel = "Hide hidden"
	}

	return layout.UniformInset(16).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				l := material.H6(th, b.title)
				l.Font.Weight = font.Bold
				return l.Layout(gtx)
			}),
			layout.Rigid(spacer(0, 10)),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
					btn(&b.upBtn, "↑ Up"), btn(&b.homeBtn, "Home"), btn(&b.mediaBtn, "Removable media"),
					btn(&b.rootBtn, "/"), btn(&b.hiddenToggle, hiddenLabel),
				)
			}),
			layout.Rigid(spacer(0, 8)),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
					layout.Flexed(1, func(gtx layout.Context) layout.Dimensions { return s.editor(gtx, &b.pathEd, "/path") }),
					layout.Rigid(spacer(8, 0)),
					layout.Rigid(material.Button(th, &b.goBtn, "Go").Layout),
				)
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				if b.err == "" {
					return layout.Dimensions{}
				}
				l := material.Body2(th, b.err)
				l.Color = rgb(0xff6b6b)
				return layout.Inset{Top: 6}.Layout(gtx, l.Layout)
			}),
			layout.Rigid(spacer(0, 8)),
			layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
				return s.cardRaw(gtx, func(gtx layout.Context) layout.Dimensions {
					gtx.Constraints.Min.Y = gtx.Constraints.Max.Y
					if len(b.entries) == 0 {
						return s.muted("(no subfolders)")(gtx)
					}
					return material.List(th, &b.list).Layout(gtx, len(b.entries), func(gtx layout.Context, i int) layout.Dimensions {
						return material.Clickable(gtx, &b.clicks[i], func(gtx layout.Context) layout.Dimensions {
							gtx.Constraints.Min.X = gtx.Constraints.Max.X
							return layout.Inset{Top: 6, Bottom: 6, Left: 8}.Layout(gtx, material.Body1(th, "📁  "+b.entries[i]).Layout)
						})
					})
				})
			}),
			layout.Rigid(spacer(0, 10)),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				children := []layout.FlexChild{}
				if b.allowNew {
					children = append(children,
						layout.Flexed(1, func(gtx layout.Context) layout.Dimensions { return s.editor(gtx, &b.newEd, "New folder name") }),
						layout.Rigid(spacer(8, 0)),
						btn(&b.newBtn, "Create folder"),
					)
				} else {
					children = append(children, layout.Flexed(1, func(gtx layout.Context) layout.Dimensions { return layout.Dimensions{} }))
				}
				children = append(children,
					layout.Rigid(spacer(12, 0)),
					btn(&b.cancelBtn, "Cancel"),
					layout.Rigid(material.Button(th, &b.chooseBtn, "Use this folder").Layout),
				)
				return layout.Flex{Alignment: layout.Middle}.Layout(gtx, children...)
			}),
		)
	})
}
