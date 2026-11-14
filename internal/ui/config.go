package ui

import (
	"encoding/json"
	"os"
	"path/filepath"
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
	return os.WriteFile(p, b, 0o644)
}
