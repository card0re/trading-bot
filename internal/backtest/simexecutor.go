// Package backtest прогоняет реальную стратегию (internal/strategy) и
// реальные индикаторы (internal/indicator) против исторических свечей без
// сети — через SimExecutor, реализующий domain.Executor и domain.EquitySource
// в памяти. Стратегия не знает, что торгует не по-настоящему: это и есть
// гарантия, что бэктест и боевой запуск не разойдутся в поведении.
package backtest

import (
	"context"
	"time"

	"trading-bot/internal/domain"
	exchange "trading-bot/internal/exchange/binance"

	"github.com/shopspring/decimal"
)

// Trade — одна закрытая сделка, единица статистики.
type Trade struct {
	EntryTime   time.Time
	ExitTime    time.Time
	EntryPrice  decimal.Decimal
	ExitPrice   decimal.Decimal
	Qty         decimal.Decimal
	PnL         decimal.Decimal
	RiskDollars decimal.Decimal // |entry-стоп|*qty на момент входа — база для R-мультипла
	ExitReason  string          // STOP_MARKET | TAKE_PROFIT_MARKET | MANUAL_CLOSE
}

// SimExecutor — исполнитель в памяти для одного символа. Реализует и
// domain.Executor, и domain.EquitySource (один и тот же тип: эквити в
// бэктесте — это же running balance, что и позиция).
//
// Упрощения (сознательные, не спрятанные): рыночный вход исполняется по
// close сигнальной свечи (с учётом slippagePct — см. ниже); стоп/тейк
// исполняются по цене триггера (тоже со slippagePct), когда High/Low свечи
// её пересекают; плечо и ликвидация не моделируются. Комиссии (feeRate) —
// учитываются.
type SimExecutor struct {
	symbol      string
	info        *exchange.SymbolInfo
	feeRate     decimal.Decimal // доля от нотионала за сделку, на каждую сторону (вход и выход)
	slippagePct decimal.Decimal // доля от цены, всегда не в пользу трейдера (BUY дороже, SELL дешевле)

	position   decimal.Decimal
	entryPrice decimal.Decimal
	entryTime  time.Time
	entryFee   decimal.Decimal

	stopPrice decimal.Decimal
	takePrice decimal.Decimal
	hasStop   bool
	hasTake   bool

	lastClose decimal.Decimal
	lastTime  time.Time

	equity decimal.Decimal
	trades []Trade
}

// TakerFeeRate — стандартная комиссия тейкера на Binance USDT-M Futures
// (0-й VIP уровень), 0.05% за сторону. Вход у нас всегда MARKET, а
// стоп/тейк исполняются как рыночные при срабатывании триггера — то есть
// обе стороны сделки берут именно тейкерскую комиссию, не мейкерскую.
var TakerFeeRate = decimal.NewFromFloat(0.0005)

// DefaultSlippagePct — консервативная оценка проскальзывания рыночного
// ордера на ликвидных парах (0.02% от цены), не откалиброванная по
// реальным сделкам — реальное проскальзывание на резком движении (особенно
// на стопе) может быть больше. Лучше закладывать что-то, чем считать
// исполнение идеальным.
var DefaultSlippagePct = decimal.NewFromFloat(0.0002)

// NewSimExecutor создаёт симулятор с заданным стартовым эквити, комиссией
// за сторону сделки и проскальзыванием (передайте decimal.Zero любому из
// них, чтобы не учитывать).
func NewSimExecutor(symbol string, info *exchange.SymbolInfo, startingEquity, feeRate, slippagePct decimal.Decimal) *SimExecutor {
	return &SimExecutor{symbol: symbol, info: info, equity: startingEquity, feeRate: feeRate, slippagePct: slippagePct}
}

// slipUp/slipDown сдвигают цену исполнения не в пользу трейдера: BUY всегда
// исполняется чуть дороже, SELL — чуть дешевле рыночной/триггерной цены.
func (s *SimExecutor) slipUp(price decimal.Decimal) decimal.Decimal {
	return price.Mul(decimal.NewFromInt(1).Add(s.slippagePct))
}

func (s *SimExecutor) slipDown(price decimal.Decimal) decimal.Decimal {
	return price.Mul(decimal.NewFromInt(1).Sub(s.slippagePct))
}

// OnCandle обновляет часы симулятора и проверяет срабатывание стопа/тейка
// по High/Low текущей свечи. Вызывается драйвером бэктеста ДО bot.OnCandle.
// Если что-то сработало — возвращает событие для bot.OnOrderEvent (как в
// реальности оно пришло бы с приватного потока), иначе nil.
func (s *SimExecutor) OnCandle(c domain.Candle) *domain.OrderEvent {
	s.lastClose = c.Close
	s.lastTime = c.CloseTime

	if s.position.IsZero() {
		return nil
	}

	// Стоп проверяем первым — если в пределах одной свечи задеты оба уровня,
	// консервативнее считать, что сработал менее выгодный исход.
	if s.position.IsPositive() {
		// Лонг: стоп снизу, тейк сверху.
		if s.hasStop && c.Low.LessThanOrEqual(s.stopPrice) {
			return s.closeAt(s.stopPrice, "STOP_MARKET")
		}
		if s.hasTake && c.High.GreaterThanOrEqual(s.takePrice) {
			return s.closeAt(s.takePrice, "TAKE_PROFIT_MARKET")
		}
		return nil
	}

	// Шорт: стоп сверху, тейк снизу.
	if s.hasStop && c.High.GreaterThanOrEqual(s.stopPrice) {
		return s.closeAt(s.stopPrice, "STOP_MARKET")
	}
	if s.hasTake && c.Low.LessThanOrEqual(s.takePrice) {
		return s.closeAt(s.takePrice, "TAKE_PROFIT_MARKET")
	}
	return nil
}

// closeAt считает PnL для обоих направлений через один и тот же знаковый
// qty (>0 лонг, <0 шорт, см. domain.Position) — не нужно ветвить формулу.
func (s *SimExecutor) closeAt(triggerPrice decimal.Decimal, reason string) *domain.OrderEvent {
	qty := s.position
	entry := s.entryPrice

	// Закрытие лонга — SELL (дешевле триггера), закрытие шорта — BUY (дороже).
	price := triggerPrice
	if qty.IsPositive() {
		price = s.slipDown(price)
	} else {
		price = s.slipUp(price)
	}
	exitFee := qty.Abs().Mul(price).Mul(s.feeRate)
	pnl := qty.Mul(price.Sub(entry)).Sub(s.entryFee).Sub(exitFee) // за вычетом комиссий обеих сторон

	riskDollars := decimal.Zero
	if s.hasStop {
		riskDollars = qty.Abs().Mul(entry.Sub(s.stopPrice)).Abs()
	}

	closeSide := "SELL" // закрытие лонга
	if qty.IsNegative() {
		closeSide = "BUY" // закрытие шорта
	}

	s.trades = append(s.trades, Trade{
		EntryTime:   s.entryTime,
		ExitTime:    s.lastTime,
		EntryPrice:  entry,
		ExitPrice:   price,
		Qty:         qty,
		PnL:         pnl,
		RiskDollars: riskDollars,
		ExitReason:  reason,
	})
	s.equity = s.equity.Add(pnl)

	s.position = decimal.Zero
	s.entryPrice = decimal.Zero
	s.hasStop = false
	s.hasTake = false

	return &domain.OrderEvent{
		Symbol:      s.symbol,
		Type:        reason,
		Status:      "FILLED",
		Side:        closeSide,
		AvgPrice:    price,
		FilledQty:   qty.Abs(),
		ReduceOnly:  true,
		RealizedPnL: pnl,
	}
}

// Trades возвращает все закрытые за прогон сделки.
func (s *SimExecutor) Trades() []Trade { return s.trades }

// --- domain.Executor ---

func (s *SimExecutor) Symbol() string { return s.symbol }

func (s *SimExecutor) NormalizeQuantity(qty, refPrice decimal.Decimal) (decimal.Decimal, error) {
	return s.info.RoundQuantity(qty, refPrice)
}

func (s *SimExecutor) NormalizePrice(price decimal.Decimal, down bool) decimal.Decimal {
	return s.info.RoundPrice(price, down)
}

func (s *SimExecutor) OpenLong(_ context.Context, qty decimal.Decimal) (decimal.Decimal, error) {
	s.position = qty
	s.entryPrice = s.slipUp(s.lastClose) // вход в лонг — BUY
	s.entryTime = s.lastTime
	s.entryFee = qty.Mul(s.entryPrice).Mul(s.feeRate)
	s.hasStop = false
	s.hasTake = false
	return s.entryPrice, nil
}

func (s *SimExecutor) OpenShort(_ context.Context, qty decimal.Decimal) (decimal.Decimal, error) {
	s.position = qty.Neg()
	s.entryPrice = s.slipDown(s.lastClose) // вход в шорт — SELL
	s.entryTime = s.lastTime
	s.entryFee = qty.Mul(s.entryPrice).Mul(s.feeRate)
	s.hasStop = false
	s.hasTake = false
	return s.entryPrice, nil
}

func (s *SimExecutor) PlaceStopLoss(_ context.Context, trigger decimal.Decimal) error {
	s.stopPrice = trigger
	s.hasStop = true
	return nil
}

func (s *SimExecutor) PlaceTakeProfit(_ context.Context, trigger decimal.Decimal) error {
	s.takePrice = trigger
	s.hasTake = true
	return nil
}

func (s *SimExecutor) CancelAll(context.Context) error {
	s.hasStop = false
	s.hasTake = false
	return nil
}

// ClosePositionMarket закрывает по последней известной цене закрытия —
// используется стратегией как аварийный выход (например, если постановка
// стопа не удалась в реальности; в симуляции она не может не удаться, но
// путь кода общий со стратегией и должен вести себя согласованно).
func (s *SimExecutor) ClosePositionMarket(context.Context) error {
	if s.position.IsZero() {
		return nil
	}
	s.closeAt(s.lastClose, "MANUAL_CLOSE")
	return nil
}

func (s *SimExecutor) Position(context.Context) (domain.Position, error) {
	return domain.Position{Amount: s.position, EntryPrice: s.entryPrice}, nil
}

// --- domain.EquitySource ---

func (s *SimExecutor) Equity(context.Context) (decimal.Decimal, error) {
	return s.equity, nil
}

// AlwaysAllowRiskGate — риск-гейт без ограничений: прогон «чистой»
// стратегии без учёта портфельных лимитов. Для прогона с учётом лимитов
// используйте настоящий risk.Manager — он тоже реализует domain.RiskGate.
type AlwaysAllowRiskGate struct{}

func (AlwaysAllowRiskGate) CanOpen(decimal.Decimal, decimal.Decimal) (bool, string) { return true, "" }
func (AlwaysAllowRiskGate) Reserve(string, decimal.Decimal)                         {}
func (AlwaysAllowRiskGate) Release(string)                                          {}
func (AlwaysAllowRiskGate) RecordRealizedPnL(decimal.Decimal)                       {}
func (AlwaysAllowRiskGate) DailyLossLimitHit(decimal.Decimal) bool                  { return false }
func (AlwaysAllowRiskGate) MaxDrawdownHit(decimal.Decimal) bool                     { return false }
