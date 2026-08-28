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
	if boot.Server.Listen != defaultListen {
		t.Fatalf("listen = %s, want %s", boot.Server.Listen, defaultListen)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("default config not written: %v", err)
	}
}

func TestLoadBootstrapPasswordMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cfg.yaml")
	yaml := "server:\n  listen: 0.0.0.0:8080\nadmin:\n  username: admin\n  password: s3cret\n"
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
