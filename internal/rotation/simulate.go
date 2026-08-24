package rotation

import (
	"fmt"
	"sort"
	"time"

	"github.com/shopspring/decimal"
)

// Params — конфигурация симуляции ротации (см. cmd/rotation для смысла полей
// и cmd/regime, где Simulate переиспользуется для блоков "ranging"-режима).
type Params struct {
	LookbackDays   int
	TopK           int
	RebalanceEvery int
}

func (p Params) String() string {
	return fmt.Sprintf("lookback=%dд topK=%d rebalance=%dд", p.LookbackDays, p.TopK, p.RebalanceEvery)
}

// DailyBar — одна дневная свеча, только то, что нужно для ранжирования по доходности.
type DailyBar struct {
	T     time.Time
	Close decimal.Decimal
}

// Stats — итог одного прогона Simulate.
type Stats struct {
	StartEquity   decimal.Decimal
	FinalEquity   decimal.Decimal
	MaxDrawdown   decimal.Decimal // доля, 0..1
	NumRebalances int
}

func (s Stats) ReturnPct() decimal.Decimal {
	if !s.StartEquity.IsPositive() {
		return decimal.Zero
	}
	return s.FinalEquity.Sub(s.StartEquity).Div(s.StartEquity).Mul(decimal.NewFromInt(100))
}

func (s Stats) String() string {
	return fmt.Sprintf("ребалансировок %d | эквити %s → %s (%s%%) | макс. просадка %s%%",
		s.NumRebalances, s.StartEquity.StringFixed(2), s.FinalEquity.StringFixed(2),
		s.ReturnPct().StringFixed(2), s.MaxDrawdown.Mul(decimal.NewFromInt(100)).StringFixed(2))
}

// Score — та же формула, что и в cmd/tune: доходность минус половина просадки
// в процентных пунктах, штрафует за просадку, но не убивает результат целиком.
func (s Stats) Score() decimal.Decimal {
	ddPct := s.MaxDrawdown.Mul(decimal.NewFromInt(100))
	return s.ReturnPct().Sub(ddPct.Mul(decimal.NewFromFloat(0.5)))
}

// Simulate прогоняет ротацию по индексам [startIdx, endIdx) общего массива
// times/closes — вызывающий отвечает за то, что все closes[symbol] обрезаны
// до одной длины и выровнены по индексу на одни и те же даты (как делает
// cmd/rotation/main.go перед вызовом). Ребалансировка на каждом shift-е
// p.RebalanceEvery дней: сначала закрываются все текущие позиции по цене
// close текущего дня, затем на основе доходности за p.LookbackDays дней (до
// и включая текущий день) открываются новые — лонг топ-K по доходности, шорт
// последних K. Позиции равновзвешены, суммарная нога лонгов = суммарной ноге
// шортов (портфель рыночно-нейтральный). Комиссия и проскальзывание — на
// каждую сторону каждой сделки.
//
// startEquity — точка отсчёта эквити для этого прогона. Вынесено в параметр
// (а не константа), потому что cmd/regime цепочкой прогоняет блоки Breakout и
// Rotation друг за другом на ОБЩЕМ эквити: конец одного блока становится
// startEquity следующего, какая бы стратегия его ни вела.
func Simulate(closes map[string][]DailyBar, symbols []string, startIdx, endIdx int, p Params, startEquity, feeRate, slippagePct decimal.Decimal) Stats {
	equity := startEquity
	peak := startEquity
	maxDD := decimal.Zero

	positions := make(map[string]decimal.Decimal)   // symbol -> знаковый qty
	entryPrices := make(map[string]decimal.Decimal) // symbol -> цена входа с учётом slippage

	closePos := func(i int) {
		for s, qty := range positions {
			if qty.IsZero() {
				continue
			}
			price := closes[s][i].Close
			execPrice := price
			if qty.IsPositive() {
				execPrice = price.Mul(decimal.NewFromInt(1).Sub(slippagePct)) // закрытие лонга — SELL
			} else {
				execPrice = price.Mul(decimal.NewFromInt(1).Add(slippagePct)) // закрытие шорта — BUY
			}
			fee := qty.Abs().Mul(execPrice).Mul(feeRate)
			pnl := qty.Mul(execPrice.Sub(entryPrices[s])).Sub(fee)
			equity = equity.Add(pnl)
		}
		positions = make(map[string]decimal.Decimal)
		entryPrices = make(map[string]decimal.Decimal)
	}

	numRebalances := 0
	lastTradedIdx := -1
	for i := startIdx; i < endIdx; i += p.RebalanceEvery {
		if i-p.LookbackDays < 0 {
			continue
		}
		closePos(i)
		if !equity.IsPositive() {
			break // счёт обнулён — дальше моделировать нечего
		}

		type ret struct {
			symbol string
			r      decimal.Decimal
		}
		rets := make([]ret, 0, len(symbols))
		for _, s := range symbols {
			c0 := closes[s][i-p.LookbackDays].Close
			c1 := closes[s][i].Close
			if c0.IsZero() {
				continue
			}
			rets = append(rets, ret{s, c1.Sub(c0).Div(c0)})
		}
		sort.Slice(rets, func(a, b int) bool { return rets[a].r.GreaterThan(rets[b].r) })

		k := p.TopK
		if 2*k > len(rets) {
			k = len(rets) / 2
		}
		if k < 1 {
			continue
		}
		longs := rets[:k]
		shorts := rets[len(rets)-k:]

		notionalPerLeg := equity.Div(decimal.NewFromInt(int64(2 * k)))
		open := func(symbol string, long bool) {
			price := closes[symbol][i].Close
			var execPrice, qty decimal.Decimal
			if long {
				execPrice = price.Mul(decimal.NewFromInt(1).Add(slippagePct))
				qty = notionalPerLeg.Div(execPrice)
			} else {
				execPrice = price.Mul(decimal.NewFromInt(1).Sub(slippagePct))
				qty = notionalPerLeg.Div(execPrice).Neg()
			}
			fee := qty.Abs().Mul(execPrice).Mul(feeRate)
			equity = equity.Sub(fee)
			positions[symbol] = qty
			entryPrices[symbol] = execPrice
		}
		for _, l := range longs {
			open(l.symbol, true)
		}
		for _, sh := range shorts {
			open(sh.symbol, false)
		}

		if equity.GreaterThan(peak) {
			peak = equity
		}
		if peak.IsPositive() {
			if dd := peak.Sub(equity).Div(peak); dd.GreaterThan(maxDD) {
				maxDD = dd
			}
		}
		numRebalances++
		lastTradedIdx = i
	}

	if lastTradedIdx >= 0 {
		closePos(minInt(lastTradedIdx+p.RebalanceEvery, endIdx-1))
	}

	return Stats{StartEquity: startEquity, FinalEquity: equity, MaxDrawdown: maxDD, NumRebalances: numRebalances}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
