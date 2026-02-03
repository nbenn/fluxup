/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package logging provides slog-based logging utilities for fluxup.
//
// This package bridges Go's standard slog with controller-runtime's logr interface,
// allowing the use of familiar log levels (DEBUG, INFO, WARN, ERROR) throughout
// the codebase while maintaining compatibility with the Kubernetes controller ecosystem.
package logging

import (
	"context"
	"io"
	"log/slog"
	"os"

	"github.com/go-logr/logr"
)

// Log levels following standard conventions.
// These map to slog's built-in levels.
const (
	LevelDebug = slog.LevelDebug // -4
	LevelInfo  = slog.LevelInfo  // 0
	LevelWarn  = slog.LevelWarn  // 4
	LevelError = slog.LevelError // 8
)

// Options configures the logger.
type Options struct {
	// Level sets the minimum log level. Defaults to LevelInfo.
	Level slog.Level

	// Development enables development mode with text output and debug level.
	Development bool

	// Output is the writer for log output. Defaults to os.Stderr.
	Output io.Writer
}

// NewLogger creates a new slog.Logger with the given options.
func NewLogger(opts Options) *slog.Logger {
	if opts.Output == nil {
		opts.Output = os.Stderr
	}

	level := opts.Level
	if opts.Development && level > LevelDebug {
		level = LevelDebug
	}

	handlerOpts := &slog.HandlerOptions{
		Level: level,
	}

	var handler slog.Handler
	if opts.Development {
		handler = slog.NewTextHandler(opts.Output, handlerOpts)
	} else {
		handler = slog.NewJSONHandler(opts.Output, handlerOpts)
	}

	return slog.New(handler)
}

// NewLogrLogger creates a logr.Logger backed by slog for use with controller-runtime.
// This is the bridge between slog and controller-runtime's logging system.
func NewLogrLogger(logger *slog.Logger) logr.Logger {
	return logr.FromSlogHandler(logger.Handler())
}

// FromContext extracts an *slog.Logger from the context.
// This retrieves the logger that controller-runtime stores in the context
// and converts it to slog for use in application code.
//
// If no logger is found in the context, returns the default slog logger.
func FromContext(ctx context.Context) *slog.Logger {
	logger := logr.FromContextAsSlogLogger(ctx)
	if logger == nil {
		return slog.Default()
	}
	return logger
}

// WithName returns a logger with an additional name segment.
// Names are joined with "/" to form a hierarchy like "controller/upgraderequest".
func WithName(logger *slog.Logger, name string) *slog.Logger {
	return logger.With("logger", name)
}

// WithValues returns a logger with additional key-value pairs attached.
func WithValues(logger *slog.Logger, keysAndValues ...any) *slog.Logger {
	return logger.With(keysAndValues...)
}
