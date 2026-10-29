package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadHistoryConfigurationAndEnvironmentPrecedence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "history:\n  archive:\n    retention: 48h\n    max_records: 23\n    max_bytes: 4096\n  collector:\n    tick_interval: 2s\n    cycle_timeout: 3s\n    max_concurrency: 1\n    max_agents_per_cycle: 7\n    page_size: 8\n    max_pages: 2\n    max_run_continuations: 11\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONFIG_PATH", path)
	t.Setenv("SOLTI_AUTH_JWT_SECRET", strings.Repeat("x", 32))
	t.Setenv("SOLTI_HISTORY_ARCHIVE_MAX_RECORDS", "31")
	t.Setenv("SOLTI_HISTORY_COLLECTOR_PAGE_SIZE", "9")
	t.Setenv("SOLTI_HISTORY_COLLECTOR_MAX_RUN_CONTINUATIONS", "13")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.History.Archive.Retention != 48*time.Hour || cfg.History.Archive.MaxRecords != 31 || cfg.History.Archive.MaxBytes != 4096 {
		t.Fatalf("archive configuration did not preserve YAML and ENV priority: %+v", cfg.History.Archive)
	}
	collector := cfg.History.Collector
	if collector.TickInterval != 2*time.Second || collector.CycleTimeout != 3*time.Second || collector.MaxConcurrency != 1 || collector.MaxAgentsPerCycle != 7 || collector.PageSize != 9 || collector.MaxPages != 2 || collector.MaxRunContinuations != 13 {
		t.Fatalf("collector configuration did not preserve YAML and ENV priority: %+v", collector)
	}
}

func TestLoadRejectsInvalidArchiveAndCollectorBudgets(t *testing.T) {
	for _, tc := range []struct{ name, key, value, errorPath string }{
		{"retention", "SOLTI_HISTORY_ARCHIVE_RETENTION", "-1s", "config.history.archive"},
		{"record budget", "SOLTI_HISTORY_ARCHIVE_MAX_RECORDS", "-1", "config.history.archive"},
		{"payload budget", "SOLTI_HISTORY_ARCHIVE_MAX_BYTES", "-1", "config.history.archive"},
		{"concurrency", "SOLTI_HISTORY_COLLECTOR_MAX_CONCURRENCY", "33", "config.history.collector"},
		{"page budget", "SOLTI_HISTORY_COLLECTOR_MAX_PAGES", "65", "config.history.collector"},
		{"continuation budget", "SOLTI_HISTORY_COLLECTOR_MAX_RUN_CONTINUATIONS", "10001", "config.history.collector"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CONFIG_PATH", "")
			t.Setenv("SOLTI_AUTH_JWT_SECRET", strings.Repeat("x", 32))
			t.Setenv(tc.key, tc.value)
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), tc.errorPath) {
				t.Fatalf("want actionable %s error, got %v", tc.errorPath, err)
			}
		})
	}
}
