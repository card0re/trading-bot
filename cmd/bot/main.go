package main

import (
	"context"
	"log"
	"os/signal"
	"syscall"
	"time"

	"trading-bot/internal/app"
	"trading-bot/internal/config"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.Println("🚀 Запуск торгового бота...")

	if err := run(); err != nil {
		log.Fatalf("❌ %v", err)
	}
	log.Println("👋 Бот остановлен")
}

func run() error {
	// ctx отменяется по Ctrl+C или SIGTERM и гасит все потоки разом.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if cfg.UseTestnet {
		log.Println("🧪 Режим TESTNET")
	} else {
		log.Println("💰 Режим БОЕВОЙ ТОРГОВЛИ — работа с реальными средствами")
	}

	// Настройка N символов — это N REST-вызовов подряд; 60с с запасом.
	startCtx, cancelStart := context.WithTimeout(ctx, 60*time.Second)
	runner, err := app.New(startCtx, cfg)
	cancelStart()
	if err != nil {
		return err
	}

	return runner.Run(ctx)
}
