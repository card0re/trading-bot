// Команда carry бэктестит delta-neutral funding-rate arbitrage
// (cash-and-carry: спот куплен, тот же нотионал шортится в перпе) —
// четвёртый независимо проверяемый класс стратегии этой сессии, и первый,
// который НЕ является направленной ставкой на цену: ценовой риск гасится
// хеджем по конструкции сделки, доход — чистый funding. Отличается от уже
// отвергнутого strategy.Params.FundingCarryMinRate (см. cmd/tune -mode
// carry), который был тем же сигналом входа, но БЕЗ хеджа — голым перпом
// с ATR-стопом, то есть всё ещё направленной ставкой.
//
// Методология walk-forward та же, что и у остальных cmd/*: сетка оценивается
// in-sample, топ-кандидаты перепроверяются на отложенном holdout, разбитом
// на несколько последовательных фолдов.
//
// ИТОГ (25.08.2026, -days 1095 -holdout 180 -folds 3): САМЫЙ ЧИСТЫЙ
// результат сессии, но НЕ ДЕПЛОИТЬ БЕЗ ЖИВОГО ИСПОЛНЕНИЯ (см. ниже). In-sample
// (2023-2025, ~2.5 года) — устойчивый плюс на КАЖДОЙ из 7 монет одновременно
// (+15-22%), просадки 0.15-2.7% — на порядок спокойнее любой направленной
// стратегии в этом репо, потому что ценовой риск гасится хеджем по
// конструкции сделки, а не риск-менеджментом поверх направленной ставки.
// Walk-forward на holdout (последние 180 дней) — почти НОЛЬ (-0.01..-0.08 у
// топ-7): не убыток и не рост, а ПРОСТОЙ — топ-кандидаты почти не находят
// сделок (лучший: 1 сделка на все 7 монет за 3 фолда). Причина видна в
// деталях фолдов у более низких порогов (EntryRate=5-10%): funding rate
// в последние ~6 месяцев стал слишком мелким, чтобы окупить фиксированную
// комиссию за круг (спот+перп, вход+выход, ~0.15% от нотионала) — те же
// самые полгода затишья, что уже нашлись в cmd/tune -mode carry (направленный
// carry) и в cmd/regime (ADX разучился отличать тренд от боковика). Это не
// "стратегия не работает", а "триггер стратегии сейчас редко срабатывает" —
// принципиально другой диагноз, чем у пробоя/ротации, которые ломаются даже
// там, где события есть. НЕ ДЕПЛОИТЬ прямо сейчас по другой причине: этот
// бэктест не проверяет живое исполнение — держать спот+шорт-перп одновременно
// требует капитала на ДВУХ рынках сразу (не только фьючерсной маржи, как
// сейчас у cmd/bot) и отдельного движка для двух ног сделки, которого в
// проекте пока нет. Разумный следующий шаг — не доверять больше истории (её
// тут физически мало, funding редкое событие), а построить live paper-режим
// и ждать следующего всплеска funding rate в реальном времени.
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

	"trading-bot/internal/carry"
	"trading-bot/internal/domain"
	exchange "trading-bot/internal/exchange/binance"

	spotapi "github.com/adshao/go-binance/v2"
	"github.com/adshao/go-binance/v2/futures"
	"github.com/shopspring/decimal"
)

// symbols — тот же набор, что реально торгует cmd/bot: смысла проверять
// carry-эдж на монетах, которые бот не торгует, нет.
var symbols = []string{
	"BTCUSDT", "ETHUSDT", "BNBUSDT", "SOLUSDT", "XRPUSDT", "ADAUSDT", "LINKUSDT",
}

const (
	startEquityF = 10000.0
	minTrades    = 3 // низкочастотная стратегия (событие — заметная ставка funding), не 15 как у пробоя

	// spotFeeRate — стандартная (без BNB-скидки, без VIP) комиссия тейкера
	// на Binance Spot, 0.1%. Вдвое выше перпа (backtest.TakerFeeRate,
	// 0.05%) — это реальная асимметрия ног сделки, не опечатка.
	spotFeeRate = 0.001
	perpFeeRate = 0.0005
)

func main() {
	cacheDir := flag.String("cache", filepath.Join(os.TempDir(), "trading-bot-carry-cache"), "каталог кэша свечей/funding")
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
	perpClient := futures.NewClient("", "")
	spotClient := spotapi.NewClient("", "")
	ctx := context.Background()

	end := time.Now()
	start := end.AddDate(0, 0, -totalDays)
	splitTime := end.AddDate(0, 0, -holdoutDays)

	fmt.Printf("📥 Funding + спот/перп цена (8h), %d дней, holdout — последние %d дней:\n", totalDays, holdoutDays)
	events := make(map[string][]carry.FundingEvent, len(symbols))
	for _, s := range symbols {
		ev, err := loadEvents(ctx, perpClient, spotClient, cacheDir, s, start, end, totalDays)
		if err != nil {
			fmt.Printf("  ⚠️  %s: пропущен (%v)\n", s, err)
			continue
		}
		events[s] = ev
		fmt.Printf("  %s: %d funding-событий\n", s, len(ev))
	}

	startEquity := decimal.NewFromFloat(startEquityF)
	grid := buildGrid()
	fmt.Printf("\n🔍 Комбинаций в сетке: %d\n\n", len(grid))

	type scored struct {
		p     carry.Params
		stats map[string]carry.Stats
		score decimal.Decimal
	}
	var results []scored
	for _, p := range grid {
		st := make(map[string]carry.Stats, len(symbols))
		total := 0
		for _, s := range symbols {
			inSample := eventsBefore(events[s], splitTime)
			st[s] = carry.Simulate(inSample, p, startEquity)
			total += st[s].NumTrades
		}
		if total < minTrades {
			continue
		}
		results = append(results, scored{p, st, aggScore(st)})
	}
	if len(results) == 0 {
		fmt.Println("⚠️  Ни одна комбинация не набрала минимум сделок для статистики — funding, видимо, редко был достаточно экстремальным.")
		return nil
	}
	sort.Slice(results, func(i, j int) bool { return results[i].score.GreaterThan(results[j].score) })

	fmt.Println("🏆 Топ по in-sample (грубая прикидка, ещё НЕ финальный выбор):")
	top := results
	if len(top) > topN {
		top = top[:topN]
	}
	for i, r := range top {
		fmt.Printf("%2d. %s | score=%s\n", i+1, describeParams(r.p), r.score.StringFixed(2))
		printPerSymbol(r.stats)
	}

	foldBounds := buildFoldBounds(splitTime, end, folds)
	fmt.Printf("\n🧪 Walk-forward проверка на holdout (последние %d дней, разбито на %d период(ов)):\n", holdoutDays, folds)

	type validated struct {
		p          carry.Params
		foldStats  []map[string]carry.Stats
		foldScores []decimal.Decimal
		agg        decimal.Decimal
	}
	var withHoldout []validated
	for _, r := range top {
		v := validated{p: r.p}
		for _, fb := range foldBounds {
			st := make(map[string]carry.Stats, len(symbols))
			for _, s := range symbols {
				st[s] = carry.Simulate(eventsBetween(events[s], fb.start, fb.end), r.p, startEquity)
			}
			v.foldStats = append(v.foldStats, st)
			v.foldScores = append(v.foldScores, aggScore(st))
		}
		v.agg = meanMinusStdev(v.foldScores)
		withHoldout = append(withHoldout, v)
	}
	sort.Slice(withHoldout, func(i, j int) bool { return withHoldout[i].agg.GreaterThan(withHoldout[j].agg) })

	for i, v := range withHoldout {
		fmt.Printf("\n%2d. %s | walk-forward score=%s\n", i+1, describeParams(v.p), v.agg.StringFixed(2))
		for fi, st := range v.foldStats {
			fmt.Printf("    — фолд %d (%s..%s), score=%s:\n",
				fi+1, foldBounds[fi].start.Format("2006-01-02"), foldBounds[fi].end.Format("2006-01-02"), v.foldScores[fi].StringFixed(2))
			printPerSymbol(st)
		}
	}

	best := withHoldout[0]
	fmt.Printf("\n✅ Лучшая по walk-forward: %s\n", describeParams(best.p))
	fmt.Println("   Не гарантия прибыли вперёд — лучшая из проверенных комбинаций на данных, которые сама сетка не подбирала под ответ.")
	return nil
}

func printPerSymbol(stats map[string]carry.Stats) {
	for _, s := range symbols {
		st, ok := stats[s]
		if !ok {
			continue
		}
		fmt.Printf("    %-8s сделок %d | funding %s | базис %s | комиссии %s | эквити %s → %s (%s%%) | просадка %s%%\n",
			s, st.NumTrades, st.FundingCollected.StringFixed(2), st.BasisPnL.StringFixed(2), st.FeesPaid.StringFixed(2),
			st.StartEquity.StringFixed(2), st.FinalEquity.StringFixed(2), st.ReturnPct().StringFixed(2), st.MaxDrawdown.StringFixed(2))
	}
}

func describeParams(p carry.Params) string {
	maxHold := "off"
	if p.MaxHoldPeriods > 0 {
		maxHold = fmt.Sprintf("%dп", p.MaxHoldPeriods)
	}
	return fmt.Sprintf("Lookback=%dп EntryRate=%s%% ExitRate=%s%% MaxHold=%s",
		p.Lookback, p.EntryAnnualRate.StringFixed(0), p.ExitAnnualRate.StringFixed(0), maxHold)
}

// aggScore — как score() в cmd/tune: средний результат по монетам минус
// штраф за просадку и за разброс между средним и худшим результатом.
func aggScore(stats map[string]carry.Stats) decimal.Decimal {
	if len(stats) == 0 {
		return decimal.Zero
	}
	sumScore := decimal.Zero
	minScore := decimal.NewFromInt(1 << 30)
	for _, s := range stats {
		sc := s.Score()
		sumScore = sumScore.Add(sc)
		if sc.LessThan(minScore) {
			minScore = sc
		}
	}
	n := decimal.NewFromInt(int64(len(stats)))
	avg := sumScore.Div(n)
	spread := avg.Sub(minScore)
	return avg.Sub(spread.Mul(decimal.NewFromFloat(0.5)))
}

type foldBound struct{ start, end time.Time }

func buildFoldBounds(splitTime, now time.Time, folds int) []foldBound {
	totalDays := int(now.Sub(splitTime).Hours() / 24)
	foldDays := totalDays / folds
	bounds := make([]foldBound, folds)
	for i := 0; i < folds; i++ {
		bounds[i].start = splitTime.AddDate(0, 0, i*foldDays)
		if i == folds-1 {
			bounds[i].end = now
		} else {
			bounds[i].end = splitTime.AddDate(0, 0, (i+1)*foldDays)
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

func eventsBefore(ev []carry.FundingEvent, t time.Time) []carry.FundingEvent {
	idx := sort.Search(len(ev), func(i int) bool { return !ev[i].Time.Before(t) })
	return ev[:idx]
}

func eventsBetween(ev []carry.FundingEvent, from, to time.Time) []carry.FundingEvent {
	start := sort.Search(len(ev), func(i int) bool { return !ev[i].Time.Before(from) })
	end := sort.Search(len(ev), func(i int) bool { return !ev[i].Time.Before(to) })
	return ev[start:end]
}

// buildGrid — Exit всегда строго меньше Entry (гистерезис), поэтому
// перебор комбинаций фильтруется, а не декартово произведение целиком.
func buildGrid() []carry.Params {
	lookbacks := []int{1, 3, 6, 12} // периодов по 8ч: 8ч/1д/2д/4д
	entryRates := []float64{5, 10, 15, 20, 30}
	exitRates := []float64{0, 3, 5}
	maxHolds := []int{0, 90} // 0=без ограничения, 90 периодов=30д

	var grid []carry.Params
	for _, lb := range lookbacks {
		for _, er := range entryRates {
			for _, xr := range exitRates {
				if xr >= er {
					continue
				}
				for _, mh := range maxHolds {
					grid = append(grid, carry.Params{
						Lookback:        lb,
						EntryAnnualRate: decimal.NewFromFloat(er),
						ExitAnnualRate:  decimal.NewFromFloat(xr),
						MaxHoldPeriods:  mh,
						SpotFeeRate:     decimal.NewFromFloat(spotFeeRate),
						PerpFeeRate:     decimal.NewFromFloat(perpFeeRate),
					})
				}
			}
		}
	}
	return grid
}

func loadEvents(ctx context.Context, perpClient *futures.Client, spotClient *spotapi.Client, cacheDir, symbol string, start, end time.Time, totalDays int) ([]carry.FundingEvent, error) {
	fundingPoints, err := loadOrFetchFunding(ctx, perpClient, cacheDir, symbol, start, end, totalDays)
	if err != nil {
		return nil, fmt.Errorf("funding: %w", err)
	}
	if len(fundingPoints) == 0 {
		return nil, fmt.Errorf("нет истории funding rate")
	}

	perpCandles, err := loadOrFetchCandles(cacheDir, fmt.Sprintf("%s_perp8h_%dd.json", symbol, totalDays),
		func() ([]domain.Candle, error) { return exchange.LoadHistoricalCandles(ctx, perpClient, symbol, "8h", start, end) })
	if err != nil {
		return nil, fmt.Errorf("перп-свечи: %w", err)
	}
	spotCandles, err := loadOrFetchCandles(cacheDir, fmt.Sprintf("%s_spot8h_%dd.json", symbol, totalDays),
		func() ([]domain.Candle, error) { return exchange.LoadHistoricalSpotCandles(ctx, spotClient, symbol, "8h", start, end) })
	if err != nil {
		return nil, fmt.Errorf("спот-свечи: %w", err)
	}

	times := make([]time.Time, len(fundingPoints))
	rates := make([]decimal.Decimal, len(fundingPoints))
	for i, f := range fundingPoints {
		times[i] = f.Time
		rates[i] = f.Rate
	}
	return carry.BuildEvents(times, rates, spotCandles, perpCandles), nil
}

// loadOrFetchCandles/loadOrFetchFunding — тот же кэш-на-диске паттерн, что
// и в cmd/tune/cmd/pairs: свечи и funding не меняются задним числом, второй
// запуск с теми же параметрами не должен снова дёргать сеть.
func loadOrFetchCandles(cacheDir, filename string, fetch func() ([]domain.Candle, error)) ([]domain.Candle, error) {
	path := filepath.Join(cacheDir, filename)
	if data, err := os.ReadFile(path); err == nil {
		var candles []domain.Candle
		if err := json.Unmarshal(data, &candles); err == nil && len(candles) > 0 {
			return candles, nil
		}
	}
	candles, err := fetch()
	if err != nil {
		return nil, err
	}
	if data, err := json.Marshal(candles); err == nil {
		if err := os.MkdirAll(cacheDir, 0o755); err == nil {
			_ = os.WriteFile(path, data, 0o644)
		}
	}
	return candles, nil
}

func loadOrFetchFunding(ctx context.Context, client *futures.Client, cacheDir, symbol string, start, end time.Time, totalDays int) ([]exchange.FundingPoint, error) {
	path := filepath.Join(cacheDir, fmt.Sprintf("%s_funding_%dd.json", symbol, totalDays))
	if data, err := os.ReadFile(path); err == nil {
		var points []exchange.FundingPoint
		if err := json.Unmarshal(data, &points); err == nil && len(points) > 0 {
			return points, nil
		}
	}
	points, err := exchange.LoadFundingRateHistory(ctx, client, symbol, start, end)
	if err != nil {
		return nil, err
	}
	if data, err := json.Marshal(points); err == nil {
		if err := os.MkdirAll(cacheDir, 0o755); err == nil {
			_ = os.WriteFile(path, data, 0o644)
		}
	}
	return points, nil
}
