package observability

import (
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
