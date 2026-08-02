package config

import "testing"

func TestLoadMockDefaults(t *testing.T) {
	t.Setenv("DEV_MOCK", "true")
	t.Setenv("GOOGLE_CLOUD_PROJECT", "")
	t.Setenv("MODEL_ID", "")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ModelID != "gemini-3.5-flash" {
		t.Fatalf("model = %q", cfg.ModelID)
	}
	if cfg.Location != "global" {
		t.Fatalf("location = %q", cfg.Location)
	}
	if !cfg.DevMock {
		t.Fatal("DevMock is false")
	}
}

func TestLoadRequiresProject(t *testing.T) {
	t.Setenv("DEV_MOCK", "false")
	t.Setenv("GOOGLE_CLOUD_PROJECT", "")

	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil")
	}
}

func TestLoadRejectsInvalidInteger(t *testing.T) {
	t.Setenv("DEV_MOCK", "true")
	t.Setenv("MAX_MESSAGES", "many")

	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil")
	}
}
