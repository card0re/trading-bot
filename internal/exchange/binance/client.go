package binance

import (
	"context"
	"fmt"
	"log"
	"strings"

	"trading-bot/internal/config"

	"github.com/adshao/go-binance/v2"
	"github.com/adshao/go-binance/v2/futures"
)

// InitClient создаёт REST-клиента и проверяет аккаунт-вайд состояние: сеть
// и one-way режим позиций. Настройка плеча/типа маржи для конкретных
// символов — отдельно, через SetupSymbol (её вызывают в цикле по всем
// торгуемым символам, а не здесь).
//
// futures.UseTestnet — глобальная переменная библиотеки, её же читают
// WS-функции, поэтому выставляем её здесь, до запуска любых потоков.
func InitClient(ctx context.Context, cfg *config.AppConfig) (*futures.Client, error) {
	futures.UseTestnet = cfg.UseTestnet
	client := binance.NewFuturesClient(cfg.APIKey, cfg.SecretKey)

	if err := client.NewPingService().Do(ctx); err != nil {
		return nil, fmt.Errorf("пинг API: %w", err)
	}

	// Стратегия рассчитана на one-way режим: в hedge-режиме тот же ордер
	// создал бы отдельную позицию вместо закрытия текущей. Режим общий на
	// весь аккаунт, поэтому проверяется один раз, а не на символ.
	mode, err := client.NewGetPositionModeService().Do(ctx)
	if err != nil {
		return nil, fmt.Errorf("чтение режима позиций: %w", err)
	}
	if mode.DualSidePosition {
		return nil, fmt.Errorf("аккаунт в hedge-режиме; бот поддерживает только one-way (отключите Hedge Mode в настройках фьючерсов)")
	}

	return client, nil
}

// SetupSymbol приводит один символ к ожидаемому плечу и типу маржи.
// Вызывается по разу на каждый торгуемый символ после InitClient.
func SetupSymbol(ctx context.Context, client *futures.Client, symbol string, leverage int, marginType string) error {
	if err := setupMarginType(ctx, client, symbol, marginType); err != nil {
		return err
	}

	lev, err := client.NewChangeLeverageService().
		Symbol(symbol).
		Leverage(leverage).
		Do(ctx)
	if err != nil {
		return fmt.Errorf("установка плеча %dx для %s: %w", leverage, symbol, err)
	}
	log.Printf("⚙️  %s: плечо %dx, маржа %s", symbol, lev.Leverage, marginType)
	return nil
}

func setupMarginType(ctx context.Context, client *futures.Client, symbol, marginType string) error {
	// Сверяемся с текущим состоянием перед вызовом смены типа: сам endpoint
	// смены на testnet иногда падает с -4067 даже когда менять нечего
	// (баг биржи, воспроизводится и при запросе уже установленного типа),
	// поэтому надёжнее не звать его вовсе, если тип уже совпадает.
	risk, err := client.NewGetPositionRiskService().Symbol(symbol).Do(ctx)
	if err != nil {
		return fmt.Errorf("чтение текущего типа маржи для %s: %w", symbol, err)
	}
	for _, p := range risk {
		// PositionRisk отдаёт "cross"/"isolated" — не то же написание, что
		// ChangeMarginType ожидает на входе ("CROSSED"/"ISOLATED").
		current := futures.MarginTypeCrossed
		if strings.EqualFold(string(p.MarginType), "isolated") {
			current = futures.MarginTypeIsolated
		}
		if current == futures.MarginType(marginType) {
			return nil
		}
	}

	if err := client.NewChangeMarginTypeService().
		Symbol(symbol).
		MarginType(futures.MarginType(marginType)).
		Do(ctx); err != nil {
		// -4046 "No need to change margin type" — тип уже установлен, это не ошибка.
		if strings.Contains(err.Error(), "-4046") {
			return nil
		}
		return fmt.Errorf("установка типа маржи %s для %s: %w", marginType, symbol, err)
	}
	return nil
}
