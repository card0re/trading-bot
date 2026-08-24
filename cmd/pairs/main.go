// Команда pairs бэктестит стат-арбитраж по парам монет (internal/pairs) —
// третий, независимо проверяемый класс стратегии этой сессии, после
// направленного пробоя (internal/strategy.Breakout) и кросс-секционной
// ротации (internal/rotation). Источник эджа снова другой: не направление
// рынка (пробой) и не относительный ранг всей корзины (ротация), а временное
// расхождение КОНКРЕТНОЙ пары монет от их обычного соотношения.
//
// Методология walk-forward та же, что и у cmd/tune/cmd/rotation/cmd/regime:
// сетка оценивается in-sample, топ-кандидаты перепроверяются на отложенном
// holdout, разбитом на несколько последовательных фолдов. Отдельная
// тонкость именно для пар: САМИ ПАРЫ отбираются (internal/pairs.SelectPairs)
// ОДИН РАЗ, только на in-sample данных, и остаются зафиксированными на
// holdout — если бы пары выбирались заново на каждом фолде (в т.ч. видя его
// собственные данные), это была бы утечка будущего в отбор, а не только в
// параметры.
//
// ИТОГ (25.08.2026): НЕ ПОДТВЕРЖДЕНО, но поучительно КАК именно. Первый
// прогон (-holdout 180 -folds 3, 3 грубых фолда по ~60д) выглядел
// многообещающе: лучший кандидат был прибылен на КАЖДОМ из 3 фолдов
// (+1.9%/+2.3%/+14.6%), отрицательный "walk-forward score" там был чисто
// артефактом штрафа за разброс между фолдами, а не убытков. Второй, более
// придирчивый прогон (-holdout 365 -folds 6, 6 фолдов по ~60д на вдвое
// большем holdout) это опроверг: внутри одного из грубых 60-дневных фолдов
// первого прогона пряталась реальная убыточная полоса (окт.2025-фев.2026,
// -5%..-14% почти у всех топ-кандидатов на более мелкой нарезке) — на более
// тонких границах она стала видна. Вывод: сначала многообещающий результат
// не пережил ужесточения walk-forward — граница фолда была просто удачной,
// не сама идея надёжной. НЕ ДЕПЛОИТЬ. Методологический урок для будущих
// идей в этом репо: недостаточно одного разбиения на фолды — стоит
// перепроверять на нескольких разных нарезках (folds/holdout), прежде чем
// доверять "прибылен на каждом фолде" как признаку устойчивости.
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
	"trading-bot/internal/pairs"

	"github.com/adshao/go-binance/v2"
	"github.com/adshao/go-binance/v2/futures"
	"github.com/shopspring/decimal"
)

// symbols — тот же широкий 19-символьный список, что и у cmd/rotation: для
// отбора пар важно количество кандидатов (21 пара из 7 символов против 171
// из 19), а не то, что именно живёт сейчас на testnet.
var symbols = []string{
	"BTCUSDT", "ETHUSDT", "BNBUSDT", "SOLUSDT", "XRPUSDT", "ADAUSDT", "LINKUSDT",
	"DOGEUSDT", "AVAXUSDT", "LTCUSDT", "DOTUSDT", "ATOMUSDT", "TRXUSDT", "ETCUSDT",
	"XLMUSDT", "UNIUSDT", "NEARUSDT", "FILUSDT", "ICPUSDT",
}

const (
	startEquityF  = 10000.0
	minTrades     = 10 // отсекаем комбинации, где почти нет сделок — статистика ничего не значит
	topNPairs     = 5  // сколько самых коррелированных пар отбирается (не перебирается сеткой — иначе комбинаторный взрыв)
	feeRatePerLeg = 0.0005
	slippagePct   = 0.0002
)

func main() {
	cacheDir := flag.String("cache", filepath.Join(os.TempDir(), "trading-bot-pairs-cache"), "каталог кэша дневных свечей")
	days := flag.Int("days", 1095, "глубина истории, дней")
	holdout := flag.Int("holdout", 180, "размер отложенного окна, дней")
	folds := flag.Int("folds", 3, "число периодов walk-forward")
	top := flag.Int("top", 10, "сколько лучших по in-sample комбинаций перепроверять на holdout")
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
	closesRaw := make(map[string][]pairs.DailyBar, len(symbols))
	var usable []string
	for _, s := range symbols {
		bars, err := loadOrFetchDaily(ctx, client, cacheDir, s, start, end, totalDays)
		if err != nil {
			fmt.Printf("  ⚠️  %s: пропущен (%v)\n", s, err)
			continue
		}
		if len(bars) < totalDays/2 {
			fmt.Printf("  ⚠️  %s: маловато истории (%d свечей), пропущен\n", s, len(bars))
			continue
		}
		closesRaw[s] = bars
		usable = append(usable, s)
	}
	symbols = usable
	if len(symbols) < 4 {
		return fmt.Errorf("после фильтрации осталось %d символов — маловато для отбора пар", len(symbols))
	}

	n := len(closesRaw[symbols[0]])
	for _, s := range symbols {
		if len(closesRaw[s]) < n {
			n = len(closesRaw[s])
		}
	}
	daily := make(map[string][]pairs.DailyBar, len(symbols))
	for _, s := range symbols {
		daily[s] = closesRaw[s][len(closesRaw[s])-n:]
	}
	times := make([]time.Time, n)
	for i := range times {
		times[i] = daily[symbols[0]][i].T
	}
	fmt.Printf("  %d символов, %d общих дневных свечей\n", len(symbols), n)

	splitIdx := sort.Search(n, func(i int) bool { return !times[i].Before(end.AddDate(0, 0, -holdoutDays)) })

	// Пары отбираются ОДИН раз, только на in-sample — см. doc-комментарий пакета.
	selected := pairs.SelectPairs(daily, symbols, 0, splitIdx, topNPairs)
	fmt.Printf("\n🔗 Отобрано %d пар (по корреляции дневных доходностей, только in-sample):\n", len(selected))
	for _, p := range selected {
		fmt.Printf("  %s\n", p)
	}

	startEquity := decimal.NewFromFloat(startEquityF)
	feeRate := decimal.NewFromFloat(feeRatePerLeg)
	slip := decimal.NewFromFloat(slippagePct)

	grid := buildGrid()
	fmt.Printf("\n🔍 Комбинаций в сетке: %d\n\n", len(grid))

	type scored struct {
		p     pairs.Params
		stats pairs.Stats
	}
	var results []scored
	for _, p := range grid {
		st := pairs.Simulate(daily, selected, p.LookbackDays, splitIdx, p, startEquity, feeRate, slip)
		if st.NumTrades < minTrades {
			continue
		}
		results = append(results, scored{p, st})
	}
	if len(results) == 0 {
		fmt.Println("⚠️  Ни одна комбинация не набрала минимум сделок для статистики.")
		return nil
	}
	sort.Slice(results, func(i, j int) bool { return results[i].stats.Score().GreaterThan(results[j].stats.Score()) })

	fmt.Println("🏆 Топ по in-sample (грубая прикидка, ещё НЕ финальный выбор):")
	top := results
	if len(top) > topN {
		top = top[:topN]
	}
	for i, r := range top {
		fmt.Printf("%2d. %s | %s\n", i+1, r.p, r.stats)
	}

	foldBounds := buildFoldBounds(times, splitIdx, n, folds)
	fmt.Printf("\n🧪 Walk-forward проверка на holdout (последние %d дней, разбито на %d период(ов), те же зафиксированные пары):\n",
		holdoutDays, folds)

	type validated struct {
		p          pairs.Params
		foldStats  []pairs.Stats
		foldScores []decimal.Decimal
		agg        decimal.Decimal
	}
	var withHoldout []validated
	for _, r := range top {
		v := validated{p: r.p}
		for _, fb := range foldBounds {
			st := pairs.Simulate(daily, selected, fb.start, fb.end, r.p, startEquity, feeRate, slip)
			v.foldStats = append(v.foldStats, st)
			v.foldScores = append(v.foldScores, st.Score())
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

// buildGrid — StopZ не независимое измерение сетки, а EntryZ+1.5: должен
// быть за EntryZ (иначе стоп срабатывал бы раньше или одновременно со
// входом), но его собственная оптимизация — не то, что здесь проверяется.
func buildGrid() []pairs.Params {
	lookbacks := []int{20, 30}
	entryZs := []float64{1.5, 2.0, 2.5}
	exitZs := []float64{0.0, 0.5}
	maxHolds := []int{15, 30}
	maxConcurrents := []int{2, 3}

	var grid []pairs.Params
	for _, lb := range lookbacks {
		for _, ez := range entryZs {
			for _, xz := range exitZs {
				for _, mh := range maxHolds {
					for _, mc := range maxConcurrents {
						grid = append(grid, pairs.Params{
							LookbackDays: lb, EntryZ: ez, ExitZ: xz,
							StopZ: ez + 1.5, MaxHoldDays: mh, MaxConcurrent: mc,
						})
					}
				}
			}
		}
	}
	return grid
}

func loadOrFetchDaily(ctx context.Context, client *futures.Client, cacheDir, symbol string, start, end time.Time, totalDays int) ([]pairs.DailyBar, error) {
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

func toDailyBars(candles []domain.Candle) []pairs.DailyBar {
	bars := make([]pairs.DailyBar, len(candles))
	for i, c := range candles {
		bars[i] = pairs.DailyBar{T: c.CloseTime, Close: c.Close}
	}
	return bars
}
