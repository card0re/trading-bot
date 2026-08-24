// Package risk содержит портфельный контроль риска — один экземпляр
// Manager используется всеми торгуемыми символами сразу, поэтому лимиты
// действуют на счёт в целом, а не на каждую монету по отдельности.
package risk

import (
	"fmt"
	"sync"
	"time"

	"github.com/shopspring/decimal"
)

// Manager реализует domain.RiskGate: удерживает открытый риск по всем
// символам и реализованный PnL за текущие сутки (UTC), и решает, можно ли
// открыть ещё одну сделку.
type Manager struct {
	mu sync.Mutex

	portfolioCapPct   decimal.Decimal // максимум суммарного открытого риска, % от эквити
	dailyLossLimitPct decimal.Decimal // дневной лимит убытка, % от эквити
	maxDrawdownPct    decimal.Decimal // максимальная просадка от исторического пика эквити, % (0 — выключено)

	openRisk         map[string]decimal.Decimal // symbol -> риск в USDT, зарезервированный под открытую позицию
	dayStart         time.Time                  // начало текущих суток (UTC), для которых считается realizedPnLToday
	realizedPnLToday decimal.Decimal
	peakEquity       decimal.Decimal // исторический максимум эквити, для MaxDrawdownHit

	now func() time.Time // подменяется в тестах, чтобы не спать в реальном времени
}

// NewManager создаёт риск-менеджер с заданными лимитами (в процентах).
// maxDrawdownPct=0 отключает проверку просадки портфеля.
func NewManager(portfolioCapPct, dailyLossLimitPct, maxDrawdownPct decimal.Decimal) *Manager {
	now := time.Now
	return &Manager{
		portfolioCapPct:   portfolioCapPct,
		dailyLossLimitPct: dailyLossLimitPct,
		maxDrawdownPct:    maxDrawdownPct,
		openRisk:          make(map[string]decimal.Decimal),
		dayStart:          dayStartUTC(now()),
		now:               now,
	}
}

func dayStartUTC(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// rolloverIfNeeded вызывается под удерживаемым m.mu. Сравнение на
// неравенство (а не строго "позже") — намеренно: в реальной работе время
// всегда идёт вперёд, но так рутина остаётся корректной и для тестового
// нестандартного времени.
func (m *Manager) rolloverIfNeeded() {
	ds := dayStartUTC(m.now())
	if !ds.Equal(m.dayStart) {
		m.dayStart = ds
		m.realizedPnLToday = decimal.Zero
	}
}

// CanOpen проверяет, укладывается ли предполагаемый риск новой сделки
// (вместе с уже открытым риском по всем символам) в портфельный лимит.
// Дневной лимит убытка сюда не входит — это отдельная проверка
// (DailyLossLimitHit), т.к. это разные причины отказа для разных логов.
func (m *Manager) CanOpen(equity, proposedRiskDollars decimal.Decimal) (bool, string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	openTotal := decimal.Zero
	for _, r := range m.openRisk {
		openTotal = openTotal.Add(r)
	}
	projected := openTotal.Add(proposedRiskDollars)
	capDollars := equity.Mul(m.portfolioCapPct).Div(decimal.NewFromInt(100))

	if projected.GreaterThan(capDollars) {
		return false, fmt.Sprintf(
			"открытый риск %s + новая сделка %s = %s превысили бы лимит %s%% от эквити (%s)",
			openTotal.StringFixed(2), proposedRiskDollars.StringFixed(2), projected.StringFixed(2),
			m.portfolioCapPct.StringFixed(2), capDollars.StringFixed(2))
	}
	return true, ""
}

// Reserve фиксирует риск сделки по символу (перезаписывает предыдущее
// значение — на символ в любой момент может быть максимум одна открытая
// позиция, т.к. Breakout не допускает вход, пока не закрыта предыдущая).
func (m *Manager) Reserve(symbol string, riskDollars decimal.Decimal) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.openRisk[symbol] = riskDollars
}

// Release освобождает зарезервированный риск при закрытии позиции.
func (m *Manager) Release(symbol string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.openRisk, symbol)
}

// SetClock подменяет источник времени, которым Manager определяет начало
// суток для дневного лимита убытка. В проде не нужен (по умолчанию
// time.Now) — предназначен для бэктеста, где "сейчас" должно совпадать со
// временем проигрываемой исторической свечи, а не с реальным временем
// прогона, иначе весь многомесячный бэктест засчитался бы как одни сутки.
func (m *Manager) SetClock(now func() time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.now = now
}

// RecordRealizedPnL добавляет реализованный PnL закрытой сделки к сумме
// за текущие сутки (UTC). Сутки переключаются автоматически.
func (m *Manager) RecordRealizedPnL(pnl decimal.Decimal) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rolloverIfNeeded()
	m.realizedPnLToday = m.realizedPnLToday.Add(pnl)
}

// DailyLossLimitHit сообщает, исчерпан ли дневной лимит убытка. При true
// новые входы запрещаются, но управление уже открытыми позициями (стоп/тейк)
// продолжается как обычно — это ограничение только на срабатывание Opening.
func (m *Manager) DailyLossLimitHit(equity decimal.Decimal) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rolloverIfNeeded()

	if !m.realizedPnLToday.IsNegative() {
		return false
	}
	limit := equity.Mul(m.dailyLossLimitPct).Div(decimal.NewFromInt(100))
	return m.realizedPnLToday.Abs().GreaterThanOrEqual(limit)
}

// MaxDrawdownHit сообщает, просел ли счёт от своего исторического пика
// эквити больше чем на maxDrawdownPct — портфельный аналог DailyLossLimitHit,
// только не сбрасывается каждые сутки, а держит новые входы заблокированными,
// пока эквити не отыграет обратно выше порога (само по себе, без ручного
// сброса — как и дневной лимит). Уже открытые позиции продолжают
// управляться стопом/тейком как обычно. 0 — выключено.
//
// Дневной лимит и портфельный риск-кап ограничивают риск ОДНОГО дня и ОДНОГО
// момента соответственно — ни один не защищает от растянутой во времени
// серии убыточных дней подряд, которая в бэктесте реально давала просадку
// портфеля глубже, чем казалось по каждому символу отдельно (см.
// cmd/backtest -mode full). Эта проверка — именно про такую серию.
//
// Обновляет внутренний пик эквити при каждом вызове — открытие позиции это
// единственное место, откуда Manager достаточно часто узнаёт текущее эквити.
func (m *Manager) MaxDrawdownHit(equity decimal.Decimal) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	if equity.GreaterThan(m.peakEquity) {
		m.peakEquity = equity
	}
	if m.maxDrawdownPct.IsZero() || m.peakEquity.IsZero() {
		return false
	}
	drawdown := m.peakEquity.Sub(equity).Div(m.peakEquity).Mul(decimal.NewFromInt(100))
	return drawdown.GreaterThanOrEqual(m.maxDrawdownPct)
}
