// Package organize turns a file's metadata into a destination path using
// user-selectable templates.
package organize

import (
	"path/filepath"
	"strings"
	"time"
)

// Preset is a named template.
type Preset struct {
	Name     string
	Template string
}

// DirPresets are the built-in directory structures.
var DirPresets = []Preset{
	{"Year / Month / Day", "{YYYY}/{MM}/{DD}"},
	{"Year / Year-Month-Day", "{YYYY}/{YYYY}-{MM}-{DD}"},
	{"Year-Month-Day", "{YYYY}-{MM}-{DD}"},
	{"Year / Month", "{YYYY}/{MM}"},
	{"Year / Year-Month", "{YYYY}/{YYYY}-{MM}"},
	{"Year", "{YYYY}"},
	{"Type / Year / Month", "{kind}/{YYYY}/{MM}"},
	{"Flat (no subfolders)", ""},
}

// NamePresets are the built-in file naming schemes. An empty template keeps
// the original name.
var NamePresets = []Preset{
	{"Keep original name", "{name}"},
	{"Date_Time (20240131_235959)", "{YYYY}{MM}{DD}_{hh}{mm}{ss}"},
	{"Date_Time_Original", "{YYYY}{MM}{DD}_{hh}{mm}{ss}_{name}"},
	{"Date-Time (2024-01-31_23-59-59)", "{YYYY}-{MM}-{DD}_{hh}-{mm}-{ss}"},
}

// Tokens documents the available placeholders.
const Tokens = "{YYYY} {YY} {MM} {MMM} {DD} {hh} {mm} {ss} {kind} {ext} {name} {source}"

// Fields holds the values substituted into a template.
type Fields struct {
	Time   time.Time
	Kind   string // e.g. "Photos"
	Name   string // original base name without extension
	Ext    string // lower-case extension without dot
	Source string // name of the source volume / folder
}

// Expand substitutes tokens in tpl.
func Expand(tpl string, f Fields) string {
	t := f.Time
	r := strings.NewReplacer(
		"{YYYY}", t.Format("2006"),
		"{YY}", t.Format("06"),
		"{MMM}", t.Format("Jan"),
		"{MM}", t.Format("01"),
		"{DD}", t.Format("02"),
		"{hh}", t.Format("15"),
		"{mm}", t.Format("04"),
		"{ss}", t.Format("05"),
		"{kind}", f.Kind,
		"{ext}", f.Ext,
		"{name}", f.Name,
		"{source}", f.Source,
	)
	return r.Replace(tpl)
}

// Dir expands a directory template into a safe relative path. Any attempt
// to escape the destination ("..", absolute paths) is neutralised.
func Dir(tpl string, f Fields) string {
	parts := strings.Split(Expand(tpl, f), "/")
	clean := parts[:0]
	for _, p := range parts {
		p = sanitize(p)
		if p == "" || p == "." || p == ".." {
			continue
		}
		clean = append(clean, p)
	}
	return filepath.Join(clean...)
}

// FileName expands a name template and appends the original extension.
func FileName(tpl string, f Fields, origExt string) string {
	if strings.TrimSpace(tpl) == "" {
		tpl = "{name}"
	}
	base := strings.ReplaceAll(Expand(tpl, f), "/", "_")
	base = sanitize(base)
	if base == "" || base == "." || base == ".." {
		base = f.Name
	}
	return base + origExt
}

func sanitize(s string) string {
	s = strings.TrimSpace(s)
	return strings.Map(func(r rune) rune {
		switch {
		case r == 0, r == '\\':
			return '_'
		case r < 32:
			return -1
		}
		return r
	}, s)
}
