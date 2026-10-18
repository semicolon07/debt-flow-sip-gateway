package observability

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
)

type rotatingWriter struct {
	mu         sync.Mutex
	path       string
	maxBytes   int64
	maxBackups int
	file       *os.File
	size       int64
}

func NewLogger(path string, maxBytes int64, maxBackups int, level string) (*slog.Logger, io.Closer, error) {
	writer, err := newRotatingWriter(path, maxBytes, maxBackups)
	if err != nil {
		return nil, nil, err
	}
	configuredLevel, err := parseLevel(level)
	if err != nil {
		_ = writer.Close()
		return nil, nil, err
	}
	logger := slog.New(slog.NewJSONHandler(io.MultiWriter(os.Stdout, writer),
		&slog.HandlerOptions{Level: configuredLevel}))
	return logger, writer, nil
}

func parseLevel(value string) (slog.Level, error) {
	switch value {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, errors.New("invalid logging level")
	}
}

func newRotatingWriter(path string, maxBytes int64, maxBackups int) (*rotatingWriter, error) {
	if path == "" || maxBytes <= 0 || maxBackups <= 0 {
		return nil, errors.New("invalid rotating log configuration")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return &rotatingWriter{
		path: path, maxBytes: maxBytes, maxBackups: maxBackups, file: file, size: info.Size(),
	}, nil
}

func (w *rotatingWriter) Write(payload []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return 0, os.ErrClosed
	}
	if w.size > 0 && w.size+int64(len(payload)) > w.maxBytes {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}
	written, err := w.file.Write(payload)
	w.size += int64(written)
	return written, err
}

func (w *rotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

func (w *rotatingWriter) rotate() error {
	if err := w.file.Close(); err != nil {
		return err
	}
	for index := w.maxBackups; index >= 1; index-- {
		from := w.path
		if index > 1 {
			from += "." + strconv.Itoa(index-1)
		}
		to := w.path + "." + strconv.Itoa(index)
		if index == w.maxBackups {
			_ = os.Remove(to)
		}
		if err := os.Rename(from, to); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	file, err := os.OpenFile(w.path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	w.file = file
	w.size = 0
	return nil
}
