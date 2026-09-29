package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBangumiWatchConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name, body          string
		add, private        bool
		threshold, keywords int
	}{
		{"defaults", "", true, true, 85, 0},
		{"configured", "auto_add_collection = false\nprivate_collection = false\nmin_progress_percent = 92\nblock_keywords = ['trailer']", false, false, 92, 1},
		{"invalid threshold", "min_progress_percent = 101", true, true, 85, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte("[BangumiSync]\n"+tc.body), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.BangumiAutoAddCollection != tc.add || cfg.BangumiPrivateCollection != tc.private || cfg.BangumiMinProgressPercent != tc.threshold || len(cfg.BangumiBlockKeywords) != tc.keywords {
				t.Fatal("Bangumi configuration not applied")
			}
		})
	}
}
