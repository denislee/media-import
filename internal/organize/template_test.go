package organize

import (
	"strings"
	"testing"
	"time"
)

var f = Fields{
	Time: time.Date(2024, 1, 31, 23, 59, 5, 0, time.UTC),
	Kind: "Photos", Name: "IMG_1", Ext: "jpg", Source: "CARD",
}

func TestDirPresets(t *testing.T) {
	want := []string{"2024/01/31", "2024/2024-01-31", "2024-01-31", "2024/01", "2024/2024-01", "2024", "Photos/2024/01", ""}
	for i, p := range DirPresets {
		if got := Dir(p.Template, f); got != want[i] {
			t.Errorf("%s: got %q want %q", p.Name, got, want[i])
		}
	}
}

func TestDirIsContained(t *testing.T) {
	for _, tpl := range []string{"../../etc", "/abs/{YYYY}", "{source}/../..", "a//b/./c"} {
		got := Dir(tpl, f)
		if len(got) > 0 && (got[0] == '/' || got == ".." || len(got) >= 3 && got[:3] == "../") {
			t.Errorf("Dir(%q) = %q escapes destination", tpl, got)
		}
	}
	if got := Dir("a//b/./c", f); got != "a/b/c" {
		t.Errorf("got %q", got)
	}
}

// replacerExpand is the previous strings.NewReplacer implementation, kept as
// the reference expand must match exactly.
func replacerExpand(tpl string, f Fields) string {
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

func TestExpandMatchesReplacer(t *testing.T) {
	var tpls []string
	for _, p := range DirPresets {
		tpls = append(tpls, p.Template)
	}
	for _, p := range NamePresets {
		tpls = append(tpls, p.Template)
	}
	tpls = append(tpls,
		"{MMM}{MM}{M}", "{{YYYY}}", "{", "}{", "{name",
		"a{kind}/{source}_{ext}", "",
	)
	fields := []Fields{
		f,
		// A value that looks like a token must not be expanded twice.
		{Time: f.Time, Kind: f.Kind, Name: "{YYYY}", Ext: f.Ext, Source: f.Source},
	}
	for _, ff := range fields {
		for _, tpl := range tpls {
			got, want := expand(tpl, ff), replacerExpand(tpl, ff)
			if got != want {
				t.Errorf("expand(%q) = %q, want %q", tpl, got, want)
			}
		}
	}
}

func TestDirAndNameAllocs(t *testing.T) {
	allocs := testing.AllocsPerRun(100, func() {
		_ = Dir("{YYYY}/{MM}/{DD}", f)
		_ = FileName("{YYYY}{MM}{DD}_{hh}{mm}{ss}_{name}", f, ".jpg")
	})
	if allocs > 10 {
		t.Errorf("Dir+FileName allocate %v times per run, want <= 10", allocs)
	}
}

func BenchmarkDirAndName(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		Dir("{YYYY}/{MM}/{DD}", f)
		FileName("{name}", f, ".jpg")
	}
}

func TestFileName(t *testing.T) {
	cases := map[string]string{
		"":                            "IMG_1.jpg",
		"{name}":                      "IMG_1.jpg",
		"{YYYY}{MM}{DD}_{hh}{mm}{ss}": "20240131_235905.jpg",
		"{YYYY}-{MMM}/{name}":         "2024-Jan_IMG_1.jpg",
		"..":                          "IMG_1.jpg",
	}
	for tpl, want := range cases {
		if got := FileName(tpl, f, ".jpg"); got != want {
			t.Errorf("FileName(%q) = %q, want %q", tpl, got, want)
		}
	}
}
