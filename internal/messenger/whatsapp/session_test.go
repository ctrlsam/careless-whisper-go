package whatsapp

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestSessionPathForNumber(t *testing.T) {
	path, err := SessionPathForNumber("/sessions", "+61 415-883-160")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := filepath.Join("/sessions", "61415883160.db"); path != want {
		t.Fatalf("got %q, want %q", path, want)
	}
	if _, err := SessionPathForNumber("/sessions", "not-a-number"); err == nil {
		t.Fatal("expected an error for a non-numeric account")
	}
}

func TestDefaultSessionDirHonoursEnv(t *testing.T) {
	t.Setenv("CARELESS_WHISPER_HOME", "/tmp/cw-home")
	if got := DefaultSessionDir(); got != "/tmp/cw-home" {
		t.Fatalf("got %q, want /tmp/cw-home", got)
	}
}

func TestListSessionsExcludesSidecars(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"61400000001.db", "61400000002.db", "61400000001.db-wal", "61400000001.db-shm", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ListSessions(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(dir, "61400000001.db"), filepath.Join(dir, "61400000002.db")}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestListSessionsMissingDir(t *testing.T) {
	got, err := ListSessions(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %v, want empty", got)
	}
}
