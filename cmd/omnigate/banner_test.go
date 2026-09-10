package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWebAccessURL_usesListenOverride(t *testing.T) {
	got := webAccessURL(filepath.Join(t.TempDir(), "missing.yaml"), "0.0.0.0:8080")
	want := "http://127.0.0.1:8080/manage"
	if got != want {
		t.Fatalf("webAccessURL override = %q, want %q", got, want)
	}
}

func TestWebAccessURL_readsConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := "server:\n  host: 192.168.1.8\n  port: 9000\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	got := webAccessURL(path, "")
	want := "http://192.168.1.8:9000/manage"
	if got != want {
		t.Fatalf("webAccessURL config = %q, want %q", got, want)
	}
}

func TestWebAccessURL_defaultsWhenMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	got := webAccessURL(path, "")
	want := "http://127.0.0.1:17777/manage"
	if got != want {
		t.Fatalf("webAccessURL missing = %q, want %q", got, want)
	}
}
