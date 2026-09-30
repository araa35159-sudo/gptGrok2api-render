package config

import "testing"

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
