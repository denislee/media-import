package organize

import (
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
