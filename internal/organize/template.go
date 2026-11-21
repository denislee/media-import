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

// expand substitutes tokens in tpl.
func expand(tpl string, f Fields) string {
	return string(appendExpand(make([]byte, 0, len(tpl)+32), tpl, f))
}

// tokens are tried in this order at each '{', so {MMM} wins over {MM}.
var tokens = [...]string{"{YYYY}", "{YY}", "{MMM}", "{MM}", "{DD}", "{hh}", "{mm}", "{ss}", "{kind}", "{ext}", "{name}", "{source}"}

// appendExpand appends tpl with its tokens substituted to b. A single pass
// keeps expand allocation-light, and because the output is never rescanned a
// value that itself looks like a token is left alone.
func appendExpand(b []byte, tpl string, f Fields) []byte {
	t := f.Time
	for i := 0; i < len(tpl); {
		if tpl[i] == '{' {
			matched := false
			for _, tok := range tokens {
				if !strings.HasPrefix(tpl[i:], tok) {
					continue
				}
				switch tok {
				case "{YYYY}":
					b = t.AppendFormat(b, "2006")
				case "{YY}":
					b = t.AppendFormat(b, "06")
				case "{MMM}":
					b = t.AppendFormat(b, "Jan")
				case "{MM}":
					b = t.AppendFormat(b, "01")
				case "{DD}":
					b = t.AppendFormat(b, "02")
				case "{hh}":
					b = t.AppendFormat(b, "15")
				case "{mm}":
					b = t.AppendFormat(b, "04")
				case "{ss}":
					b = t.AppendFormat(b, "05")
				case "{kind}":
					b = append(b, f.Kind...)
				case "{ext}":
					b = append(b, f.Ext...)
				case "{name}":
					b = append(b, f.Name...)
				case "{source}":
					b = append(b, f.Source...)
				}
				i += len(tok)
				matched = true
				break
			}
			if matched {
				continue
			}
		}
		b = append(b, tpl[i])
		i++
	}
	return b
}

// Dir expands a directory template into a safe relative path. Any attempt
// to escape the destination ("..", absolute paths) is neutralised.
func Dir(tpl string, f Fields) string {
	parts := strings.Split(expand(tpl, f), "/")
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
	base := strings.ReplaceAll(expand(tpl, f), "/", "_")
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
