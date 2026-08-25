// Package carry симулирует delta-neutral funding-rate arbitrage (он же
// cash-and-carry): спот куплен, столько же нотионала шортится в перпе.
// Ценовой риск взаимно гасится (лонг спот + шорт перп), доход — чистый
// funding, который платят лонги шортам на положительной ставке. В отличие
// от strategy.Params.FundingCarryMinRate (испытан и отвергнут как
// направленная ставка без хеджа — см. cmd/tune -mode carry) здесь нет
// ATR-стопа и нет ценового риска по конструкции сделки — реальная причина,
// по которой этот класс стратегий у профи документированно работает
// (10-30% годовых, источники — arbitragescanner.io, kraken.com/learn).
package carry

import (
	"sort"
	"time"

	"trading-bot/internal/domain"

	"github.com/shopspring/decimal"
)

// periodsPerYear — funding на Binance каждые 8 часов, 365*24/8 = 1095.
const periodsPerYear = 1095

// FundingEvent — одна выплата funding вместе со спот- и перп-ценой на этот
// момент (нужны обе, чтобы честно посчитать базис-PnL при входе/выходе, а
// не считать его нулевым).
type FundingEvent struct {
	Time      time.Time
	Rate      decimal.Decimal // ставка за период; >0 — лонги платят шортам
	SpotPrice decimal.Decimal
	PerpPrice decimal.Decimal
}

// BuildEvents сливает funding-точки со спот- и перп-свечами: для каждой
// funding-точки берётся цена ближайшей ПРЕДЫДУЩЕЙ свечи (sort.Search) —
// funding списывается по факту, ставка на будущее ещё не известна трейдеру
// в моменте, поэтому смотреть вперёд было бы утечкой информации. Точки, для
// которых не набралось истории цены (самое начало периода), пропускаются.
func BuildEvents(fundingTimes []time.Time, fundingRates []decimal.Decimal, spot, perp []domain.Candle) []FundingEvent {
	out := make([]FundingEvent, 0, len(fundingTimes))
	for i, t := range fundingTimes {
		sp, ok1 := priceBefore(spot, t)
		pp, ok2 := priceBefore(perp, t)
		if !ok1 || !ok2 {
			continue
		}
		out = append(out, FundingEvent{Time: t, Rate: fundingRates[i], SpotPrice: sp, PerpPrice: pp})
	}
	return out
}

func priceBefore(bars []domain.Candle, t time.Time) (decimal.Decimal, bool) {
	idx := sort.Search(len(bars), func(i int) bool { return bars[i].CloseTime.After(t) })
	if idx == 0 {
		return decimal.Zero, false
	}
	return bars[idx-1].Close, true
}

// Params — пороги входа/выхода в терминах ГОДОВОЙ ставки (интуитивнее
// перебирать и читать, чем ставку за 8ч), сглаженные скользящим средним по
// Lookback периодам — единичная 8-часовая ставка слишком шумная и может
// дёрнуться в минус на один период посреди устойчиво выгодного окна.
type Params struct {
	Lookback        int             // периодов (по 8ч) для скользящего среднего ставки
	EntryAnnualRate decimal.Decimal // войти, когда сглаженная ставка annualized >= это (в %, напр. 10 = 10%)
	ExitAnnualRate  decimal.Decimal // выйти, когда упала ниже (гистерезис: Exit < Entry)
	MaxHoldPeriods  int             // 0 — без ограничения
	SpotFeeRate     decimal.Decimal // taker, за одну ногу, за один вход ИЛИ выход
	PerpFeeRate     decimal.Decimal
}

// Stats — как backtest.Stats, но с разбивкой источника PnL: собранный
// funding отдельно от базис-PnL (расхождение спот/перп цены на входе и
// выходе), чтобы честно увидеть, что именно делает деньги, а не одну сумму.
type Stats struct {
	StartEquity      decimal.Decimal
	FinalEquity      decimal.Decimal
	FundingCollected decimal.Decimal
	BasisPnL         decimal.Decimal
	FeesPaid         decimal.Decimal
	NumTrades        int
	MaxDrawdown      decimal.Decimal
}

func (s Stats) ReturnPct() decimal.Decimal {
	if s.StartEquity.IsZero() {
		return decimal.Zero
	}
	return s.FinalEquity.Sub(s.StartEquity).Div(s.StartEquity).Mul(decimal.NewFromInt(100))
}

// Score — как у остальных инструментов сессии: доходность минус штраф за
// просадку, чтобы не путать "прибыльно" с "прибыльно и спокойно".
func (s Stats) Score() decimal.Decimal {
	return s.ReturnPct().Sub(s.MaxDrawdown.Mul(decimal.NewFromFloat(0.5)))
}

// Simulate проходит события последовательно, держит позицию максимум одну
// за раз (не пирамидит), сайзинг — весь текущий эквити на каждую сделку
// (без хеджа плечо не нужно — позиция и так дельта-нейтральна по цене).
func Simulate(events []FundingEvent, p Params, startEquity decimal.Decimal) Stats {
	equity := startEquity
	peak := startEquity
	maxDD := decimal.Zero

	var fundingSum, basisSum, feesSum decimal.Decimal
	numTrades := 0

	inPosition := false
	var entrySpot, entryPerp decimal.Decimal
	heldPeriods := 0

	hundred := decimal.NewFromInt(100)

	for i, ev := range events {
		if inPosition {
			funding := equity.Mul(ev.Rate)
			fundingSum = fundingSum.Add(funding)
			equity = equity.Add(funding)
			heldPeriods++
		}

		avgRate := trailingAvg(events, i, p.Lookback)
		annualized := avgRate.Mul(decimal.NewFromInt(periodsPerYear)).Mul(hundred)

		shouldExit := inPosition && (annualized.LessThan(p.ExitAnnualRate) ||
			(p.MaxHoldPeriods > 0 && heldPeriods >= p.MaxHoldPeriods))
		if shouldExit {
			basis := ev.SpotPrice.Sub(ev.PerpPrice).Sub(entrySpot.Sub(entryPerp))
			basisPct := decimal.Zero
			if entrySpot.IsPositive() {
				basisPct = basis.Div(entrySpot)
			}
			basisPnL := equity.Mul(basisPct)
			basisSum = basisSum.Add(basisPnL)
			equity = equity.Add(basisPnL)

			fee := equity.Mul(p.SpotFeeRate.Add(p.PerpFeeRate))
			feesSum = feesSum.Add(fee)
			equity = equity.Sub(fee)

			inPosition = false
			heldPeriods = 0
		} else if !inPosition && annualized.GreaterThanOrEqual(p.EntryAnnualRate) {
			fee := equity.Mul(p.SpotFeeRate.Add(p.PerpFeeRate))
			feesSum = feesSum.Add(fee)
			equity = equity.Sub(fee)

			entrySpot, entryPerp = ev.SpotPrice, ev.PerpPrice
			inPosition = true
			numTrades++ // считаем по входу: позиция, ещё не закрытая на конец окна, — тоже сделка, не "0"
		}

		if equity.GreaterThan(peak) {
			peak = equity
		}
		if peak.IsPositive() {
			dd := peak.Sub(equity).Div(peak).Mul(hundred)
			if dd.GreaterThan(maxDD) {
				maxDD = dd
			}
		}
	}

	return Stats{
		StartEquity:      startEquity,
		FinalEquity:      equity,
		FundingCollected: fundingSum,
		BasisPnL:         basisSum,
		FeesPaid:         feesSum,
		NumTrades:        numTrades,
		MaxDrawdown:      maxDD,
	}
}

// trailingAvg — среднее по последним lookback ставкам, включая текущую.
func trailingAvg(events []FundingEvent, i, lookback int) decimal.Decimal {
	from := i - lookback + 1
	if from < 0 {
		from = 0
	}
	window := events[from : i+1]
	sum := decimal.Zero
	for _, e := range window {
		sum = sum.Add(e.Rate)
	}
	return sum.Div(decimal.NewFromInt(int64(len(window))))
}
