// Copyright (c) 2026 Ruthlessly Practical LLC. All rights reserved.
// Use of this source code is governed by a BSD-3-Clause license
// that can be found in the LICENSE file.

// Package logging provides structured logging setup for all Hippocampus binaries.
package logging

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/ruthlesslypractical/hippocampus/internal/config"
)

// Setup initializes slog for a specific module (e.g. "daemon", "hook", "summarize").
// It creates log files and sets the default logger.
// Returns a cleanup function to close file handles.
func Setup(cfg config.Config, module string) func() {
	logDir := cfg.ResolveLogDir()
	os.MkdirAll(logDir, 0755)

	// Parse level
	var level slog.Level
	switch strings.ToLower(cfg.Log.Level) {
	case "debug":
		level = slog.LevelDebug
	case "info":
		level = slog.LevelInfo
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	var writers []io.Writer
	var closers []io.Closer

	// Main log file (at configured level)
	mainLogPath := filepath.Join(logDir, module+".log")
	mainFile, err := os.OpenFile(mainLogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err == nil {
		writers = append(writers, mainFile)
		closers = append(closers, mainFile)
	}

	// Stderr output
	if cfg.Log.AlsoStderr {
		writers = append(writers, os.Stderr)
	}

	// Set up main handler
	var mainWriter io.Writer
	if len(writers) == 1 {
		mainWriter = writers[0]
	} else if len(writers) > 1 {
		mainWriter = io.MultiWriter(writers...)
	} else {
		mainWriter = os.Stderr
	}

	mainHandler := slog.NewTextHandler(mainWriter, &slog.HandlerOptions{
		Level: level,
	})
	slog.SetDefault(slog.New(mainHandler))

	// Debug file (separate, always at debug level)
	if cfg.Log.DebugFile {
		debugLogPath := filepath.Join(logDir, module+"-debug.log")
		debugFile, err := os.OpenFile(debugLogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
		if err == nil {
			closers = append(closers, debugFile)
			// Replace the default handler with a multi-handler that writes
			// to both the main output (at configured level) and debug file (at debug level)
			debugHandler := slog.NewTextHandler(debugFile, &slog.HandlerOptions{
				Level: slog.LevelDebug,
			})
			slog.SetDefault(slog.New(newMultiHandler(mainHandler, debugHandler)))
		}
	}

	// Return cleanup function
	return func() {
		for _, c := range closers {
			c.Close()
		}
	}
}

// multiHandler fans out log records to multiple handlers.
type multiHandler struct {
	handlers []slog.Handler
}

func newMultiHandler(handlers ...slog.Handler) *multiHandler {
	return &multiHandler{handlers: handlers}
}

func (h *multiHandler) Enabled(_ context.Context, level slog.Level) bool {
	for _, handler := range h.handlers {
		if handler.Enabled(context.Background(), level) {
			return true
		}
	}
	return false
}

func (h *multiHandler) Handle(ctx context.Context, r slog.Record) error {
	for _, handler := range h.handlers {
		if handler.Enabled(ctx, r.Level) {
			if err := handler.Handle(ctx, r); err != nil {
				return err
			}
		}
	}
	return nil
}

func (h *multiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	newHandlers := make([]slog.Handler, len(h.handlers))
	for i, handler := range h.handlers {
		newHandlers[i] = handler.WithAttrs(attrs)
	}
	return &multiHandler{handlers: newHandlers}
}

func (h *multiHandler) WithGroup(name string) slog.Handler {
	newHandlers := make([]slog.Handler, len(h.handlers))
	for i, handler := range h.handlers {
		newHandlers[i] = handler.WithGroup(name)
	}
	return &multiHandler{handlers: newHandlers}
}
