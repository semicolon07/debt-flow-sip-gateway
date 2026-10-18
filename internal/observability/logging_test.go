package observability

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRotatingWriterBoundsOperationalLogs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", "gateway.jsonl")
	writer, err := newRotatingWriter(path, 64, 2)
	if err != nil {
		t.Fatal(err)
	}
	for range 6 {
		if _, err := writer.Write([]byte(strings.Repeat("x", 31) + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []string{path, path + ".1", path + ".2"} {
		info, err := os.Stat(candidate)
		if err != nil {
			t.Fatalf("expected bounded log file %s: %v", candidate, err)
		}
		if info.Size() > 64 {
			t.Fatalf("log file %s exceeded bound: %d", candidate, info.Size())
		}
	}
}

func TestParseLevel(t *testing.T) {
	tests := map[string]slog.Level{
		"debug": slog.LevelDebug,
		"info":  slog.LevelInfo,
		"warn":  slog.LevelWarn,
		"error": slog.LevelError,
	}
	for input, want := range tests {
		t.Run(input, func(t *testing.T) {
			got, err := parseLevel(input)
			if err != nil || got != want {
				t.Fatalf("parseLevel(%q)=(%v, %v), want (%v, nil)", input, got, err, want)
			}
		})
	}
	if _, err := parseLevel("trace"); err == nil {
		t.Fatal("expected unsupported logging level to fail")
	}
}
