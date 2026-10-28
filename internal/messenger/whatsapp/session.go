package whatsapp

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// DefaultSessionDir is the directory session stores live in by default. It is
// $CARELESS_WHISPER_HOME when set, otherwise ~/.careless-whisper, falling back
// to the working directory when the home directory cannot be determined. Using
// a fixed per-user directory means runs find the same sessions regardless of
// the working directory.
func DefaultSessionDir() string {
	if dir := strings.TrimSpace(os.Getenv("CARELESS_WHISPER_HOME")); dir != "" {
		return dir
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".careless-whisper")
	}
	return "."
}

// NormalizeNumber strips a leading + and separators from an international
// number, returning digits only. It is the exported form of the internal
// normaliser used for session file names.
func NormalizeNumber(number string) (string, error) {
	return normalizePhoneNumber(number)
}

// SessionPathForNumber returns the session store path for an account inside
// dir, named by the account's normalised number: <dir>/<number>.db. Naming the
// file after the bound number makes it self-describing, in place of an opaque
// account index.
func SessionPathForNumber(dir, number string) (string, error) {
	normalised, err := normalizePhoneNumber(number)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, normalised+".db"), nil
}

// ListSessions returns the session store files (*.db) in dir, sorted. A missing
// directory yields an empty slice, not an error. The WAL sidecars (-wal, -shm)
// do not match *.db and are excluded.
func ListSessions(dir string) ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "*.db"))
	if err != nil {
		return nil, err
	}
	sort.Strings(matches)
	return matches, nil
}
