package strategy

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"trading-bot/internal/domain"

	"github.com/shopspring/decimal"
)

// fakeExecutor — исполнитель в памяти. Позволяет проверять логику входов и
// выходов без сети и без биржи.
type fakeExecutor struct {
	mu sync.Mutex

	position   decimal.Decimal
	entryPrice decimal.Decimal

	openCalls   int
	stopCalls   int
	takeCalls   int
	cancelCalls int
	closeCalls  int

	lastStop decimal.Decimal
	lastTake decimal.Decimal

	openErr  error
	stopErr  error
	takeErr  error
	closeErr error
}

func (f *fakeExecutor) Symbol() string { return "BTCUSDT" }

func (f *fakeExecutor) OpenLong(_ context.Context, qty decimal.Decimal) (decimal.Decimal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.openCalls++
	if f.openErr != nil {
		return decimal.Zero, f.openErr
	}
	f.position = f.position.Add(qty)
	f.entryPrice = decimal.NewFromInt(100)
	return f.entryPrice, nil
}

func (f *fakeExecutor) OpenShort(_ context.Context, qty decimal.Decimal) (decimal.Decimal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.openCalls++
	if f.openErr != nil {
		return decimal.Zero, f.openErr
	}
	f.position = f.position.Sub(qty)
	f.entryPrice = decimal.NewFromInt(100)
	return f.entryPrice, nil
}

func (f *fakeExecutor) ClosePositionMarket(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.closeCalls++
	if f.closeErr != nil {
		return f.closeErr
	}
	f.position = decimal.Zero
	f.entryPrice = decimal.Zero
	return nil
}

func (f *fakeExecutor) PlaceStopLoss(_ context.Context, trigger decimal.Decimal) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.stopCalls++
	f.lastStop = trigger
	return f.stopErr
}

func (f *fakeExecutor) PlaceTakeProfit(_ context.Context, trigger decimal.Decimal) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.takeCalls++
	f.lastTake = trigger
	return f.takeErr
}

func (f *fakeExecutor) CancelAll(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.cancelCalls++
	return nil
}

func (f *fakeExecutor) Position(context.Context) (domain.Position, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return domain.Position{Amount: f.position, EntryPrice: f.entryPrice}, nil
}

func (f *fakeExecutor) NormalizeQuantity(qty, _ decimal.Decimal) (decimal.Decimal, error) {
	return qty, nil
}

func (f *fakeExecutor) NormalizePrice(price decimal.Decimal, _ bool) decimal.Decimal {
	return price
}

func (f *fakeExecutor) snapshot() fakeExecutor {
	f.mu.Lock()
	defer f.mu.Unlock()
	return fakeExecutor{
		position:    f.position,
		openCalls:   f.openCalls,
		stopCalls:   f.stopCalls,
		takeCalls:   f.takeCalls,
		cancelCalls: f.cancelCalls,
		closeCalls:  f.closeCalls,
		lastStop:    f.lastStop,
		lastTake:    f.lastTake,
	}
}

// fakeEquitySource — фиксированный (или ошибочный) баланс счёта для тестов.
type fakeEquitySource struct {
	mu     sync.Mutex
	equity decimal.Decimal
	err    error
}

func newFakeEquitySource(equity float64) *fakeEquitySource {
	return &fakeEquitySource{equity: decimal.NewFromFloat(equity)}
}

func (f *fakeEquitySource) Equity(context.Context) (decimal.Decimal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.equity, f.err
}

// fakeRiskGate — портфельный риск-менеджер в памяти для тестов. По умолчанию
// (newFakeRiskGate) разрешает всё — так существующие тесты входа не нужно
// переписывать под риск-логику, которую они не проверяют.
type fakeRiskGate struct {
	mu               sync.Mutex
	allow            bool
	reason           string
	dailyLimitHit    bool
	drawdownLimitHit bool

	reserveCalls      int
	releaseCalls      int
	pnlCalls          int
	lastReserveSymbol string
	lastReserveRisk   decimal.Decimal
	lastPnL           decimal.Decimal
}

func newFakeRiskGate() *fakeRiskGate {
	return &fakeRiskGate{allow: true}
}

func (f *fakeRiskGate) CanOpen(_, _ decimal.Decimal) (bool, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.allow, f.reason
}

func (f *fakeRiskGate) Reserve(symbol string, riskDollars decimal.Decimal) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reserveCalls++
	f.lastReserveSymbol = symbol
	f.lastReserveRisk = riskDollars
}

func (f *fakeRiskGate) Release(string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.releaseCalls++
}

func (f *fakeRiskGate) RecordRealizedPnL(pnl decimal.Decimal) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pnlCalls++
	f.lastPnL = pnl
}

func (f *fakeRiskGate) DailyLossLimitHit(decimal.Decimal) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dailyLimitHit
}

func (f *fakeRiskGate) MaxDrawdownHit(decimal.Decimal) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.drawdownLimitHit
}

func (f *fakeRiskGate) snapshot() fakeRiskGate {
	f.mu.Lock()
	defer f.mu.Unlock()
	return fakeRiskGate{
		reserveCalls:      f.reserveCalls,
		releaseCalls:      f.releaseCalls,
		pnlCalls:          f.pnlCalls,
		lastReserveSymbol: f.lastReserveSymbol,
		lastReserveRisk:   f.lastReserveRisk,
		lastPnL:           f.lastPnL,
	}
}

// fakeFundingSource — фиксированная ставка финансирования для тестов.
type fakeFundingSource struct {
	rate decimal.Decimal
	err  error
}

func (f *fakeFundingSource) FundingRate(context.Context) (decimal.Decimal, error) {
	return f.rate, f.err
}

// testParams — небольшие периоды, чтобы индикаторы прогревались за те же
// 3 свечи, что и раньше. ATRPeriod=1 и VolumeAvgPeriod=1 делают их Ready
// сразу после первого Update — это осознанный выбор для предсказуемости
// ручных расчётов в тестах, а не рекомендация для боевого конфига.
func testParams() Params {
	return Params{
		LookbackBars:      3,
		BreakoutPct:       decimal.Zero,
		CooldownBars:      1,
		TrendEMAPeriod:    3,
		ATRPeriod:         1,
		VolumeAvgPeriod:   1,
		VolumeMultiplier:  decimal.NewFromFloat(0.5),
		ATRStopMultiplier: decimal.NewFromInt(1),
		RiskRewardRatio:   decimal.NewFromInt(2),
		RiskPerTradePct:   decimal.NewFromInt(1),
	}
}

// newTestBot собирает бота со всеми фейками по умолчанию (эквити 10000,
// риск-гейт разрешает всё) — большинству тестов не важна конкретная связка.
func newTestBot(exec *fakeExecutor) (*Breakout, *fakeEquitySource, *fakeRiskGate) {
	eq := newFakeEquitySource(10000)
	risk := newFakeRiskGate()
	return NewBreakout(exec, eq, risk, nil, testParams()), eq, risk
}

// candle строит закрытую свечу. Low ставится на 1 ниже close (а не равным
// close, как раньше), чтобы True Range не был нулевым — иначе ATR не мог бы
// посчитать реальное расстояние до стопа. Volume по умолчанию — 1, этого
// достаточно, чтобы пройти объёмный фильтр при testParams().VolumeMultiplier
// (0.5) на любой константной истории объёма.
func candle(high, closePrice float64) domain.Candle {
	h := decimal.NewFromFloat(high)
	c := decimal.NewFromFloat(closePrice)
	return domain.Candle{
		Symbol:    "BTCUSDT",
		OpenTime:  time.Now(),
		CloseTime: time.Now(),
		Open:      c,
		High:      h,
		Low:       c.Sub(decimal.NewFromInt(1)),
		Close:     c,
		Volume:    decimal.NewFromInt(1),
		IsClosed:  true,
	}
}

// candleDown — свеча для тестов пробоя вниз (шорт): High ставится на 1 выше
// close (не равным, чтобы TR не был нулевым), Low задаётся явно — это и есть
// уровень пробоя.
func candleDown(low, closePrice float64) domain.Candle {
	l := decimal.NewFromFloat(low)
	c := decimal.NewFromFloat(closePrice)
	return domain.Candle{
		Symbol:    "BTCUSDT",
		OpenTime:  time.Now(),
		CloseTime: time.Now(),
		Open:      c,
		High:      c.Add(decimal.NewFromInt(1)),
		Low:       l,
		Close:     c,
		Volume:    decimal.NewFromInt(1),
		IsClosed:  true,
	}
}

func warmFlat(bot *Breakout) {
	bot.Warmup([]domain.Candle{candle(100, 100), candle(100, 100), candle(100, 100)})
}

func TestBreakoutOpensOnNewHigh(t *testing.T) {
	exec := &fakeExecutor{}
	bot, _, _ := newTestBot(exec)
	warmFlat(bot)

	bot.OnCandle(context.Background(), candle(105, 105))

	got := exec.snapshot()
	if got.openCalls != 1 {
		t.Fatalf("ожидался 1 вход, получено %d", got.openCalls)
	}
	if got.stopCalls != 1 || got.takeCalls != 1 {
		t.Fatalf("ожидались стоп и тейк, получено stop=%d take=%d", got.stopCalls, got.takeCalls)
	}
	if bot.State() != "IN_POSITION" {
		t.Fatalf("ожидалось IN_POSITION, получено %s", bot.State())
	}
}

func TestBreakoutOpensShortOnNewLow(t *testing.T) {
	exec := &fakeExecutor{}
	bot, _, _ := newTestBot(exec)
	warmFlat(bot)

	bot.OnCandle(context.Background(), candleDown(90, 90))

	got := exec.snapshot()
	if got.openCalls != 1 {
		t.Fatalf("ожидался 1 вход, получено %d", got.openCalls)
	}
	if got.stopCalls != 1 || got.takeCalls != 1 {
		t.Fatalf("ожидались стоп и тейк, получено stop=%d take=%d", got.stopCalls, got.takeCalls)
	}
	if bot.State() != "IN_POSITION" {
		t.Fatalf("ожидалось IN_POSITION, получено %s", bot.State())
	}
	// ATR после этой свечи = 10 (TR = |low(90)-prevClose(100)| = 10),
	// ATRStopMultiplier=1, RiskRewardRatio=2: стоп(шорт)=100+10=110, тейк=100-20=80.
	if !got.lastStop.Equal(decimal.NewFromInt(110)) {
		t.Fatalf("ожидался стоп 110, получено %s", got.lastStop)
	}
	if !got.lastTake.Equal(decimal.NewFromInt(80)) {
		t.Fatalf("ожидался тейк 80, получено %s", got.lastTake)
	}
}

func TestNoEntryWithoutBreakout(t *testing.T) {
	exec := &fakeExecutor{}
	bot, _, _ := newTestBot(exec)
	warmFlat(bot)

	// Закрытие ровно на уровне максимума окна — это не пробой.
	bot.OnCandle(context.Background(), candle(100, 100))

	if got := exec.snapshot(); got.openCalls != 0 {
		t.Fatalf("вход не должен был произойти, получено %d вызовов", got.openCalls)
	}
}

func TestGreenCandleAloneIsNotSignal(t *testing.T) {
	exec := &fakeExecutor{}
	bot, _, _ := newTestBot(exec)
	warmFlat(bot)

	// Свеча выросла от своего открытия, но не пробила максимум окна.
	c := candle(99, 99)
	c.Open = decimal.NewFromInt(98)
	bot.OnCandle(context.Background(), c)

	if got := exec.snapshot(); got.openCalls != 0 {
		t.Fatalf("рост внутри свечи не должен быть сигналом, получено %d входов", got.openCalls)
	}
}

func TestEntryBlockedByTrendFilter(t *testing.T) {
	exec := &fakeExecutor{}
	bot, _, _ := newTestBot(exec)
	warmFlat(bot)

	// Искусственно задираем тренд высоко над окном пробоя (white-box —
	// тест в том же пакете, что и brain.go): пробой и объём пройдут,
	// а фильтр тренда — нет.
	bot.trendEMA.Update(decimal.NewFromInt(1000))

	bot.OnCandle(context.Background(), candle(105, 105))

	if got := exec.snapshot(); got.openCalls != 0 {
		t.Fatalf("вход при цене ниже EMA-тренда должен быть заблокирован, openCalls=%d", got.openCalls)
	}
	if bot.State() != "IDLE" {
		t.Fatalf("ожидалось IDLE, получено %s", bot.State())
	}
}

func TestEntryBlockedByLowVolume(t *testing.T) {
	exec := &fakeExecutor{}
	bot, _, _ := newTestBot(exec)
	warmFlat(bot)

	c := candle(105, 105)
	c.Volume = decimal.NewFromFloat(0.01) // намного ниже средней (1) * 0.5

	bot.OnCandle(context.Background(), c)

	if got := exec.snapshot(); got.openCalls != 0 {
		t.Fatalf("вход при слабом объёме должен быть заблокирован, openCalls=%d", got.openCalls)
	}
	if bot.State() != "IDLE" {
		t.Fatalf("ожидалось IDLE, получено %s", bot.State())
	}
}

func TestEntryBlockedByRiskGate(t *testing.T) {
	exec := &fakeExecutor{}
	eq := newFakeEquitySource(10000)
	risk := newFakeRiskGate()
	risk.allow = false
	risk.reason = "портфельный лимит риска исчерпан"
	bot := NewBreakout(exec, eq, risk, nil, testParams())
	warmFlat(bot)

	bot.OnCandle(context.Background(), candle(105, 105))

	if got := exec.snapshot(); got.openCalls != 0 {
		t.Fatalf("риск-гейт должен был заблокировать вход, openCalls=%d", got.openCalls)
	}
	// Автомат должен вернуться в IDLE, а не зависнуть в OPENING.
	if bot.State() != "IDLE" {
		t.Fatalf("ожидалось IDLE после отказа риск-гейта, получено %s", bot.State())
	}
}

func TestEntryBlockedByDailyLossLimit(t *testing.T) {
	exec := &fakeExecutor{}
	eq := newFakeEquitySource(10000)
	risk := newFakeRiskGate()
	risk.dailyLimitHit = true
	bot := NewBreakout(exec, eq, risk, nil, testParams())
	warmFlat(bot)

	bot.OnCandle(context.Background(), candle(105, 105))

	if got := exec.snapshot(); got.openCalls != 0 {
		t.Fatalf("дневной лимит убытка должен был заблокировать вход, openCalls=%d", got.openCalls)
	}
	if bot.State() != "IDLE" {
		t.Fatalf("ожидалось IDLE, получено %s", bot.State())
	}
}

func TestEntryBlockedByMaxDrawdown(t *testing.T) {
	exec := &fakeExecutor{}
	eq := newFakeEquitySource(10000)
	risk := newFakeRiskGate()
	risk.drawdownLimitHit = true
	bot := NewBreakout(exec, eq, risk, nil, testParams())
	warmFlat(bot)

	bot.OnCandle(context.Background(), candle(105, 105))

	if got := exec.snapshot(); got.openCalls != 0 {
		t.Fatalf("просадка портфеля от пика должна была заблокировать вход, openCalls=%d", got.openCalls)
	}
	if bot.State() != "IDLE" {
		t.Fatalf("ожидалось IDLE, получено %s", bot.State())
	}
}

func TestPositionSizeScalesWithEquity(t *testing.T) {
	exec1 := &fakeExecutor{}
	bot1 := NewBreakout(exec1, newFakeEquitySource(10000), newFakeRiskGate(), nil, testParams())
	warmFlat(bot1)
	bot1.OnCandle(context.Background(), candle(105, 105))

	exec2 := &fakeExecutor{}
	bot2 := NewBreakout(exec2, newFakeEquitySource(20000), newFakeRiskGate(), nil, testParams())
	warmFlat(bot2)
	bot2.OnCandle(context.Background(), candle(105, 105))

	qty1 := exec1.snapshot().position
	qty2 := exec2.snapshot().position
	if qty1.IsZero() {
		t.Fatal("ожидался ненулевой объём при эквити 10000")
	}
	want := qty1.Mul(decimal.NewFromInt(2))
	if !qty2.Equal(want) {
		t.Fatalf("при удвоении эквити объём должен удвоиться: было %s, стало %s (ожидалось %s)", qty1, qty2, want)
	}
}

func TestRiskReserveOnEntryAndReleaseOnClose(t *testing.T) {
	exec := &fakeExecutor{}
	eq := newFakeEquitySource(10000)
	risk := newFakeRiskGate()
	bot := NewBreakout(exec, eq, risk, nil, testParams())
	warmFlat(bot)

	bot.OnCandle(context.Background(), candle(105, 105))

	afterOpen := risk.snapshot()
	if afterOpen.reserveCalls != 1 {
		t.Fatalf("ожидался 1 вызов Reserve, получено %d", afterOpen.reserveCalls)
	}
	if afterOpen.lastReserveSymbol != "BTCUSDT" {
		t.Fatalf("Reserve вызван не с тем символом: %s", afterOpen.lastReserveSymbol)
	}
	if afterOpen.lastReserveRisk.LessThanOrEqual(decimal.Zero) {
		t.Fatalf("зарезервированный риск должен быть положительным, получено %s", afterOpen.lastReserveRisk)
	}

	// Стоп сработал на бирже: позиция обнулилась.
	exec.mu.Lock()
	exec.position = decimal.Zero
	exec.entryPrice = decimal.Zero
	exec.mu.Unlock()

	bot.OnOrderEvent(context.Background(), domain.OrderEvent{
		Symbol: "BTCUSDT", Type: "STOP_MARKET", Status: "FILLED", Side: "SELL",
		AvgPrice: decimal.NewFromInt(95),
	})

	afterClose := risk.snapshot()
	if afterClose.releaseCalls != 1 {
		t.Fatalf("ожидался 1 вызов Release, получено %d", afterClose.releaseCalls)
	}
}

func TestRealizedPnLRecordedFromOrderEvent(t *testing.T) {
	exec := &fakeExecutor{}
	eq := newFakeEquitySource(10000)
	risk := newFakeRiskGate()
	bot := NewBreakout(exec, eq, risk, nil, testParams())
	warmFlat(bot)

	bot.OnCandle(context.Background(), candle(105, 105))

	exec.mu.Lock()
	exec.position = decimal.Zero
	exec.entryPrice = decimal.Zero
	exec.mu.Unlock()

	bot.OnOrderEvent(context.Background(), domain.OrderEvent{
		Symbol: "BTCUSDT", Type: "STOP_MARKET", Status: "FILLED", Side: "SELL",
		AvgPrice:    decimal.NewFromInt(95),
		RealizedPnL: decimal.NewFromInt(-100),
	})

	got := risk.snapshot()
	if got.pnlCalls != 1 {
		t.Fatalf("ожидался 1 вызов RecordRealizedPnL, получено %d", got.pnlCalls)
	}
	if !got.lastPnL.Equal(decimal.NewFromInt(-100)) {
		t.Fatalf("ожидался PnL -100, получено %s", got.lastPnL)
	}
}

// Нулевой rp (например, у открывающего MARKET-филла) не должен считаться
// реализованной сделкой.
func TestZeroRealizedPnLNotRecorded(t *testing.T) {
	exec := &fakeExecutor{}
	eq := newFakeEquitySource(10000)
	risk := newFakeRiskGate()
	bot := NewBreakout(exec, eq, risk, nil, testParams())
	warmFlat(bot)

	bot.OnCandle(context.Background(), candle(105, 105))

	// Эхо открывающего MARKET-филла: rp у него всегда 0 (позиция не
	// закрывалась, реализовывать нечего).
	bot.OnOrderEvent(context.Background(), domain.OrderEvent{
		Symbol: "BTCUSDT", Type: "MARKET", Status: "FILLED", Side: "BUY",
		AvgPrice:    decimal.NewFromInt(100),
		RealizedPnL: decimal.Zero,
	})

	if got := risk.snapshot(); got.pnlCalls != 0 {
		t.Fatalf("нулевой rp не должен писаться как реализованный PnL, pnlCalls=%d", got.pnlCalls)
	}
}

func TestNoDoubleEntryWhileInPosition(t *testing.T) {
	exec := &fakeExecutor{}
	bot, _, _ := newTestBot(exec)
	warmFlat(bot)

	bot.OnCandle(context.Background(), candle(105, 105))
	bot.OnCandle(context.Background(), candle(110, 110))
	bot.OnCandle(context.Background(), candle(120, 120))

	if got := exec.snapshot(); got.openCalls != 1 {
		t.Fatalf("ожидался ровно 1 вход, получено %d", got.openCalls)
	}
}

func TestStopFailureClosesPosition(t *testing.T) {
	exec := &fakeExecutor{stopErr: errors.New("отказ биржи")}
	bot, _, _ := newTestBot(exec)
	warmFlat(bot)

	bot.OnCandle(context.Background(), candle(105, 105))

	got := exec.snapshot()
	if got.closeCalls != 1 {
		t.Fatalf("позиция без стопа должна быть закрыта, closeCalls=%d", got.closeCalls)
	}
	if !got.position.IsZero() {
		t.Fatalf("позиция должна быть нулевой, получено %s", got.position)
	}
	if bot.State() != "IDLE" {
		t.Fatalf("ожидалось IDLE, получено %s", bot.State())
	}
}

func TestTakeProfitFailureKeepsProtectedPosition(t *testing.T) {
	exec := &fakeExecutor{takeErr: errors.New("отказ биржи")}
	bot, _, _ := newTestBot(exec)
	warmFlat(bot)

	bot.OnCandle(context.Background(), candle(105, 105))

	// Стоп стоит — риск ограничен, позицию закрывать не нужно.
	if got := exec.snapshot(); got.closeCalls != 0 {
		t.Fatalf("позиция со стопом не должна закрываться, closeCalls=%d", got.closeCalls)
	}
	if bot.State() != "IN_POSITION" {
		t.Fatalf("ожидалось IN_POSITION, получено %s", bot.State())
	}
}

func TestStopAndTakeAreOnOppositeSidesOfEntry(t *testing.T) {
	exec := &fakeExecutor{}
	bot, _, _ := newTestBot(exec)
	warmFlat(bot)

	bot.OnCandle(context.Background(), candle(105, 105))

	got := exec.snapshot()
	entry := decimal.NewFromInt(100) // фейк всегда отдаёт эту цену входа
	if !got.lastStop.LessThan(entry) {
		t.Fatalf("стоп %s должен быть ниже входа %s", got.lastStop, entry)
	}
	if !got.lastTake.GreaterThan(entry) {
		t.Fatalf("тейк %s должен быть выше входа %s", got.lastTake, entry)
	}
	// ATR на момент пробойной свечи = 5 (True Range между предыдущим close=100
	// и высокой/низкой этой свечи 105/104), ATRStopMultiplier=1, RiskRewardRatio=2:
	// стоп = 100-5=95, тейк = 100+5*2=110.
	if !got.lastStop.Equal(decimal.NewFromInt(95)) {
		t.Fatalf("ожидался стоп 95, получено %s", got.lastStop)
	}
	if !got.lastTake.Equal(decimal.NewFromInt(110)) {
		t.Fatalf("ожидался тейк 110, получено %s", got.lastTake)
	}
}

func TestReconcileDetectsClosedPosition(t *testing.T) {
	exec := &fakeExecutor{}
	bot, _, _ := newTestBot(exec)
	warmFlat(bot)

	bot.OnCandle(context.Background(), candle(105, 105))

	// Стоп сработал на бирже: позиция обнулилась.
	exec.mu.Lock()
	exec.position = decimal.Zero
	exec.entryPrice = decimal.Zero
	exec.mu.Unlock()

	bot.OnOrderEvent(context.Background(), domain.OrderEvent{
		Symbol:   "BTCUSDT",
		Type:     "STOP_MARKET",
		Status:   "FILLED",
		Side:     "SELL",
		AvgPrice: decimal.NewFromInt(99),
	})

	if bot.State() != "IDLE" {
		t.Fatalf("после закрытия позиции ожидалось IDLE, получено %s", bot.State())
	}
}

func TestCooldownBlocksImmediateReentry(t *testing.T) {
	exec := &fakeExecutor{}
	bot, _, _ := newTestBot(exec)
	warmFlat(bot)

	bot.OnCandle(context.Background(), candle(105, 105))

	exec.mu.Lock()
	exec.position = decimal.Zero
	exec.entryPrice = decimal.Zero
	exec.mu.Unlock()
	bot.OnOrderEvent(context.Background(), domain.OrderEvent{
		Symbol: "BTCUSDT", Type: "STOP_MARKET", Status: "FILLED", Side: "SELL",
	})

	// CooldownBars=1: следующая свеча съедает паузу, вход невозможен.
	bot.OnCandle(context.Background(), candle(130, 130))
	if got := exec.snapshot(); got.openCalls != 1 {
		t.Fatalf("во время паузы вход запрещён, openCalls=%d", got.openCalls)
	}

	// А следующая за ней уже может открыть сделку.
	bot.OnCandle(context.Background(), candle(140, 140))
	if got := exec.snapshot(); got.openCalls != 2 {
		t.Fatalf("после паузы ожидался повторный вход, openCalls=%d", got.openCalls)
	}
}

func TestRecoverAdoptsExistingLong(t *testing.T) {
	exec := &fakeExecutor{
		position:   decimal.NewFromFloat(0.01),
		entryPrice: decimal.NewFromInt(200),
	}
	bot, _, _ := newTestBot(exec)
	// Recover полагается на прогретый ATR (в реальной оркестрации Warmup
	// всегда вызывается раньше Recover — см. internal/app).
	bot.Warmup([]domain.Candle{candle(200, 200)})

	if err := bot.Recover(context.Background()); err != nil {
		t.Fatalf("Recover вернул ошибку: %v", err)
	}

	got := exec.snapshot()
	if got.stopCalls != 1 || got.takeCalls != 1 {
		t.Fatalf("защита должна быть переставлена, stop=%d take=%d", got.stopCalls, got.takeCalls)
	}
	// Единственная прогревочная свеча даёт TR=High-Low=1 (нет предыдущего
	// close), значит ATR=1: стоп=200-1=199, тейк=200+1*2=202.
	if !got.lastStop.Equal(decimal.NewFromInt(199)) {
		t.Fatalf("стоп должен считаться от ATR при цене входа 200, получено %s", got.lastStop)
	}
	if !got.lastTake.Equal(decimal.NewFromInt(202)) {
		t.Fatalf("тейк должен считаться от ATR при цене входа 200, получено %s", got.lastTake)
	}
	if bot.State() != "IN_POSITION" {
		t.Fatalf("ожидалось IN_POSITION, получено %s", bot.State())
	}
}

func TestRecoverAdoptsExistingShort(t *testing.T) {
	exec := &fakeExecutor{
		position:   decimal.NewFromFloat(-0.01),
		entryPrice: decimal.NewFromInt(200),
	}
	bot, _, _ := newTestBot(exec)
	// Recover полагается на прогретый ATR (в реальной оркестрации Warmup
	// всегда вызывается раньше Recover — см. internal/app).
	bot.Warmup([]domain.Candle{candle(200, 200)})

	if err := bot.Recover(context.Background()); err != nil {
		t.Fatalf("Recover вернул ошибку: %v", err)
	}

	got := exec.snapshot()
	if got.stopCalls != 1 || got.takeCalls != 1 {
		t.Fatalf("защита должна быть переставлена, stop=%d take=%d", got.stopCalls, got.takeCalls)
	}
	// Единственная прогревочная свеча даёт TR=High-Low=1 (нет предыдущего
	// close), значит ATR=1: стоп(шорт)=200+1=201, тейк=200-1*2=198.
	if !got.lastStop.Equal(decimal.NewFromInt(201)) {
		t.Fatalf("стоп шорта должен считаться от ATR при цене входа 200, получено %s", got.lastStop)
	}
	if !got.lastTake.Equal(decimal.NewFromInt(198)) {
		t.Fatalf("тейк шорта должен считаться от ATR при цене входа 200, получено %s", got.lastTake)
	}
	if got.closeCalls != 0 {
		t.Fatalf("шорт больше не закрывается автоматически — он поддерживается, closeCalls=%d", got.closeCalls)
	}
	if bot.State() != "IN_POSITION" {
		t.Fatalf("ожидалось IN_POSITION, получено %s", bot.State())
	}
}

func TestShutdownKeepsProtectionWhilePositionOpen(t *testing.T) {
	exec := &fakeExecutor{}
	bot, _, _ := newTestBot(exec)
	warmFlat(bot)

	bot.OnCandle(context.Background(), candle(105, 105))
	before := exec.snapshot()

	bot.Shutdown(context.Background())

	after := exec.snapshot()
	if after.cancelCalls != before.cancelCalls {
		t.Fatalf("при открытой позиции ордера снимать нельзя: cancelCalls %d → %d",
			before.cancelCalls, after.cancelCalls)
	}
}

func TestShutdownCancelsOrdersWhenFlat(t *testing.T) {
	exec := &fakeExecutor{}
	bot, _, _ := newTestBot(exec)

	bot.Shutdown(context.Background())

	if got := exec.snapshot(); got.cancelCalls != 1 {
		t.Fatalf("без позиции ордера должны быть сняты, cancelCalls=%d", got.cancelCalls)
	}
}

// TestConcurrentCandlesAndEvents ловит гонки: свечи и события приходят из
// разных горутин, и раньше состояние менялось без синхронизации.
func TestConcurrentCandlesAndEvents(t *testing.T) {
	exec := &fakeExecutor{}
	bot, _, _ := newTestBot(exec)
	warmFlat(bot)

	ctx := context.Background()
	var wg sync.WaitGroup

	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func(n int) {
			defer wg.Done()
			bot.OnCandle(ctx, candle(float64(100+n), float64(100+n)))
		}(i)
		go func() {
			defer wg.Done()
			bot.OnOrderEvent(ctx, domain.OrderEvent{
				Symbol: "BTCUSDT", Type: "STOP_MARKET", Status: "FILLED", Side: "SELL",
			})
		}()
	}
	wg.Wait()
}

func TestEntryBlockedByLowADX(t *testing.T) {
	exec := &fakeExecutor{}
	eq := newFakeEquitySource(10000)
	risk := newFakeRiskGate()
	params := testParams()
	params.TrendStrengthMinADX = decimal.NewFromInt(50) // недостижимо сразу после прогрева
	bot := NewBreakout(exec, eq, risk, nil, params)
	warmFlat(bot)

	bot.OnCandle(context.Background(), candle(105, 105))

	if got := exec.snapshot(); got.openCalls != 0 {
		t.Fatalf("низкий ADX должен был заблокировать вход, openCalls=%d", got.openCalls)
	}
	if bot.State() != "IDLE" {
		t.Fatalf("ожидалось IDLE, получено %s", bot.State())
	}
}

func TestEntryAllowedWhenADXAboveThreshold(t *testing.T) {
	exec := &fakeExecutor{}
	eq := newFakeEquitySource(10000)
	risk := newFakeRiskGate()
	params := testParams()
	params.TrendStrengthMinADX = decimal.NewFromInt(10) // низкий, легко достижимый порог
	bot := NewBreakout(exec, eq, risk, nil, params)
	warmFlat(bot)

	// Разгоняем внутренний ADX устойчивым трендом white-box вызовом (тест в
	// том же пакете, что и brain.go) — без этого потребовалось бы десятки
	// реальных OnCandle с полноценным пробоем, что не нужно для проверки
	// именно фильтра.
	price := 100.0
	for i := 0; i < 40; i++ {
		price++
		bot.adx.Update(decimal.NewFromFloat(price+0.5), decimal.NewFromFloat(price-0.5), decimal.NewFromFloat(price))
	}
	if !bot.adx.Value().GreaterThanOrEqual(params.TrendStrengthMinADX) {
		t.Fatalf("подготовка теста: ADX должен быть выше порога, получено %s", bot.adx.Value())
	}

	bot.OnCandle(context.Background(), candle(105, 105))

	if got := exec.snapshot(); got.openCalls != 1 {
		t.Fatalf("вход должен был пройти при высоком ADX, openCalls=%d", got.openCalls)
	}
}

func TestBreakevenMovesStopAfterTriggerR(t *testing.T) {
	exec := &fakeExecutor{}
	eq := newFakeEquitySource(10000)
	risk := newFakeRiskGate()
	params := testParams()
	params.BreakevenTriggerR = decimal.NewFromFloat(1.0)
	bot := NewBreakout(exec, eq, risk, nil, params)
	warmFlat(bot)

	// Вход: entry=100 (фейк всегда её отдаёт), стоп=95, тейк=110 (см.
	// TestStopAndTakeAreOnOppositeSidesOfEntry — тот же расчёт ATR).
	bot.OnCandle(context.Background(), candle(105, 105))
	if bot.State() != "IN_POSITION" {
		t.Fatalf("ожидалось IN_POSITION, получено %s", bot.State())
	}
	before := exec.snapshot()

	// Риск = 100-95 = 5, значит 1R прибыли — High >= 105. Подаём свечу с
	// High=106, чего достаточно для срабатывания переноса в безубыток.
	bot.OnCandle(context.Background(), candle(106, 103))

	after := exec.snapshot()
	if after.stopCalls != before.stopCalls+1 {
		t.Fatalf("ожидался повторный вызов стопа (перенос в безубыток), было %d, стало %d",
			before.stopCalls, after.stopCalls)
	}
	if !after.lastStop.Equal(decimal.NewFromInt(100)) {
		t.Fatalf("ожидался стоп в безубыток на цене входа 100, получено %s", after.lastStop)
	}
	if bot.State() != "IN_POSITION" {
		t.Fatalf("перенос стопа не должен закрывать позицию, получено %s", bot.State())
	}
}

func TestBreakevenDoesNotTriggerBelowThreshold(t *testing.T) {
	exec := &fakeExecutor{}
	eq := newFakeEquitySource(10000)
	risk := newFakeRiskGate()
	params := testParams()
	params.BreakevenTriggerR = decimal.NewFromFloat(1.0)
	bot := NewBreakout(exec, eq, risk, nil, params)
	warmFlat(bot)

	bot.OnCandle(context.Background(), candle(105, 105))
	before := exec.snapshot()

	// Риск = 5, нужен High >= 105 для 1R. High=104 — недостаточно.
	bot.OnCandle(context.Background(), candle(104, 102))

	after := exec.snapshot()
	if after.stopCalls != before.stopCalls {
		t.Fatalf("стоп не должен переставляться до достижения BreakevenTriggerR, было %d, стало %d",
			before.stopCalls, after.stopCalls)
	}
}

func TestMeanReversionEntryWhenChoppyAndStretched(t *testing.T) {
	exec := &fakeExecutor{}
	eq := newFakeEquitySource(10000)
	risk := newFakeRiskGate()
	params := testParams()
	params.TrendStrengthMinADX = decimal.NewFromInt(50) // высокий порог: пробойный вход почти всегда заблокирован
	params.MeanRevATRMultiplier = decimal.NewFromFloat(0.5)
	bot := NewBreakout(exec, eq, risk, nil, params)
	warmFlat(bot)

	// Первая свеча сдвигает тренд/ATR вниз, сигнала не даёт: пробойный шорт
	// блокирует ADX-фильтр, а для возврата к среднему цена ещё недостаточно
	// растянута относительно новой (тоже сдвинувшейся) средней.
	bot.OnCandle(context.Background(), candle(96, 95))
	if got := exec.snapshot(); got.openCalls != 0 {
		t.Fatalf("подготовительная свеча не должна была дать вход, openCalls=%d", got.openCalls)
	}

	// Вторая свеча резко проваливается ниже уже сместившейся средней — это
	// и есть растяжение, на которое реагирует возврат к среднему.
	bot.OnCandle(context.Background(), candle(75, 70))

	got := exec.snapshot()
	if got.openCalls != 1 {
		t.Fatalf("ожидался вход на возврат к среднему, openCalls=%d", got.openCalls)
	}
	if !got.position.IsPositive() {
		t.Fatalf("ожидался лонг (цена далеко ниже средней — ставка на отскок вверх), позиция=%s", got.position)
	}
}

func TestFundingCarryEntryLongWhenRateVeryNegative(t *testing.T) {
	exec := &fakeExecutor{}
	eq := newFakeEquitySource(10000)
	risk := newFakeRiskGate()
	funding := &fakeFundingSource{rate: decimal.NewFromFloat(-0.001)} // шорты платят лонгам
	params := testParams()
	params.FundingCarryMinRate = decimal.NewFromFloat(0.0005)
	bot := NewBreakout(exec, eq, risk, funding, params)
	warmFlat(bot)

	// close=100 внутри диапазона прогрева — ни пробой, ни (выключенный)
	// возврат к среднему сигнала не дают, единственный источник — funding.
	bot.OnCandle(context.Background(), candle(100, 100))

	got := exec.snapshot()
	if got.openCalls != 1 {
		t.Fatalf("ожидался вход на funding carry, openCalls=%d", got.openCalls)
	}
	if !got.position.IsPositive() {
		t.Fatalf("ожидался лонг (funding сильно отрицательный), позиция=%s", got.position)
	}
}

func TestFundingCarryEntryShortWhenRateVeryPositive(t *testing.T) {
	exec := &fakeExecutor{}
	eq := newFakeEquitySource(10000)
	risk := newFakeRiskGate()
	funding := &fakeFundingSource{rate: decimal.NewFromFloat(0.001)} // лонги платят шортам
	params := testParams()
	params.FundingCarryMinRate = decimal.NewFromFloat(0.0005)
	bot := NewBreakout(exec, eq, risk, funding, params)
	warmFlat(bot)

	bot.OnCandle(context.Background(), candle(100, 100))

	got := exec.snapshot()
	if got.openCalls != 1 {
		t.Fatalf("ожидался вход на funding carry, openCalls=%d", got.openCalls)
	}
	if !got.position.IsNegative() {
		t.Fatalf("ожидался шорт (funding сильно положительный), позиция=%s", got.position)
	}
}

func TestFundingCarryDoesNotTriggerBelowThreshold(t *testing.T) {
	exec := &fakeExecutor{}
	eq := newFakeEquitySource(10000)
	risk := newFakeRiskGate()
	funding := &fakeFundingSource{rate: decimal.NewFromFloat(0.0001)} // ниже порога
	params := testParams()
	params.FundingCarryMinRate = decimal.NewFromFloat(0.0005)
	bot := NewBreakout(exec, eq, risk, funding, params)
	warmFlat(bot)

	bot.OnCandle(context.Background(), candle(100, 100))

	if got := exec.snapshot(); got.openCalls != 0 {
		t.Fatalf("funding ниже порога не должен был дать вход, openCalls=%d", got.openCalls)
	}
}

func TestVolTargetingReducesSizeOnVolatilitySpike(t *testing.T) {
	run := func(volTargetPeriod int) decimal.Decimal {
		exec := &fakeExecutor{}
		eq := newFakeEquitySource(10000)
		risk := newFakeRiskGate()
		params := testParams()
		params.VolTargetPeriod = volTargetPeriod
		bot := NewBreakout(exec, eq, risk, nil, params)
		warmFlat(bot) // ATR на плоских свечах прогрева ровный — база для сравнения

		// Резкий скачок пробивает и тренд, и объём — обычный пробойный вход,
		// но с сильно возросшим ATR (TR этой свечи против ровного прогрева).
		bot.OnCandle(context.Background(), candle(150, 140))
		return exec.snapshot().position
	}

	unscaled := run(0) // выключено
	scaled := run(3)   // включено

	if !unscaled.IsPositive() || !scaled.IsPositive() {
		t.Fatalf("оба прогона должны были войти в лонг: unscaled=%s scaled=%s", unscaled, scaled)
	}
	if !scaled.LessThan(unscaled) {
		t.Fatalf("при включённом таргетировании волатильности объём должен быть меньше: scaled=%s, unscaled=%s", scaled, unscaled)
	}
}

// TestATRAverageNotPoisonedByPreReadyZeros ловит регресс: ATR.Update()
// возвращает ровно 0 на каждом баре до собственной готовности (затравка ещё
// не набрана), и если это нулевое значение безусловно скармливать в
// RollingAverage для VolTargetPeriod, средний ATR будет надолго занижен
// нулями из прошлого — даже когда сам ATR уже нормально считается. Тест
// намеренно берёт ATRPeriod=3 (а не 1, как в testParams) и совсем маленький
// VolTargetPeriod=2, чтобы засорение было бы видно сразу же после первого
// валидного значения.
func TestATRAverageNotPoisonedByPreReadyZeros(t *testing.T) {
	exec := &fakeExecutor{}
	eq := newFakeEquitySource(10000)
	risk := newFakeRiskGate()
	params := testParams()
	params.ATRPeriod = 3
	params.VolTargetPeriod = 2
	bot := NewBreakout(exec, eq, risk, nil, params)

	// Постоянные High/Low/Close — TR каждого бара = 10 (High-Low, дальше
	// prevClose тот же, так что max(...) остаётся 10). После 3 баров ATR
	// готов и равен 10.
	high, low, close := decimal.NewFromInt(110), decimal.NewFromInt(100), decimal.NewFromInt(105)
	bot.mu.Lock()
	bot.updateATR(high, low, close) // бар 1: ATR ещё не Ready, возвращает 0
	bot.updateATR(high, low, close) // бар 2: всё ещё не Ready, возвращает 0
	bot.updateATR(high, low, close) // бар 3: становится Ready, ATR=10
	bot.mu.Unlock()

	if !bot.atr.Ready() {
		t.Fatal("подготовка теста: ATR должен быть Ready после 3 баров при ATRPeriod=3")
	}
	// Без фикса atrAvg получил бы [0 (бар2), 10 (бар3)] в окне из 2 — среднее
	// 5. С фиксом — только [10] (бар1/2 отфильтрованы) — среднее 10.
	if got := bot.atrAvg.Value(); !got.Equal(decimal.NewFromInt(10)) {
		t.Fatalf("atrAvg засорён нулевыми значениями ATR до его готовности: получено %s, ожидалось 10", got)
	}
}
