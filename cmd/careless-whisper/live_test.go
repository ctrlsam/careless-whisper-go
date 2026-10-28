package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestResolveSessionPath(t *testing.T) {
	t.Run("explicit path wins", func(t *testing.T) {
		got, err := resolveSessionPath("/explicit/cw.db", "/ignored", "61400000001")
		if err != nil || got != "/explicit/cw.db" {
			t.Fatalf("got %q, %v", got, err)
		}
	})
	t.Run("account resolves in dir", func(t *testing.T) {
		dir := t.TempDir()
		got, err := resolveSessionPath("", dir, "+61 400 000 001")
		if err != nil {
			t.Fatal(err)
		}
		if want := filepath.Join(dir, "61400000001.db"); got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})
	t.Run("sole session auto-selected", func(t *testing.T) {
		dir := t.TempDir()
		only := filepath.Join(dir, "61400000009.db")
		if err := os.WriteFile(only, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := resolveSessionPath("", dir, "")
		if err != nil || got != only {
			t.Fatalf("got %q, %v", got, err)
		}
	})
	t.Run("no sessions errors", func(t *testing.T) {
		if _, err := resolveSessionPath("", t.TempDir(), ""); err == nil {
			t.Fatal("expected an error when no sessions exist")
		}
	})
	t.Run("multiple sessions require account", func(t *testing.T) {
		dir := t.TempDir()
		for _, n := range []string{"61400000001.db", "61400000002.db"} {
			if err := os.WriteFile(filepath.Join(dir, n), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := resolveSessionPath("", dir, ""); err == nil {
			t.Fatal("expected an error when multiple sessions exist and no --account given")
		}
	})
}

func TestResolveAccountPaths(t *testing.T) {
	t.Run("number list resolves to files", func(t *testing.T) {
		dir := t.TempDir()
		got, err := resolveAccountPaths("", dir, "61400000001, 61400000002")
		if err != nil {
			t.Fatal(err)
		}
		want := []string{filepath.Join(dir, "61400000001.db"), filepath.Join(dir, "61400000002.db")}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})
	t.Run("explicit path is used as the single account", func(t *testing.T) {
		got, err := resolveAccountPaths("/explicit/cw.db", t.TempDir(), "")
		if err != nil || len(got) != 1 || got[0] != "/explicit/cw.db" {
			t.Fatalf("got %v, %v; want [/explicit/cw.db]", got, err)
		}
	})
	t.Run("sole session auto-selected", func(t *testing.T) {
		dir := t.TempDir()
		only := filepath.Join(dir, "61400000001.db")
		if err := os.WriteFile(only, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := resolveAccountPaths("", dir, "")
		if err != nil || len(got) != 1 || got[0] != only {
			t.Fatalf("got %v, %v", got, err)
		}
	})
	t.Run("multiple sessions require account", func(t *testing.T) {
		dir := t.TempDir()
		for _, n := range []string{"61400000001.db", "61400000002.db"} {
			if err := os.WriteFile(filepath.Join(dir, n), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := resolveAccountPaths("", dir, ""); err == nil {
			t.Fatal("expected an error when multiple sessions exist and no --account given")
		}
	})
	t.Run("nothing to use errors", func(t *testing.T) {
		if _, err := resolveAccountPaths("", t.TempDir(), ""); err == nil {
			t.Fatal("expected an error when no sessions and no --account")
		}
	})
}

