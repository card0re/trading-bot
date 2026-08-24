// Команда rotation бэктестит принципиально другой класс стратегии, чем
// internal/strategy.Breakout: кросс-секционную ротацию по относительной
// силе (cross-sectional momentum). Направленный пробой (long/short по
// тренду одной монеты) после исчерпывающей walk-forward проверки (24.08.2026,
// 6 независимых вариантов: пробой, +ADX, +безубыток, +возврат к среднему,
// 4h) стабильно не показывает эджа на последних 6 месяцах — рынку не хватает
// именно НАПРАВЛЕННОГО тренда. Ротация ставит не на направление рынка
// целиком, а на РАЗНИЦУ между монетами: лонг тех, что растут быстрее
// остальных, шорт тех, что медленнее (или падают быстрее) — портфель
// рыночно-нейтральный (сумма лонгов = сумме шортов), в теории может
// зарабатывать даже когда рынок в среднем стоит на месте, если между
// монетами есть разброс силы. Это не вариация прежней стратегии, а другой
// источник эджа: дисперсия между активами вместо направления рынка.
//
// Упрощение (сознательное): в отличие от Breakout, здесь НЕТ стопа на
// отдельную позицию — риск ограничивается только периодичностью
// ребалансировки и диверсификацией (topK). Это стандартно для этого класса
// стратегий (риск распределён по многим позициям, а не контролируется
// на каждой отдельно), но при малом topK одна нога может двигаться против
// нас всю неделю без остановки — реальный риск, видно по просадке в статистике.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"

	"trading-bot/internal/domain"
	exchange "trading-bot/internal/exchange/binance"

	"github.com/adshao/go-binance/v2"
	"github.com/adshao/go-binance/v2/futures"
	"github.com/shopspring/decimal"
)

// Шире, чем в cmd/tune: ротация ставит на РАЗНИЦУ между монетами, поэтому
// чем больше кандидатов для сравнения — тем осмысленнее ранжирование.
// DOGE/AVAX/LTC раньше исключили из направленной стратегии (стабильно в
// минусе на пробое) — здесь это не повод исключать их снова: "плохая для
// тренда" не значит "плохая для относительного ранжирования против других".
var symbols = []string{
	"BTCUSDT", "ETHUSDT", "BNBUSDT", "SOLUSDT", "XRPUSDT", "ADAUSDT", "LINKUSDT",
	"DOGEUSDT", "AVAXUSDT", "LTCUSDT", "DOTUSDT", "ATOMUSDT", "TRXUSDT", "ETCUSDT",
	"XLMUSDT", "UNIUSDT", "NEARUSDT", "FILUSDT", "ICPUSDT",
}

var (
	startEquityF   = 10000.0
	feeRatePerLegF = 0.0005 // как backtest.TakerFeeRate
	slippagePctF   = 0.0002 // как backtest.DefaultSlippagePct
)

type params struct {
	lookbackDays   int // окно расчёта относительной доходности
	topK           int // сколько лонгов и сколько шортов одновременно
	rebalanceEvery int // раз в сколько дней пересобирать портфель
}

func (p params) String() string {
	return fmt.Sprintf("lookback=%dд topK=%d rebalance=%dд", p.lookbackDays, p.topK, p.rebalanceEvery)
}

type dailyBar struct {
	t     time.Time
	close decimal.Decimal
}

func main() {
	cacheDir := flag.String("cache", filepath.Join(os.TempDir(), "trading-bot-rotation-cache"), "каталог кэша дневных свечей")
	days := flag.Int("days", 1095, "глубина истории, дней")
	holdout := flag.Int("holdout", 180, "размер отложенного окна, дней")
	folds := flag.Int("folds", 3, "число периодов walk-forward")
	top := flag.Int("top", 10, "сколько лучших по in-sample перепроверять на holdout")
	flag.Parse()

	if err := run(*cacheDir, *days, *holdout, *folds, *top); err != nil {
		log.Fatalf("❌ %v", err)
	}
}

func run(cacheDir string, totalDays, holdoutDays, folds, topN int) error {
	futures.UseTestnet = false
	client := binance.NewFuturesClient("", "")
	ctx := context.Background()

	end := time.Now()
	start := end.AddDate(0, 0, -totalDays)

	fmt.Printf("📥 Дневные свечи, %d дней, holdout — последние %d дней:\n", totalDays, holdoutDays)
	closes := make(map[string][]dailyBar, len(symbols))
	var usable []string
	for _, s := range symbols {
		bars, err := loadOrFetchDaily(ctx, client, cacheDir, s, start, end, totalDays)
		if err != nil {
			// Не валим весь прогон из-за одного проблемного символа (не
			// торгуется на фьючерсах, переименован и т.п.) — пропускаем его.
			fmt.Printf("  ⚠️  %s: пропущен (%v)\n", s, err)
			continue
		}
		if len(bars) < totalDays/2 {
			fmt.Printf("  ⚠️  %s: маловато истории (%d свечей), пропущен\n", s, len(bars))
			continue
		}
		closes[s] = bars
		usable = append(usable, s)
		fmt.Printf("  %s: %d свечей\n", s, len(bars))
	}
	symbols = usable
	if len(symbols) < 4 {
		return fmt.Errorf("после фильтрации осталось %d символов — маловато для ранжирования", len(symbols))
	}

	// Разъехавшиеся по длине ряды (если у монеты история короче остальных)
	// обрезаем по минимальной длине с конца — без интерполяции.
	n := len(closes[symbols[0]])
	for _, s := range symbols {
		if len(closes[s]) < n {
			n = len(closes[s])
		}
	}
	for _, s := range symbols {
		closes[s] = closes[s][len(closes[s])-n:]
	}
	times := make([]time.Time, n)
	for i := range times {
		times[i] = closes[symbols[0]][i].t
	}

	splitIdx := sort.Search(n, func(i int) bool { return !times[i].Before(end.AddDate(0, 0, -holdoutDays)) })

	grid := buildGrid()
	fmt.Printf("\n🔍 Комбинаций в сетке: %d\n\n", len(grid))

	type scored struct {
		p     params
		stats rotationStats
	}
	var results []scored
	for _, p := range grid {
		st := simulate(closes, p.lookbackDays, splitIdx, p)
		if st.numRebalances < 10 {
			continue // слишком мало ребалансировок — статистика ничего не значит
		}
		results = append(results, scored{p, st})
	}
	if len(results) == 0 {
		fmt.Println("⚠️  Ни одна комбинация не набрала минимум ребалансировок.")
		return nil
	}
	sort.Slice(results, func(i, j int) bool { return results[i].stats.score().GreaterThan(results[j].stats.score()) })

	fmt.Println("🏆 Топ по in-sample (грубая прикидка, ещё НЕ финальный выбор):")
	top := results
	if len(top) > topN {
		top = top[:topN]
	}
	for i, r := range top {
		fmt.Printf("%2d. %s | %s\n", i+1, r.p, r.stats)
	}

	foldBounds := buildFoldBounds(times, splitIdx, n, folds)
	fmt.Printf("\n🧪 Walk-forward проверка на holdout (последние %d дней, разбито на %d период(ов)):\n", holdoutDays, folds)

	type validated struct {
		p          params
		foldStats  []rotationStats
		foldScores []decimal.Decimal
		agg        decimal.Decimal
	}
	var withHoldout []validated
	for _, r := range top {
		v := validated{p: r.p}
		for _, fb := range foldBounds {
			st := simulate(closes, fb.start, fb.end, r.p)
			v.foldStats = append(v.foldStats, st)
			v.foldScores = append(v.foldScores, st.score())
		}
		v.agg = meanMinusStdev(v.foldScores)
		withHoldout = append(withHoldout, v)
	}
	sort.Slice(withHoldout, func(i, j int) bool { return withHoldout[i].agg.GreaterThan(withHoldout[j].agg) })

	for i, v := range withHoldout {
		fmt.Printf("\n%2d. %s | walk-forward score=%s\n", i+1, v.p, v.agg.StringFixed(2))
		for fi, st := range v.foldStats {
			fmt.Printf("    фолд %d (%s..%s): %s\n",
				fi+1, times[foldBounds[fi].start].Format("2006-01-02"), times[foldBounds[fi].end-1].Format("2006-01-02"), st)
		}
	}

	best := withHoldout[0]
	fmt.Printf("\n✅ Лучшая по walk-forward: %s\n", best.p)
	return nil
}

// rotationStats — итог одного прогона симуляции.
type rotationStats struct {
	startEquity   decimal.Decimal
	finalEquity   decimal.Decimal
	maxDrawdown   decimal.Decimal // доля, 0..1
	numRebalances int
}

func (s rotationStats) String() string {
	ret := decimal.Zero
	if s.startEquity.IsPositive() {
		ret = s.finalEquity.Sub(s.startEquity).Div(s.startEquity).Mul(decimal.NewFromInt(100))
	}
	return fmt.Sprintf("ребалансировок %d | эквити %s → %s (%s%%) | макс. просадка %s%%",
		s.numRebalances, s.startEquity.StringFixed(2), s.finalEquity.StringFixed(2),
		ret.StringFixed(2), s.maxDrawdown.Mul(decimal.NewFromInt(100)).StringFixed(2))
}

func (s rotationStats) score() decimal.Decimal {
	ret := decimal.Zero
	if s.startEquity.IsPositive() {
		ret = s.finalEquity.Sub(s.startEquity).Div(s.startEquity).Mul(decimal.NewFromInt(100))
	}
	ddPct := s.maxDrawdown.Mul(decimal.NewFromInt(100))
	return ret.Sub(ddPct.Mul(decimal.NewFromFloat(0.5)))
}

// simulate прогоняет ротацию по индексам [startIdx, endIdx) общего массива
// times/closes. Ребалансировка на каждом shift-е P.rebalanceEvery дней:
// сначала закрываются все текущие позиции по цене close текущего дня, затем
// на основе доходности за p.lookbackDays дней (до и включая текущий день)
// открываются новые — лонг топ-K по доходности, шорт последних K.
// Позиции равновзвешены, суммарная нога лонгов = суммарной ноге шортов
// (портфель рыночно-нейтральный). Комиссия и проскальзывание — на каждую
// сторону каждой сделки, как и в internal/backtest.
func simulate(closes map[string][]dailyBar, startIdx, endIdx int, p params) rotationStats {
	startEquity := decimal.NewFromFloat(startEquityF)
	feeRate := decimal.NewFromFloat(feeRatePerLegF)
	slip := decimal.NewFromFloat(slippagePctF)

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
			price := closes[s][i].close
			execPrice := price
			if qty.IsPositive() {
				execPrice = price.Mul(decimal.NewFromInt(1).Sub(slip)) // закрытие лонга — SELL
			} else {
				execPrice = price.Mul(decimal.NewFromInt(1).Add(slip)) // закрытие шорта — BUY
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
	for i := startIdx; i < endIdx; i += p.rebalanceEvery {
		if i-p.lookbackDays < 0 {
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
			c0 := closes[s][i-p.lookbackDays].close
			c1 := closes[s][i].close
			if c0.IsZero() {
				continue
			}
			rets = append(rets, ret{s, c1.Sub(c0).Div(c0)})
		}
		sort.Slice(rets, func(a, b int) bool { return rets[a].r.GreaterThan(rets[b].r) })

		k := p.topK
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
			price := closes[symbol][i].close
			var execPrice, qty decimal.Decimal
			if long {
				execPrice = price.Mul(decimal.NewFromInt(1).Add(slip))
				qty = notionalPerLeg.Div(execPrice)
			} else {
				execPrice = price.Mul(decimal.NewFromInt(1).Sub(slip))
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
		closePos(minInt(lastTradedIdx+p.rebalanceEvery, endIdx-1))
	}

	return rotationStats{startEquity: startEquity, finalEquity: equity, maxDrawdown: maxDD, numRebalances: numRebalances}
}

// foldBound — индексы [start, end) в общем массиве times.
type foldBound struct{ start, end int }

func buildFoldBounds(times []time.Time, splitIdx, n, folds int) []foldBound {
	holdoutLen := n - splitIdx
	step := holdoutLen / folds
	bounds := make([]foldBound, folds)
	for i := 0; i < folds; i++ {
		bounds[i].start = splitIdx + i*step
		if i == folds-1 {
			bounds[i].end = n
		} else {
			bounds[i].end = splitIdx + (i+1)*step
		}
	}
	return bounds
}

func meanMinusStdev(scores []decimal.Decimal) decimal.Decimal {
	if len(scores) == 0 {
		return decimal.Zero
	}
	if len(scores) == 1 {
		return scores[0]
	}
	vals := make([]float64, len(scores))
	sum := 0.0
	for i, s := range scores {
		f, _ := s.Float64()
		vals[i] = f
		sum += f
	}
	mean := sum / float64(len(vals))
	var variance float64
	for _, v := range vals {
		d := v - mean
		variance += d * d
	}
	variance /= float64(len(vals))
	return decimal.NewFromFloat(mean - math.Sqrt(variance))
}

// buildGrid — небольшая сетка: окно доходности, широта портфеля (сколько
// лонгов/шортов) и частота ребалансировки.
func buildGrid() []params {
	lookbacks := []int{3, 5, 7, 10, 14, 20}
	topKs := []int{1, 2, 3, 4, 5}
	rebalances := []int{1, 3, 7}

	var grid []params
	for _, lb := range lookbacks {
		for _, k := range topKs {
			for _, rb := range rebalances {
				grid = append(grid, params{lookbackDays: lb, topK: k, rebalanceEvery: rb})
			}
		}
	}
	return grid
}

func loadOrFetchDaily(ctx context.Context, client *futures.Client, cacheDir, symbol string, start, end time.Time, totalDays int) ([]dailyBar, error) {
	path := filepath.Join(cacheDir, fmt.Sprintf("%s_1d_%dd.json", symbol, totalDays))
	if data, err := os.ReadFile(path); err == nil {
		var candles []domain.Candle
		if err := json.Unmarshal(data, &candles); err == nil && len(candles) > 0 {
			return toDailyBars(candles), nil
		}
	}

	candles, err := exchange.LoadHistoricalCandles(ctx, client, symbol, "1d", start, end)
	if err != nil {
		return nil, err
	}
	if data, err := json.Marshal(candles); err == nil {
		if err := os.MkdirAll(cacheDir, 0o755); err == nil {
			_ = os.WriteFile(path, data, 0o644)
		}
	}
	return toDailyBars(candles), nil
}

func toDailyBars(candles []domain.Candle) []dailyBar {
	bars := make([]dailyBar, len(candles))
	for i, c := range candles {
		bars[i] = dailyBar{t: c.CloseTime, close: c.Close}
	}
	return bars
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
