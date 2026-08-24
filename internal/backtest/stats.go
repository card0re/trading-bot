package backtest

import (
	"fmt"

	"github.com/shopspring/decimal"
)

// Stats — сводная статистика по набору закрытых сделок. Осознанно не
// считает Sharpe/alpha/beta — для них нужны безрисковая ставка и бенчмарк,
// которые здесь не заданы; метрики ниже вычислимы из самих сделок.
type Stats struct {
	TradeCount int
	Wins       int
	Losses     int

	WinRatePct     decimal.Decimal
	ProfitFactor   decimal.Decimal // sum(прибыльных)/|sum(убыточных)|; 0, если убыточных сделок не было
	AvgRMultiple   decimal.Decimal // средний PnL/риск на сделку — проверяет, отрабатывает ли RiskRewardRatio на практике
	TotalReturnPct decimal.Decimal
	MaxDrawdownPct decimal.Decimal // наибольшая просадка эквити от локального пика, %

	StartingEquity decimal.Decimal
	FinalEquity    decimal.Decimal
}

// ComputeStats считает статистику по сделкам в хронологическом порядке
// закрытия относительно стартового эквити.
func ComputeStats(trades []Trade, startingEquity decimal.Decimal) Stats {
	stats := Stats{StartingEquity: startingEquity, FinalEquity: startingEquity}
	if len(trades) == 0 {
		return stats
	}

	var wins, losses, rCount int
	sumWins := decimal.Zero
	sumLosses := decimal.Zero // положительная величина
	sumR := decimal.Zero

	equity := startingEquity
	peak := startingEquity
	maxDD := decimal.Zero

	for _, tr := range trades {
		switch {
		case tr.PnL.IsPositive():
			wins++
			sumWins = sumWins.Add(tr.PnL)
		case tr.PnL.IsNegative():
			losses++
			sumLosses = sumLosses.Add(tr.PnL.Abs())
		}

		if !tr.RiskDollars.IsZero() {
			sumR = sumR.Add(tr.PnL.Div(tr.RiskDollars))
			rCount++
		}

		equity = equity.Add(tr.PnL)
		if equity.GreaterThan(peak) {
			peak = equity
		}
		if peak.IsPositive() {
			if dd := peak.Sub(equity).Div(peak); dd.GreaterThan(maxDD) {
				maxDD = dd
			}
		}
	}

	stats.TradeCount = len(trades)
	stats.Wins = wins
	stats.Losses = losses
	stats.FinalEquity = equity
	stats.WinRatePct = decimal.NewFromInt(int64(wins)).
		Div(decimal.NewFromInt(int64(len(trades)))).Mul(decimal.NewFromInt(100))
	if !sumLosses.IsZero() {
		stats.ProfitFactor = sumWins.Div(sumLosses)
	}
	if !startingEquity.IsZero() {
		stats.TotalReturnPct = equity.Sub(startingEquity).Div(startingEquity).Mul(decimal.NewFromInt(100))
	}
	stats.MaxDrawdownPct = maxDD.Mul(decimal.NewFromInt(100))
	if rCount > 0 {
		stats.AvgRMultiple = sumR.Div(decimal.NewFromInt(int64(rCount)))
	}
	return stats
}

func (s Stats) String() string {
	return fmt.Sprintf(
		"сделок %d (побед %d / убытков %d, winrate %s%%) | profit factor %s | средний R %s | "+
			"эквити %s → %s (%s%%) | макс. просадка %s%%",
		s.TradeCount, s.Wins, s.Losses, s.WinRatePct.StringFixed(1),
		s.ProfitFactor.StringFixed(2), s.AvgRMultiple.StringFixed(2),
		s.StartingEquity.StringFixed(2), s.FinalEquity.StringFixed(2), s.TotalReturnPct.StringFixed(2),
		s.MaxDrawdownPct.StringFixed(2))
}
