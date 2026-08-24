// Package logging настраивает глобальный slog-логгер один раз при старте
// каждого бота (cmd/bot, cmd/rotationbot) — оба хотят одно и то же:
// текстовый вывод с уровнем, управляемым LOG_LEVEL, без внешних зависимостей.
package logging

import (
	"log/slog"
	"os"
	"strings"
)

// Setup ставит glob-логгер по умолчанию с уровнем из LOG_LEVEL
// (debug|info|warn|error, по умолчанию info) и текстовым форматом
// (level=... msg=...) — не JSON: единственный читатель сейчас — человек
// через journalctl, а не агрегатор логов. Понадобится JSON — заменить
// обработчик здесь одной строкой, вызывающий код трогать не придётся.
func Setup() {
	level := levelFromEnv(os.Getenv("LOG_LEVEL"))
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level})))
}

func levelFromEnv(v string) slog.Level {
	switch strings.ToLower(v) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
