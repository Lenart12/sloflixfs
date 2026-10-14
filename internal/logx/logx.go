// Package logx logs printf-style messages at slog levels through the default logger, so -log-level
// filters them.
package logx

import (
	"context"
	"fmt"
	"log/slog"
)

func Debugf(format string, args ...any) { logf(slog.LevelDebug, format, args...) }
func Infof(format string, args ...any)  { logf(slog.LevelInfo, format, args...) }
func Warnf(format string, args ...any)  { logf(slog.LevelWarn, format, args...) }
func Errorf(format string, args ...any) { logf(slog.LevelError, format, args...) }

func logf(l slog.Level, format string, args ...any) {
	ctx := context.Background()
	if slog.Default().Enabled(ctx, l) { // skip formatting filtered messages
		slog.Log(ctx, l, fmt.Sprintf(format, args...))
	}
}
