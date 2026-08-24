package backtest

import (
	"context"
	"testing"
	"time"

	"trading-bot/internal/domain"
	exchange "trading-bot/internal/exchange/binance"

	"github.com/shopspring/decimal"
)

func testInfo() *exchange.SymbolInfo {
	return &exchange.SymbolInfo{
		Symbol:      "BTCUSDT",
		TickSize:    decimal.RequireFromString("0.01"),
		StepSize:    decimal.RequireFromString("0.001"),
		MinQty:      decimal.RequireFromString("0.001"),
		MinNotional: decimal.RequireFromString("10"),
		PricePrec:   2,
		QtyPrec:     3,
	}
}

func mkCandle(closePrice, high, low float64, t time.Time) domain.Candle {
	c := decimal.NewFromFloat(closePrice)
	return domain.Candle{
		Symbol:    "BTCUSDT",
		OpenTime:  t,
		CloseTime: t,
		Open:      c,
		High:      decimal.NewFromFloat(high),
		Low:       decimal.NewFromFloat(low),
		Close:     c,
		Volume:    decimal.NewFromInt(1),
		IsClosed:  true,
	}
}

func TestSimExecutor_StopFillComputesPnLAndTrade(t *testing.T) {
	sim := NewSimExecutor("BTCUSDT", testInfo(), decimal.NewFromInt(1000), decimal.Zero, decimal.Zero)
	ctx := context.Background()
	now := time.Now()

	// Свеча входа: close=100.
	if ev := sim.OnCandle(mkCandle(100, 100, 100, now)); ev != nil {
		t.Fatalf("до открытия позиции OnCandle не должен возвращать событие, получено %+v", ev)
	}

	entry, err := sim.OpenLong(ctx, decimal.NewFromInt(1))
	if err != nil {
		t.Fatalf("OpenLong вернул ошибку: %v", err)
	}
	if !entry.Equal(decimal.NewFromInt(100)) {
		t.Fatalf("ожидалась цена входа 100, получено %s", entry)
	}

	if err := sim.PlaceStopLoss(ctx, decimal.NewFromInt(90)); err != nil {
		t.Fatalf("PlaceStopLoss: %v", err)
	}
	if err := sim.PlaceTakeProfit(ctx, decimal.NewFromInt(120)); err != nil {
		t.Fatalf("PlaceTakeProfit: %v", err)
	}

	// Следующая свеча пробивает стоп (low=85 < 90), тейк не задет.
	ev := sim.OnCandle(mkCandle(92, 95, 85, now.Add(time.Minute)))
	if ev == nil {
		t.Fatal("ожидалось событие срабатывания стопа")
	}
	if ev.Type != "STOP_MARKET" || ev.Status != "FILLED" {
		t.Fatalf("неожиданное событие: %+v", ev)
	}
	if !ev.AvgPrice.Equal(decimal.NewFromInt(90)) {
		t.Fatalf("фил должен быть ровно по цене триггера 90, получено %s", ev.AvgPrice)
	}
	if !ev.RealizedPnL.Equal(decimal.NewFromInt(-10)) {
		t.Fatalf("ожидался PnL -10 (1*(90-100)), получено %s", ev.RealizedPnL)
	}

	pos, _ := sim.Position(ctx)
	if !pos.IsFlat() {
		t.Fatalf("после стопа позиция должна быть плоской, получено %s", pos.Amount)
	}

	trades := sim.Trades()
	if len(trades) != 1 {
		t.Fatalf("ожидалась 1 сделка, получено %d", len(trades))
	}
	tr := trades[0]
	if !tr.PnL.Equal(decimal.NewFromInt(-10)) {
		t.Fatalf("PnL сделки: получено %s, ожидалось -10", tr.PnL)
	}
	if !tr.RiskDollars.Equal(decimal.NewFromInt(10)) {
		t.Fatalf("риск сделки: получено %s, ожидалось 10 (|100-90|*1)", tr.RiskDollars)
	}
	if tr.ExitReason != "STOP_MARKET" {
		t.Fatalf("причина выхода: получено %s", tr.ExitReason)
	}

	equity, _ := sim.Equity(ctx)
	if !equity.Equal(decimal.NewFromInt(990)) {
		t.Fatalf("эквити после убытка: получено %s, ожидалось 990", equity)
	}
}

func TestSimExecutor_FeesDeductedFromBothSides(t *testing.T) {
	sim := NewSimExecutor("BTCUSDT", testInfo(), decimal.NewFromInt(1000), decimal.NewFromFloat(0.001), decimal.Zero) // 0.1% за сторону
	ctx := context.Background()
	now := time.Now()

	sim.OnCandle(mkCandle(100, 100, 100, now))
	sim.OpenLong(ctx, decimal.NewFromInt(1)) // вход: комиссия = 1*100*0.001 = 0.1
	sim.PlaceStopLoss(ctx, decimal.NewFromInt(90))

	ev := sim.OnCandle(mkCandle(92, 95, 85, now.Add(time.Minute))) // выход по стопу 90: комиссия = 1*90*0.001 = 0.09
	if ev == nil {
		t.Fatal("ожидалось срабатывание стопа")
	}
	// Валовой PnL = 1*(90-100) = -10. Минус комиссии 0.1+0.09 = -10.19.
	want := decimal.NewFromFloat(-10.19)
	if !ev.RealizedPnL.Equal(want) {
		t.Fatalf("PnL с учётом комиссий: получено %s, ожидалось %s", ev.RealizedPnL, want)
	}
}

func TestSimExecutor_TakeProfitFillsBeforeStopWhenOnlyTakeCrossed(t *testing.T) {
	sim := NewSimExecutor("BTCUSDT", testInfo(), decimal.NewFromInt(1000), decimal.Zero, decimal.Zero)
	ctx := context.Background()
	now := time.Now()

	sim.OnCandle(mkCandle(100, 100, 100, now))
	sim.OpenLong(ctx, decimal.NewFromInt(2))
	sim.PlaceStopLoss(ctx, decimal.NewFromInt(90))
	sim.PlaceTakeProfit(ctx, decimal.NewFromInt(120))

	ev := sim.OnCandle(mkCandle(122, 125, 105, now.Add(time.Minute)))
	if ev == nil || ev.Type != "TAKE_PROFIT_MARKET" {
		t.Fatalf("ожидалось срабатывание тейка, получено %+v", ev)
	}
	if !ev.RealizedPnL.Equal(decimal.NewFromInt(40)) { // 2*(120-100)
		t.Fatalf("ожидался PnL 40, получено %s", ev.RealizedPnL)
	}
}

func TestSimExecutor_ClosePositionMarketRecordsManualTrade(t *testing.T) {
	sim := NewSimExecutor("BTCUSDT", testInfo(), decimal.NewFromInt(1000), decimal.Zero, decimal.Zero)
	ctx := context.Background()
	now := time.Now()

	sim.OnCandle(mkCandle(100, 100, 100, now))
	sim.OpenLong(ctx, decimal.NewFromInt(1))

	if err := sim.ClosePositionMarket(ctx); err != nil {
		t.Fatalf("ClosePositionMarket: %v", err)
	}

	trades := sim.Trades()
	if len(trades) != 1 || trades[0].ExitReason != "MANUAL_CLOSE" {
		t.Fatalf("ожидалась 1 ручная сделка, получено %+v", trades)
	}
	pos, _ := sim.Position(ctx)
	if !pos.IsFlat() {
		t.Fatal("после ручного закрытия позиция должна быть плоской")
	}
}

func TestSimExecutor_SlippageWorksAgainstTrader(t *testing.T) {
	// 1% проскальзывания — крупное намеренно, чтобы эффект был однозначно
	// виден на круглых числах, а не потерялся в округлении.
	sim := NewSimExecutor("BTCUSDT", testInfo(), decimal.NewFromInt(1000), decimal.Zero, decimal.NewFromFloat(0.01))
	ctx := context.Background()
	now := time.Now()

	sim.OnCandle(mkCandle(100, 100, 100, now))
	entry, err := sim.OpenLong(ctx, decimal.NewFromInt(1))
	if err != nil {
		t.Fatalf("OpenLong: %v", err)
	}
	// Вход в лонг — BUY, слипается вверх: 100*1.01 = 101.
	if !entry.Equal(decimal.NewFromInt(101)) {
		t.Fatalf("вход с проскальзыванием: получено %s, ожидалось 101", entry)
	}

	sim.PlaceStopLoss(ctx, decimal.NewFromInt(90))
	ev := sim.OnCandle(mkCandle(85, 92, 85, now.Add(time.Minute)))
	if ev == nil {
		t.Fatal("ожидалось срабатывание стопа")
	}
	// Закрытие лонга — SELL, слипается вниз: 90*0.99 = 89.1.
	if !ev.AvgPrice.Equal(decimal.NewFromFloat(89.1)) {
		t.Fatalf("выход с проскальзыванием: получено %s, ожидалось 89.1", ev.AvgPrice)
	}
	// PnL = 1*(89.1-101) = -11.9, хуже, чем -10 без проскальзывания.
	if !ev.RealizedPnL.Equal(decimal.NewFromFloat(-11.9)) {
		t.Fatalf("PnL с проскальзыванием: получено %s, ожидалось -11.9", ev.RealizedPnL)
	}
}
