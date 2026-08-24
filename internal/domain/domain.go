// Package domain содержит типы, общие для биржевого слоя и стратегии.
// Он не зависит ни от одной библиотеки биржи, поэтому стратегию можно
// тестировать без сетевых вызовов.
package domain

import (
	"context"
	"time"

	"github.com/shopspring/decimal"
)

// Candle — закрытая (или формирующаяся) свеча.
type Candle struct {
	Symbol    string
	OpenTime  time.Time
	CloseTime time.Time
	Open      decimal.Decimal
	High      decimal.Decimal
	Low       decimal.Decimal
	Close     decimal.Decimal
	Volume    decimal.Decimal
	// TakerBuyVolume — часть Volume, пришедшая от тейкеров-покупателей
	// (агрессивные BUY, а не пассивные лимитки на bid). TakerBuyVolume/Volume
	// — грубый прокси биржевого дисбаланса потока ордеров без доступа к
	// самому стакану (у Binance нет бесплатной истории L2-глубины, а это
	// поле есть в каждой свече, и в REST, и в WS, готово к бэктесту).
	TakerBuyVolume decimal.Decimal
	IsClosed       bool
}

// OrderEvent — событие по ордеру из приватного потока.
type OrderEvent struct {
	Symbol        string
	ClientOrderID string
	Type          string // MARKET, STOP_MARKET, TAKE_PROFIT_MARKET, ...
	Status        string // NEW, FILLED, CANCELED, EXPIRED, ...
	Side          string // BUY / SELL
	AvgPrice      decimal.Decimal
	FilledQty     decimal.Decimal
	ReduceOnly    bool
	// RealizedPnL — реализованный PnL этого фила в котируемой валюте (поле
	// rp из ORDER_TRADE_UPDATE). Ненулевой только на закрывающих сделках;
	// источник для дневного лимита убытка RiskGate — авторитетнее, чем
	// пересчёт от цены входа/выхода на стороне стратегии.
	RealizedPnL decimal.Decimal
}

// Position — текущая позиция по символу. Amount: >0 лонг, <0 шорт, 0 — плоско.
type Position struct {
	Amount     decimal.Decimal
	EntryPrice decimal.Decimal
}

func (p Position) IsFlat() bool { return p.Amount.IsZero() }
func (p Position) IsLong() bool { return p.Amount.IsPositive() }

// Notifier отправляет уведомления о ключевых событиях (сделка, критическая
// ошибка) во внешний канал. Best-effort — реализация сама решает, что
// делать с ошибкой отправки, торговую логику это не блокирует.
type Notifier interface {
	Notify(ctx context.Context, message string)
}

// FundingRateSource отдаёт текущую (последнюю известную) ставку
// финансирования по символу, в процентах за период (обычно 8 часов).
// Положительная ставка — лонги платят шортам (рынок перекошен в лонг),
// отрицательная — наоборот. Один экземпляр — один символ (в отличие от
// EquitySource/RiskGate, ставка финансирования не общая на аккаунт).
type FundingRateSource interface {
	FundingRate(ctx context.Context) (decimal.Decimal, error)
}

// EquitySource отдаёт текущий эквити счёта (баланс + нереализованный PnL) —
// базу для расчёта размера позиции по риску. Отдельный интерфейс, а не метод
// Executor: эквити общий на весь аккаунт, а не привязан к одному символу.
type EquitySource interface {
	Equity(ctx context.Context) (decimal.Decimal, error)
}

// RiskGate — портфельный контроль риска, общий на все торгуемые символы.
// Стратегия спрашивает разрешения перед входом и отчитывается об изменении
// риска и реализованном PnL; агрегированный риск и дневной лимит убытка
// считает risk.Manager (internal/risk) — один экземпляр на все символы.
type RiskGate interface {
	// CanOpen проверяет, укладывается ли предполагаемый риск сделки
	// (в долларах) в лимиты портфеля. reason объясняет отказ для логов.
	CanOpen(equity, proposedRiskDollars decimal.Decimal) (allowed bool, reason string)
	// Reserve фиксирует риск сделки после реального открытия позиции
	// (по факту исполненного объёма и выставленного стопа, а не оценки).
	Reserve(symbol string, riskDollars decimal.Decimal)
	// Release освобождает зарезервированный риск при закрытии позиции.
	Release(symbol string)
	// RecordRealizedPnL учитывает реализованный PnL закрытой сделки в
	// дневном лимите убытка.
	RecordRealizedPnL(pnl decimal.Decimal)
	// DailyLossLimitHit сообщает, достигнут ли дневной лимит убытка —
	// в этом случае новые входы запрещены, но управление открытой позицией
	// (стоп/тейк) продолжается как обычно.
	DailyLossLimitHit(equity decimal.Decimal) bool
	// MaxDrawdownHit сообщает, просел ли счёт от исторического пика эквити
	// больше заданного порога — в отличие от DailyLossLimitHit не сбрасывается
	// каждые сутки, а держит новые входы заблокированными, пока эквити не
	// восстановится. Управление уже открытыми позициями не затрагивает.
	MaxDrawdownHit(equity decimal.Decimal) bool
}

// Executor — всё, что стратегии нужно от биржи. Символ зашит в реализацию,
// поэтому стратегия не может случайно отправить ордер не по той паре.
type Executor interface {
	// OpenLong открывает лонг по рынку и возвращает среднюю цену входа.
	OpenLong(ctx context.Context, qty decimal.Decimal) (decimal.Decimal, error)
	// OpenShort открывает шорт по рынку и возвращает среднюю цену входа.
	OpenShort(ctx context.Context, qty decimal.Decimal) (decimal.Decimal, error)
	// ClosePositionMarket закрывает текущую позицию по рынку (reduceOnly).
	ClosePositionMarket(ctx context.Context) error
	// PlaceStopLoss ставит стоп на закрытие всей позиции.
	PlaceStopLoss(ctx context.Context, triggerPrice decimal.Decimal) error
	// PlaceTakeProfit ставит тейк на закрытие всей позиции.
	PlaceTakeProfit(ctx context.Context, triggerPrice decimal.Decimal) error
	// CancelAll снимает и обычные, и алго-ордера по символу.
	CancelAll(ctx context.Context) error
	// Position возвращает текущую позицию по символу.
	Position(ctx context.Context) (Position, error)
	// NormalizeQuantity приводит объём к stepSize и проверяет minQty/minNotional.
	NormalizeQuantity(qty, refPrice decimal.Decimal) (decimal.Decimal, error)
	// NormalizePrice приводит цену к tickSize (down=true — вниз, иначе вверх).
	NormalizePrice(price decimal.Decimal, down bool) decimal.Decimal
	// Symbol возвращает торгуемый символ.
	Symbol() string
}
