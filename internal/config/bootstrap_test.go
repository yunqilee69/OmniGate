package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadBootstrapDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cfg.yaml")
	boot, err := LoadBootstrap(path)
	if err != nil {
		t.Fatalf("load bootstrap: %v", err)
	}
	if boot.Server.Host != defaultHost || boot.Server.Port != defaultPort {
		t.Fatalf("server = %s:%d, want %s:%d", boot.Server.Host, boot.Server.Port, defaultHost, defaultPort)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("default config not written: %v", err)
	}
}

func TestLoadBootstrapPasswordMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cfg.yaml")
	yaml := "server:\n  host: 0.0.0.0\n  port: 8080\nadmin:\n  username: admin\n  password: s3cret\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	boot, err := LoadBootstrap(path)
	if err != nil {
		t.Fatalf("load bootstrap: %v", err)
	}
	if boot.Admin.Username != "admin" || boot.Admin.Password != "s3cret" {
		t.Fatalf("admin creds = %+v", boot.Admin)
	}
}

func TestLoadBootstrapAdminValidation(t *testing.T) {
	for _, tc := range []struct {
		name, yaml, wantErr string
	}{
		{"colon in username", "admin:\n  username: ad:min\n  password: pw\n", "冒号"},
		{"missing password", "admin:\n  username: admin\n", "password 为空"},
	} {
		path := filepath.Join(t.TempDir(), "cfg.yaml")
		if err := os.WriteFile(path, []byte(tc.yaml), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := LoadBootstrap(path)
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Fatalf("%s: err = %v, want contains %q", tc.name, err, tc.wantErr)
		}
	}
}

func TestLoadBootstrapDatabasePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cfg.yaml")
	yaml := "database:\n  path: /custom/db/path.db\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	boot, err := LoadBootstrap(path)
	if err != nil {
		t.Fatalf("load bootstrap: %v", err)
	}
	if boot.Database.Path != "/custom/db/path.db" {
		t.Fatalf("database.path = %q, want /custom/db/path.db", boot.Database.Path)
	}
}

func TestLoadBootstrapLogPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cfg.yaml")
	yaml := "log:\n  path: /var/log/omnigate.log\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	boot, err := LoadBootstrap(path)
	if err != nil {
		t.Fatalf("load bootstrap: %v", err)
	}
	if boot.Log.Path != "/var/log/omnigate.log" {
		t.Fatalf("log.path = %q, want /var/log/omnigate.log", boot.Log.Path)
	}
}

func TestLoadBootstrapEmptyPaths(t *testing.T) {
	// 未设置 database.path / log.path 时，应为空字符串（由调用方填入默认值）
	path := filepath.Join(t.TempDir(), "cfg.yaml")
	boot, err := LoadBootstrap(path)
	if err != nil {
		t.Fatalf("load bootstrap: %v", err)
	}
	if boot.Database.Path != "" {
		t.Fatalf("database.path = %q, want empty", boot.Database.Path)
	}
	if boot.Log.Path != "" {
		t.Fatalf("log.path = %q, want empty", boot.Log.Path)
	}
}

func TestLoadBootstrapEnvOverrideDBPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cfg.yaml")
	yaml := "database:\n  path: /from/yaml.db\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DATABASE_PATH", "/from/env.db")
	boot, err := LoadBootstrap(path)
	if err != nil {
		t.Fatalf("load bootstrap: %v", err)
	}
	if boot.Database.Path != "/from/env.db" {
		t.Fatalf("database.path = %q, want /from/env.db (env overrides yaml)", boot.Database.Path)
	}
}

func TestLoadBootstrapEnvOverrideLogPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cfg.yaml")
	yaml := "log:\n  path: /from/yaml.log\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOG_PATH", "/from/env.log")
	boot, err := LoadBootstrap(path)
	if err != nil {
		t.Fatalf("load bootstrap: %v", err)
	}
	if boot.Log.Path != "/from/env.log" {
		t.Fatalf("log.path = %q, want /from/env.log (env overrides yaml)", boot.Log.Path)
	}
}
