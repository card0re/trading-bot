// Package pairs — статистический арбитраж по высококоррелированным парам
// монет: лог-спред между ценами двух активов возвращается к своему
// скользящему среднему — вход при сильном отклонении (z-score), выход при
// возврате, по стопу времени или по стопу дивергенции. Дольнар-нейтрально
// (лонг одной ноги, шорт другой на равный номинал), как и internal/rotation,
// но источник эджа другой: временное расхождение КОНКРЕТНОЙ пары, а не
// относительный ранг всей корзины. Отличается и от single-symbol
// mean-reversion внутри internal/strategy.Breakout (уже проверен и отклонён
// — там "среднее" это собственная EMA монеты, здесь — отношение между двумя
// разными монетами).
//
// Корреляция вместо полноценной коинтеграции (без теста Engle-Granger/ADF):
// сознательное упрощение — коинтеграция статистически строже, но требует
// регрессии остатков и таблиц критических значений, а высокая корреляция
// дневных доходностей — стандартный, гораздо более дешёвый практический
// прокси для "эти два актива обычно двигаются вместе", которого достаточно,
// чтобы проверить саму идею.
package pairs

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/shopspring/decimal"
)

// DailyBar — одна дневная свеча, только то, что нужно для спреда.
type DailyBar struct {
	T     time.Time
	Close decimal.Decimal
}

// Pair — одна отобранная пара символов.
type Pair struct {
	A, B        string
	Correlation float64 // корреляция дневных лог-доходностей за период отбора, для отчёта
}

func (p Pair) String() string {
	return fmt.Sprintf("%s/%s (corr=%.2f)", p.A, p.B, p.Correlation)
}

// SelectPairs считает корреляцию Пирсона дневных лог-доходностей для каждой
// пары символов на [startIdx, endIdx) и возвращает topN пар с наибольшей
// корреляцией. Отбор нужно делать ТОЛЬКО на in-sample окне — вызывающий
// отвечает за то, чтобы endIdx не заходил в holdout, иначе отбор пар сам по
// себе станет утечкой будущего в прошлое (holdout должен проверять пары,
// отобранные БЕЗ подглядывания в него).
func SelectPairs(daily map[string][]DailyBar, symbols []string, startIdx, endIdx, topN int) []Pair {
	rets := make(map[string][]float64, len(symbols))
	for _, s := range symbols {
		bars := daily[s]
		r := make([]float64, 0, endIdx-startIdx)
		for i := startIdx + 1; i < endIdx && i < len(bars); i++ {
			c0, _ := bars[i-1].Close.Float64()
			c1, _ := bars[i].Close.Float64()
			if c0 <= 0 || c1 <= 0 {
				r = append(r, math.NaN())
				continue
			}
			r = append(r, math.Log(c1/c0))
		}
		rets[s] = r
	}

	var pairs []Pair
	for i := 0; i < len(symbols); i++ {
		for j := i + 1; j < len(symbols); j++ {
			a, b := symbols[i], symbols[j]
			pairs = append(pairs, Pair{A: a, B: b, Correlation: pearson(rets[a], rets[b])})
		}
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].Correlation > pairs[j].Correlation })
	if len(pairs) > topN {
		pairs = pairs[:topN]
	}
	return pairs
}

func pearson(a, b []float64) float64 {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	if n < 2 {
		return 0
	}
	var sumA, sumB float64
	valid := 0
	for i := 0; i < n; i++ {
		if math.IsNaN(a[i]) || math.IsNaN(b[i]) {
			continue
		}
		sumA += a[i]
		sumB += b[i]
		valid++
	}
	if valid < 2 {
		return 0
	}
	meanA, meanB := sumA/float64(valid), sumB/float64(valid)
	var cov, varA, varB float64
	for i := 0; i < n; i++ {
		if math.IsNaN(a[i]) || math.IsNaN(b[i]) {
			continue
		}
		da, db := a[i]-meanA, b[i]-meanB
		cov += da * db
		varA += da * da
		varB += db * db
	}
	if varA == 0 || varB == 0 {
		return 0
	}
	return cov / math.Sqrt(varA*varB)
}

// Params — конфигурация симуляции стат-арба по парам.
type Params struct {
	LookbackDays  int     // окно скользящего среднего/стандартного отклонения спреда
	EntryZ        float64 // |z| >= EntryZ — открыть позицию
	ExitZ         float64 // |z| <= ExitZ — закрыть, спред вернулся
	StopZ         float64 // |z| >= StopZ — закрыть принудительно, дивергенция не развернулась (должен быть > EntryZ)
	MaxHoldDays   int     // стоп по времени — верхняя граница риска без стопа по цене
	MaxConcurrent int     // сколько пар могут быть открыты одновременно
}

func (p Params) String() string {
	return fmt.Sprintf("lookback=%dд entryZ=%.1f exitZ=%.1f stopZ=%.1f maxHold=%dд maxConcurrent=%d",
		p.LookbackDays, p.EntryZ, p.ExitZ, p.StopZ, p.MaxHoldDays, p.MaxConcurrent)
}

// Stats — итог одного прогона Simulate.
type Stats struct {
	StartEquity, FinalEquity, MaxDrawdown decimal.Decimal
	NumTrades                             int
}

func (s Stats) ReturnPct() decimal.Decimal {
	if !s.StartEquity.IsPositive() {
		return decimal.Zero
	}
	return s.FinalEquity.Sub(s.StartEquity).Div(s.StartEquity).Mul(decimal.NewFromInt(100))
}

func (s Stats) Score() decimal.Decimal {
	return s.ReturnPct().Sub(s.MaxDrawdown.Mul(decimal.NewFromInt(100)).Mul(decimal.NewFromFloat(0.5)))
}

func (s Stats) String() string {
	return fmt.Sprintf("сделок %d | эквити %s → %s (%s%%) | макс. просадка %s%%",
		s.NumTrades, s.StartEquity.StringFixed(2), s.FinalEquity.StringFixed(2),
		s.ReturnPct().StringFixed(2), s.MaxDrawdown.Mul(decimal.NewFromInt(100)).StringFixed(2))
}

type openPos struct {
	longSym, shortSym     string
	longQty, shortQty     decimal.Decimal
	longEntry, shortEntry decimal.Decimal
	openedDay             int
}

// Simulate прогоняет стат-арб по фиксированному списку pairs на [startIdx,
// endIdx) общего дневного индекса. Каждый день: сначала проверяются открытые
// позиции на выход (реверсия/стоп времени/стоп дивергенции), затем среди пар
// без открытой позиции ищутся новые входы (|z| >= EntryZ), приоритет —
// самому растянутому спреду, если свободных слотов (MaxConcurrent) меньше,
// чем сигналов. Просадка считается по РЕАЛИЗОВАННОМУ эквити (после закрытия
// сделок), не по марк-ту-маркету открытых позиций между входом и выходом —
// то же упрощение, что и в internal/rotation.Simulate, тем же обоснованием:
// консервативная (не заниженная) оценка порядка величины просадки.
func Simulate(daily map[string][]DailyBar, pairs []Pair, startIdx, endIdx int, p Params, startEquity, feeRate, slippagePct decimal.Decimal) Stats {
	equity := startEquity
	peak := startEquity
	maxDD := decimal.Zero
	numTrades := 0

	open := make(map[int]*openPos, p.MaxConcurrent)
	zSeries := make([][]float64, len(pairs))
	for pi, pr := range pairs {
		zSeries[pi] = rollingZ(daily[pr.A], daily[pr.B], p.LookbackDays)
	}
	seriesLen := func(pi int) int { return len(zSeries[pi]) }

	closePos := func(pi, day int) {
		pos := open[pi]
		if pos == nil {
			return
		}
		longClose := daily[pos.longSym][day].Close.Mul(decimal.NewFromInt(1).Sub(slippagePct))
		shortClose := daily[pos.shortSym][day].Close.Mul(decimal.NewFromInt(1).Add(slippagePct))
		longFee := pos.longQty.Mul(longClose).Mul(feeRate)
		shortFee := pos.shortQty.Mul(shortClose).Mul(feeRate)
		longPnL := pos.longQty.Mul(longClose.Sub(pos.longEntry)).Sub(longFee)
		shortPnL := pos.shortQty.Mul(pos.shortEntry.Sub(shortClose)).Sub(shortFee)
		equity = equity.Add(longPnL).Add(shortPnL)
		delete(open, pi)
	}

	for day := startIdx; day < endIdx; day++ {
		if !equity.IsPositive() {
			break
		}

		for pi := range open {
			if day >= seriesLen(pi) {
				continue
			}
			z := zSeries[pi][day]
			pos := open[pi]
			held := day - pos.openedDay
			if math.Abs(z) <= p.ExitZ || held >= p.MaxHoldDays || math.Abs(z) >= p.StopZ {
				closePos(pi, day)
			}
		}

		type candidate struct {
			pi int
			z  float64
		}
		var candidates []candidate
		for pi := range pairs {
			if open[pi] != nil || day >= seriesLen(pi) {
				continue
			}
			z := zSeries[pi][day]
			if math.IsNaN(z) || math.Abs(z) < p.EntryZ {
				continue
			}
			candidates = append(candidates, candidate{pi, z})
		}
		sort.Slice(candidates, func(i, j int) bool { return math.Abs(candidates[i].z) > math.Abs(candidates[j].z) })

		slotsFree := p.MaxConcurrent - len(open)
		for _, c := range candidates {
			if slotsFree <= 0 {
				break
			}
			pr := pairs[c.pi]
			notionalPerLeg := equity.Div(decimal.NewFromInt(int64(2 * p.MaxConcurrent)))
			// Спред = ln(A) - ln(B). z > 0 значит A относительно дорог против
			// B прямо сейчас — шортим A, лонгуем B, ставим на схождение обратно.
			longSym, shortSym := pr.A, pr.B
			if c.z > 0 {
				longSym, shortSym = pr.B, pr.A
			}
			longPrice := daily[longSym][day].Close.Mul(decimal.NewFromInt(1).Add(slippagePct))
			shortPrice := daily[shortSym][day].Close.Mul(decimal.NewFromInt(1).Sub(slippagePct))
			longQty := notionalPerLeg.Div(longPrice)
			shortQty := notionalPerLeg.Div(shortPrice)
			fee := longQty.Mul(longPrice).Mul(feeRate).Add(shortQty.Mul(shortPrice).Mul(feeRate))
			equity = equity.Sub(fee)
			open[c.pi] = &openPos{longSym: longSym, shortSym: shortSym, longQty: longQty, shortQty: shortQty,
				longEntry: longPrice, shortEntry: shortPrice, openedDay: day}
			numTrades++
			slotsFree--
		}

		if equity.GreaterThan(peak) {
			peak = equity
		}
		if peak.IsPositive() {
			if dd := peak.Sub(equity).Div(peak); dd.GreaterThan(maxDD) {
				maxDD = dd
			}
		}
	}

	lastDay := endIdx - 1
	for pi := range open {
		d := lastDay
		if n := seriesLen(pi); n > 0 && d >= n {
			d = n - 1
		}
		closePos(pi, d)
	}

	return Stats{StartEquity: startEquity, FinalEquity: equity, MaxDrawdown: maxDD, NumTrades: numTrades}
}

// rollingZ считает z-score лог-спреда ln(A)-ln(B) относительно его
// скользящего среднего/стандартного отклонения за предыдущие window дней
// (окно строго ДО текущего дня — без заглядывания в сам день i). Первые
// window значений — NaN, спред ещё не прогрет.
func rollingZ(a, b []DailyBar, window int) []float64 {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	spread := make([]float64, n)
	for i := 0; i < n; i++ {
		ca, _ := a[i].Close.Float64()
		cb, _ := b[i].Close.Float64()
		if ca <= 0 || cb <= 0 {
			spread[i] = math.NaN()
			continue
		}
		spread[i] = math.Log(ca) - math.Log(cb)
	}

	z := make([]float64, n)
	for i := range z {
		if i < window {
			z[i] = math.NaN()
			continue
		}
		mean, sd := meanStdev(spread[i-window : i])
		if sd == 0 || math.IsNaN(mean) || math.IsNaN(spread[i]) {
			z[i] = math.NaN()
			continue
		}
		z[i] = (spread[i] - mean) / sd
	}
	return z
}

func meanStdev(xs []float64) (mean, sd float64) {
	var sum float64
	valid := 0
	for _, x := range xs {
		if math.IsNaN(x) {
			continue
		}
		sum += x
		valid++
	}
	if valid == 0 {
		return math.NaN(), math.NaN()
	}
	mean = sum / float64(valid)
	var varSum float64
	for _, x := range xs {
		if math.IsNaN(x) {
			continue
		}
		d := x - mean
		varSum += d * d
	}
	return mean, math.Sqrt(varSum / float64(valid))
}
