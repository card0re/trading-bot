package logging

import (
	"log/slog"
	"testing"
)

func TestLevelFromEnv(t *testing.T) {
	cases := map[string]slog.Level{
		"":        slog.LevelInfo,
		"info":    slog.LevelInfo,
		"garbage": slog.LevelInfo, // неизвестное значение — тихий откат на info, не падение
		"debug":   slog.LevelDebug,
		"DEBUG":   slog.LevelDebug, // регистр не важен
		"warn":    slog.LevelWarn,
		"warning": slog.LevelWarn,
		"error":   slog.LevelError,
	}
	for in, want := range cases {
		if got := levelFromEnv(in); got != want {
			t.Errorf("levelFromEnv(%q) = %v, хотел %v", in, got, want)
		}
	}
}
