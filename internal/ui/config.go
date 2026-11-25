package ui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/denislee/media-import/internal/importer"
)

// settings are remembered between runs in $XDG_CONFIG_HOME/media-import.
type settings struct {
	Source       string          `json:"source"`
	Dest         string          `json:"dest"`
	DirPreset    string          `json:"dir_preset"`
	CustomDir    string          `json:"custom_dir"`
	NamePreset   string          `json:"name_preset"`
	CustomName   string          `json:"custom_name"`
	Kinds        map[string]bool `json:"kinds"`
	Move         bool            `json:"move"`
	SkipDups     bool            `json:"skip_dups"`
	DupCheck     string          `json:"dup_check"`    // importer.DupCheckFast or DupCheckFull
	DupAnyName   bool            `json:"dup_any_name"` // also detect copies under other names
	Verify       bool            `json:"verify"`
	FFprobe      bool            `json:"ffprobe"`
	MtimeCapture bool            `json:"mtime_capture"`
	DryRun       bool            `json:"dry_run"`
	Hidden       bool            `json:"hidden"`
}

func defaultSettings() settings {
	home, _ := os.UserHomeDir()
	return settings{
		Dest:       filepath.Join(home, "Pictures", "Imported"),
		DirPreset:  "0",
		CustomDir:  "{YYYY}/{MM}-{MMM}/{DD}",
		NamePreset: "0",
		CustomName: "{YYYY}{MM}{DD}_{hh}{mm}{ss}_{name}",
		Kinds:      map[string]bool{"Photos": true, "RAW": true, "Videos": true, "Audio": false},
		SkipDups:   true,
		DupCheck:   importer.DupCheckFast,
		FFprobe:    true,
	}
}

func configPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "media-import", "settings.json")
}

func loadSettings() settings {
	s := defaultSettings()
	if p := configPath(); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			_ = json.Unmarshal(b, &s)
		}
	}
	return s
}

// writeFileAtomic writes b to p by way of p.tmp and a rename. A crash mid-write
// leaves the old file (or none), never a truncated one; a leftover p.tmp is
// ignored by the loaders.
func writeFileAtomic(p string, b []byte) error {
	tmp := p + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// instanceLock takes the per-user lock that keeps a second window from sharing
// (and wiping) the resume journal and resume.json.
func instanceLock(dir string) (*os.File, error) {
	return importer.LockFile(filepath.Join(dir, "lock"))
}

func saveSettings(s settings) error {
	p := configPath()
	if p == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(p, b)
}

// resumeMeta describes an import that can be resumed. It is written when an
// import starts and removed when it completes; the matching journal records
// per-file progress.
type resumeMeta struct {
	Settings     settings  `json:"settings"`
	Source       string    `json:"source"`      // absolute source directory
	OldSources   []string  `json:"old_sources"` // earlier mount paths of the same source
	Dest         string    `json:"dest"`
	DirTemplate  string    `json:"dir_template"`
	NameTemplate string    `json:"name_template"`
	Started      time.Time `json:"started"`
}

func stateDir() string {
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "media-import")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "state", "media-import")
}

func journalPath() string { return filepath.Join(stateDir(), "journal") }
func resumePath() string  { return filepath.Join(stateDir(), "resume.json") }

func saveResumeMeta(m resumeMeta) error {
	if err := os.MkdirAll(stateDir(), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(resumePath(), b)
}

// loadResume returns the interrupted import, if any.
func loadResume() (*resumeMeta, *importer.ResumeState) {
	b, err := os.ReadFile(resumePath())
	if err != nil {
		return nil, nil
	}
	var m resumeMeta
	if json.Unmarshal(b, &m) != nil {
		return nil, nil
	}
	rs, err := importer.ReadJournal(journalPath())
	if err != nil {
		return nil, nil
	}
	return &m, rs
}

func clearResume() {
	os.Remove(resumePath())
	os.Remove(journalPath())
}

// discardResume forgets the interrupted import rs describes and removes the
// temporary files its in-flight copies left in the destination (best effort).
func discardResume(rs *importer.ResumeState) {
	if rs != nil {
		for _, dst := range rs.Pending {
			_ = os.Remove(importer.PartPath(dst))
		}
	}
	clearResume()
}
