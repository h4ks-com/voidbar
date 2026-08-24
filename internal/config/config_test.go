package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "voidbar.toml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Auth.Registration != "closed" {
		t.Fatalf("default registration = %q", cfg.Auth.Registration)
	}
	if cfg.Server.Listen != "127.0.0.1:8080" {
		t.Fatalf("default listen = %q", cfg.Server.Listen)
	}
}

func TestLoadTOML(t *testing.T) {
	path := writeConfig(t, `
[server]
listen = "0.0.0.0:9000"
public_url = "https://voidbar.example.com"

[storage]
path = "/srv/voidbar"

[auth]
registration = "invite"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Listen != "0.0.0.0:9000" {
		t.Fatalf("listen = %q", cfg.Server.Listen)
	}
	if cfg.Storage.Path != "/srv/voidbar" {
		t.Fatalf("storage path = %q", cfg.Storage.Path)
	}
	if cfg.Auth.Registration != "invite" {
		t.Fatalf("registration = %q", cfg.Auth.Registration)
	}
	if got, want := cfg.MasterKeyPath(), filepath.Join("/srv/voidbar", "master.key"); got != want {
		t.Fatalf("master key path = %q, want %q", got, want)
	}
}

func TestLoadExplicitMasterKeyPath(t *testing.T) {
	path := writeConfig(t, `
[auth]
registration = "closed"
master_key_path = "/etc/voidbar.key"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MasterKeyPath() != "/etc/voidbar.key" {
		t.Fatalf("master key path = %q", cfg.MasterKeyPath())
	}
}

func TestEnvOverrides(t *testing.T) {
	path := writeConfig(t, `[server]
listen = "0.0.0.0:9000"
public_url = "http://x"
`)
	t.Setenv("VOIDBAR_SERVER_LISTEN", "127.0.0.1:9999")
	t.Setenv("VOIDBAR_AUTH_REGISTRATION", "open")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Listen != "127.0.0.1:9999" {
		t.Fatalf("listen = %q", cfg.Server.Listen)
	}
	if cfg.Auth.Registration != "open" {
		t.Fatalf("registration = %q", cfg.Auth.Registration)
	}
}

func TestInvalidRegistration(t *testing.T) {
	path := writeConfig(t, `[auth]
registration = "sometimes"
`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestInvalidPublicURL(t *testing.T) {
	path := writeConfig(t, `[server]
listen = ":1"
public_url = "ftp://nope"
`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestGatewayWSURL(t *testing.T) {
	cfg := Default()
	if got, want := cfg.GatewayWSURL(), "ws://127.0.0.1:8080/gateway"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	cfg.Server.PublicURL = "https://voidbar.example.com"
	if got, want := cfg.GatewayWSURL(), "wss://voidbar.example.com/gateway"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestMissingFileExplicit(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.toml")); err == nil {
		t.Fatal("expected error for missing explicit config")
	}
}
