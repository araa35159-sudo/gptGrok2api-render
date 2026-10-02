package config

import "testing"

func TestImageConcurrencyAcceptsSixteenAndRespectsExplicitSettings(t *testing.T) {
	for _, tc := range []struct {
		name, total, account   string
		wantTotal, wantAccount int
	}{
		{"defaults", "", "", 16, 16},
		{"sixteen_is_not_clamped_to_four", "16", "16", 16, 16},
		{"explicit_lower_limits", "4", "2", 4, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GO_CONFIG_PATH", t.TempDir()+"/config.json")
			t.Setenv("GO_IMAGE_MAX_CONCURRENCY", tc.total)
			t.Setenv("GO_IMAGE_ACCOUNT_CONCURRENCY", tc.account)
			cfg, err := Load(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if cfg.ImageMaxConcurrency != tc.wantTotal || cfg.ImageAccountLimit != tc.wantAccount {
				t.Fatalf("concurrency = %d/%d, want %d/%d", cfg.ImageMaxConcurrency, cfg.ImageAccountLimit, tc.wantTotal, tc.wantAccount)
			}
		})
	}
}

func TestListenAddrUsesRailwayPortUnlessExplicitlyConfigured(t *testing.T) {
	t.Setenv("GO_CONFIG_PATH", t.TempDir()+"/config.json")
	t.Setenv("PORT", "31234")
	t.Setenv("GO_LISTEN_ADDR", "")
	t.Setenv("CHATGPT2API_LISTEN_ADDR", "")

	cfg, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr != ":31234" {
		t.Fatalf("expected Railway port, got %q", cfg.ListenAddr)
	}

	t.Setenv("GO_LISTEN_ADDR", ":10000")
	cfg, err = Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr != ":10000" {
		t.Fatalf("expected explicit listen address, got %q", cfg.ListenAddr)
	}
}
