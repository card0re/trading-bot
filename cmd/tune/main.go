// Команда tune подбирает параметры стратегии перебором по сетке на
// исторических данных. Методология намеренно не однопроходная: сетка
// оценивается на более старой части истории (in-sample), а топ-кандидаты
// по ней перепроверяются на отложенном, не участвовавшем в отборе куске
// (out-of-sample, самые последние дни) — иначе легко подобрать числа,
// которые красиво объясняют прошлое, но ничего не говорят про будущее.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"

	"trading-bot/internal/backtest"
	"trading-bot/internal/domain"
	exchange "trading-bot/internal/exchange/binance"
	"trading-bot/internal/strategy"

	"github.com/adshao/go-binance/v2"
	"github.com/adshao/go-binance/v2/futures"
	"github.com/shopspring/decimal"
)

// Расширено и перепроверено после добавления шортов в стратегию (см.
// internal/strategy): широкий прогон на 10 монетах (24.08.2026, 365д, 1h)
// показал устойчивый плюс на holdout по 7 из них во всех проверенных
// вариантах параметров, и устойчивый минус по DOGE/AVAX/LTC — они убраны.
// Список — намеренная смесь крупной и не очень капитализации: тир как
// отдельный переключатель не понадобился, один набор параметров одинаково
// работает на всех семи.
var symbols = []string{
	"BTCUSDT", "ETHUSDT", "BNBUSDT", // крупная капитализация
	"SOLUSDT", "XRPUSDT", "ADAUSDT", "LINKUSDT", // средняя
}

const startEquityF = 10000.0

// minTradesPerSym отсекает комбинации, где по монете почти нет сделок —
// статистика ничего не значит. Не константа: -mode carry (см. buildCarryGrid)
// торгует по редкому событию (экстремальный funding), сделок на порядок
// меньше, чем у пробоя — с тем же порогом 15 сетка отфильтровала бы ВСЁ.
var minTradesPerSym = 15

// totalDays/holdoutDays — глубина истории и размер отложенного окна.
// Настраиваются флагами -days/-holdout (см. main); значения по умолчанию —
// последний прогон, не жёстко зашитая методология.
var (
	totalDays   = 365
	holdoutDays = 90
)

// interval — таймфрейм; на более крупном ATR-стоп шире относительно цены,
// и комиссия съедает меньшую долю риска на сделку. 5m проверен и отбракован
// (см. .env.example) — 1h теперь дефолт, а не просто одна из опций.
var interval = "1h"

func main() {
	cacheDir := flag.String("cache", filepath.Join(os.TempDir(), "trading-bot-tune-cache"), "каталог кэша исторических свечей")
	topN := flag.Int("top", 10, "сколько лучших по in-sample комбинаций перепроверять на holdout")
	intervalFlag := flag.String("interval", "1h", "таймфрейм свечей")
	daysFlag := flag.Int("days", totalDays, "глубина истории, дней")
	holdoutFlag := flag.Int("holdout", holdoutDays, "размер отложенного окна (out-of-sample), дней")
	foldsFlag := flag.Int("folds", 3, "на сколько последовательных периодов делить holdout для walk-forward "+
		"проверки устойчивости (1 = старое поведение, весь holdout одним куском)")
	modeFlag := flag.String("mode", "breakout", `"breakout" (по умолчанию) — обычная сетка пробоя+тренда; `+
		`"carry" — пробойный и mean-reversion входы отключены (BreakoutPct недостижим, MeanRevATRMultiplier=0), `+
		`остаётся только вход "на funding carry" сам по себе, изолированно от остальных сигналов; `+
		`"orderflow" — остальные параметры зафиксированы на уже найденном лучшем наборе, перебирается только `+
		`OrderFlowMinRatio (подтверждение пробоя дисбалансом потока ордеров)`)
	flag.Parse()
	interval = *intervalFlag
	totalDays = *daysFlag
	holdoutDays = *holdoutFlag
	if *foldsFlag < 1 {
		log.Fatal("❌ -folds должен быть >= 1")
	}
	if *modeFlag != "breakout" && *modeFlag != "carry" && *modeFlag != "orderflow" {
		log.Fatalf("❌ -mode должен быть \"breakout\", \"carry\" или \"orderflow\", получено %q", *modeFlag)
	}

	if err := run(*cacheDir, *topN, *foldsFlag, *modeFlag); err != nil {
		log.Fatalf("❌ %v", err)
	}
}

func run(cacheDir string, topN, folds int, mode string) error {
	log.SetOutput(io.Discard) // глушим лог стратегии — на сетке из тысяч прогонов это гигабайты шума

	if mode == "carry" {
		minTradesPerSym = 5 // редкое событие (экстремальный funding) — сделок на порядок меньше, чем у пробоя
	}

	futures.UseTestnet = false
	client := binance.NewFuturesClient("", "")
	ctx := context.Background()

	infos, err := exchange.LoadAllSymbolInfo(ctx, client, symbols)
	if err != nil {
		return err
	}

	fmt.Printf("📥 Данные: %d дней (%s), holdout — последние %d дней\n", totalDays, interval, holdoutDays)
	histories := make(map[string][]domain.Candle, len(symbols))
	fundings := make(map[string][]exchange.FundingPoint, len(symbols))
	for _, symbol := range symbols {
		h, err := loadOrFetch(ctx, client, cacheDir, symbol)
		if err != nil {
			return fmt.Errorf("история %s: %w", symbol, err)
		}
		histories[symbol] = h
		fmt.Printf("  %s: %d свечей\n", symbol, len(h))

		fr, err := loadOrFetchFunding(ctx, client, cacheDir, symbol)
		if err != nil {
			return fmt.Errorf("funding rate %s: %w", symbol, err)
		}
		fundings[symbol] = fr
	}

	splitTime := time.Now().AddDate(0, 0, -holdoutDays)

	grid := buildGrid()
	switch mode {
	case "carry":
		grid = buildCarryGrid()
	case "orderflow":
		grid = buildOrderFlowGrid()
	}
	fmt.Printf("🔍 Комбинаций в сетке: %d (× %d символов = %d прогонов in-sample)\n\n",
		len(grid), len(symbols), len(grid)*len(symbols))

	startEquity := decimal.NewFromFloat(startEquityF)

	type scored struct {
		params  strategy.Params
		inStats map[string]backtest.Stats
		score   decimal.Decimal
	}

	start := time.Now()
	var results []scored
	for _, params := range grid {
		warmup := maxInt(params.LookbackBars, params.TrendEMAPeriod, params.ATRPeriod, params.VolumeAvgPeriod)

		inStats := make(map[string]backtest.Stats, len(symbols))
		ok := true
		for _, symbol := range symbols {
			full := histories[symbol]
			splitIdx := sort.Search(len(full), func(i int) bool { return !full[i].CloseTime.Before(splitTime) })
			if splitIdx <= warmup {
				ok = false
				break
			}
			inStats[symbol] = runOne(full[:splitIdx], warmup, infos[symbol], symbol, startEquity, params, fundings[symbol])
			if inStats[symbol].TradeCount < minTradesPerSym {
				ok = false
			}
		}
		if !ok {
			continue
		}

		results = append(results, scored{params: params, inStats: inStats, score: score(inStats)})
	}
	fmt.Printf("⏱  Сетка посчитана за %s, прошло фильтр (>=%d сделок/монета): %d из %d\n\n",
		time.Since(start).Round(time.Millisecond), minTradesPerSym, len(results), len(grid))

	if len(results) == 0 {
		fmt.Println("⚠️  Ни одна комбинация не набрала минимум сделок для статистики — сетку нужно расширять или брать больше истории.")
		return nil
	}

	sort.Slice(results, func(i, j int) bool { return results[i].score.GreaterThan(results[j].score) })
	if len(results) > topN {
		results = results[:topN]
	}

	fmt.Println("🏆 Топ по in-sample (грубая прикидка, ещё НЕ финальный выбор):")
	for i, r := range results {
		fmt.Printf("%2d. %s | score=%s\n", i+1, describeParams(r.params), r.score.StringFixed(2))
		printPerSymbol(r.inStats)
	}

	// Holdout делится на `folds` последовательных непересекающихся периодов
	// (folds=1 — старое поведение, весь holdout одним куском). Кандидат,
	// который хорош только на одном удачном отрезке из нескольких, но плох
	// на остальных, должен проиграть ровному результату — иначе легко
	// принять случайное попадание в удачный режим рынка за устойчивый эдж.
	fmt.Printf("\n🧪 Walk-forward проверка на holdout (последние %d дней, разбито на %d период(ов), поиск их не видел):\n",
		holdoutDays, folds)

	foldBounds := buildFoldBounds(splitTime, folds)

	type validated struct {
		params     strategy.Params
		inScore    decimal.Decimal
		foldStats  []map[string]backtest.Stats
		foldScores []decimal.Decimal
		aggScore   decimal.Decimal // среднее по фолдам минус стандартное отклонение — штраф за нестабильность
	}
	var withHoldout []validated
	for _, r := range results {
		warmup := maxInt(r.params.LookbackBars, r.params.TrendEMAPeriod, r.params.ATRPeriod, r.params.VolumeAvgPeriod)

		v := validated{params: r.params, inScore: r.score}
		for _, fb := range foldBounds {
			foldStats := make(map[string]backtest.Stats, len(symbols))
			for _, symbol := range symbols {
				full := histories[symbol]
				foldStartIdx := sort.Search(len(full), func(i int) bool { return !full[i].CloseTime.Before(fb.start) })
				foldEndIdx := sort.Search(len(full), func(i int) bool { return !full[i].CloseTime.Before(fb.end) })
				from := foldStartIdx - warmup
				if from < 0 {
					from = 0
				}
				if foldEndIdx > len(full) {
					foldEndIdx = len(full)
				}
				foldStats[symbol] = runOne(full[from:foldEndIdx], foldStartIdx-from, infos[symbol], symbol, startEquity, r.params, fundings[symbol])
			}
			v.foldStats = append(v.foldStats, foldStats)
			v.foldScores = append(v.foldScores, score(foldStats))
		}
		v.aggScore = meanMinusStdev(v.foldScores)
		withHoldout = append(withHoldout, v)
	}

	sort.Slice(withHoldout, func(i, j int) bool { return withHoldout[i].aggScore.GreaterThan(withHoldout[j].aggScore) })

	for i, v := range withHoldout {
		fmt.Printf("\n%2d. %s\n    in-sample score=%s | walk-forward score=%s (среднее по фолдам минус разброс)\n",
			i+1, describeParams(v.params), v.inScore.StringFixed(2), v.aggScore.StringFixed(2))
		for fi, fs := range v.foldStats {
			fmt.Printf("    — фолд %d (%s..%s), score=%s:\n",
				fi+1, foldBounds[fi].start.Format("2006-01-02"), foldBounds[fi].end.Format("2006-01-02"),
				v.foldScores[fi].StringFixed(2))
			printPerSymbol(fs)
		}
	}

	best := withHoldout[0]
	fmt.Printf("\n✅ Лучшая по walk-forward score (не по in-sample!): %s\n", describeParams(best.params))
	fmt.Println("   Это не гарантия прибыли вперёд — это лучшая из проверенных комбинаций на " +
		"данных, которые сама сетка не подбирала под ответ, да ещё и стабильная по нескольким " +
		"последовательным периодам, а не удачная только на одном.")

	return nil
}

// foldBound — границы одного периода walk-forward проверки.
type foldBound struct{ start, end time.Time }

// buildFoldBounds делит [splitTime, now) на folds последовательных
// непересекающихся периодов. Остаток от целочисленного деления дней
// уходит в последний (самый свежий) период.
func buildFoldBounds(splitTime time.Time, folds int) []foldBound {
	now := time.Now()
	foldDays := holdoutDays / folds
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

// meanMinusStdev — среднее минус стандартное отклонение по набору оценок
// фолдов: не только "в среднем неплохо", но и "стабильно неплохо" —
// разбросанные оценки (плюс на одном фолде, минус на другом) штрафуются.
// Точность считается через float64 — это ранжирующая эвристика для отчёта,
// не денежная величина, декларативная точность decimal здесь не нужна.
func meanMinusStdev(scores []decimal.Decimal) decimal.Decimal {
	if len(scores) == 0 {
		return decimal.Zero
	}
	if len(scores) == 1 {
		return scores[0]
	}
	sum := 0.0
	vals := make([]float64, len(scores))
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
	stdev := math.Sqrt(variance)
	return decimal.NewFromFloat(mean - stdev)
}

func runOne(history []domain.Candle, warmup int, info *exchange.SymbolInfo, symbol string, startEquity decimal.Decimal, params strategy.Params, fundingPoints []exchange.FundingPoint) backtest.Stats {
	sim := backtest.NewSimExecutor(symbol, info, startEquity, backtest.TakerFeeRate, backtest.DefaultSlippagePct)

	// typed nil в domain.FundingRateSource != nil интерфейс — оборачиваем,
	// только если funding реально используется (carry-вход
	// FundingCarryMinRate); иначе оставляем интерфейс настоящим nil (0 =
	// "выключено", а не "никогда не входить на carry").
	var fundingSrc domain.FundingRateSource
	var replay *backtest.FundingReplay
	if params.FundingCarryMinRate.IsPositive() && len(fundingPoints) > 0 {
		replay = backtest.NewFundingReplay(fundingPoints)
		fundingSrc = replay
	}

	bot := strategy.NewBreakout(sim, sim, backtest.AlwaysAllowRiskGate{}, fundingSrc, params)
	bot.Warmup(history[:warmup])
	ctx := context.Background()
	for _, c := range history[warmup:] {
		if replay != nil {
			replay.Advance(c.CloseTime)
		}
		if ev := sim.OnCandle(c); ev != nil {
			bot.OnOrderEvent(ctx, *ev)
		}
		bot.OnCandle(ctx, c)
	}
	return backtest.ComputeStats(sim.Trades(), startEquity)
}

// score вознаграждает средний результат по всем монетам, штрафует за
// среднюю просадку и — отдельно — за разброс между средним и худшим
// результатом: комбинация, которая тащит один результат за счёт одной
// удачной монеты, а по остальным в минусе, хуже, чем ровный результат.
func score(stats map[string]backtest.Stats) decimal.Decimal {
	if len(stats) == 0 {
		return decimal.Zero
	}
	sumReturn := decimal.Zero
	sumDD := decimal.Zero
	minReturn := decimal.NewFromInt(1 << 30)
	for _, s := range stats {
		sumReturn = sumReturn.Add(s.TotalReturnPct)
		sumDD = sumDD.Add(s.MaxDrawdownPct)
		if s.TotalReturnPct.LessThan(minReturn) {
			minReturn = s.TotalReturnPct
		}
	}
	n := decimal.NewFromInt(int64(len(stats)))
	avgReturn := sumReturn.Div(n)
	avgDD := sumDD.Div(n)
	spread := avgReturn.Sub(minReturn)

	return avgReturn.
		Sub(avgDD.Mul(decimal.NewFromFloat(0.5))).
		Sub(spread.Mul(decimal.NewFromFloat(0.5)))
}

func printPerSymbol(stats map[string]backtest.Stats) {
	for _, symbol := range symbols {
		s, ok := stats[symbol]
		if !ok {
			continue
		}
		fmt.Printf("    %-8s %s\n", symbol, s)
	}
}

func describeParams(p strategy.Params) string {
	adx := "off"
	if !p.TrendStrengthMinADX.IsZero() {
		adx = p.TrendStrengthMinADX.String()
	}
	be := "off"
	if !p.BreakevenTriggerR.IsZero() {
		be = p.BreakevenTriggerR.String() + "R"
	}
	meanRev := "off"
	if !p.MeanRevATRMultiplier.IsZero() {
		meanRev = p.MeanRevATRMultiplier.String() + "×ATR"
	}
	carry := "off"
	if !p.FundingCarryMinRate.IsZero() {
		carry = p.FundingCarryMinRate.String()
	}
	volTarget := "off"
	if p.VolTargetPeriod > 0 {
		volTarget = fmt.Sprintf("%d", p.VolTargetPeriod)
	}
	orderFlow := "off"
	if !p.OrderFlowMinRatio.IsZero() {
		orderFlow = p.OrderFlowMinRatio.String()
	}
	return fmt.Sprintf(
		"LB=%d BreakoutPct=%s TrendEMA=%d VolMult=%s ATRStopMult=%s RR=%s MinADX=%s Breakeven=%s MeanRev=%s Carry=%s VolTarget=%s OrderFlow=%s",
		p.LookbackBars, p.BreakoutPct.StringFixed(2), p.TrendEMAPeriod,
		p.VolumeMultiplier.StringFixed(2), p.ATRStopMultiplier.StringFixed(2), p.RiskRewardRatio.StringFixed(2),
		adx, be, meanRev, carry, volTarget, orderFlow)
}

// baseCandidates — топ-10 по holdout из первого широкого прогона (270д,
// 648 комбинаций × 5 монет, LB везде выиграл на 30). Второй проход не
// пересчитывает всю сетку заново — только добавляет фильтр по funding rate
// поверх уже найденных рабочих наборов, на сузившемся до BNB+ETH списке.
func baseCandidates() []strategy.Params {
	type base struct {
		bp, vm, am, rr float64
		tp             int
	}
	bases := []base{
		{0.05, 1.5, 2.0, 1.5, 100},
		{0.00, 2.0, 2.0, 1.5, 100},
		{0.05, 2.0, 3.0, 1.0, 100},
		{0.05, 2.0, 3.0, 1.0, 20},
		{0.00, 2.0, 3.0, 1.0, 100},
		{0.05, 2.0, 2.0, 1.5, 50},
		{0.05, 2.0, 2.0, 1.5, 100},
		{0.05, 2.0, 2.0, 1.5, 20},
		{0.00, 2.0, 2.0, 1.5, 50},
		{0.00, 2.0, 2.0, 1.5, 20},
	}
	out := make([]strategy.Params, len(bases))
	for i, b := range bases {
		out[i] = strategy.Params{
			LookbackBars:      30,
			BreakoutPct:       decimal.NewFromFloat(b.bp),
			CooldownBars:      3,
			TrendEMAPeriod:    b.tp,
			ATRPeriod:         14,
			VolumeAvgPeriod:   20,
			VolumeMultiplier:  decimal.NewFromFloat(b.vm),
			ATRStopMultiplier: decimal.NewFromFloat(b.am),
			RiskRewardRatio:   decimal.NewFromFloat(b.rr),
			RiskPerTradePct:   decimal.NewFromFloat(1.0),
		}
	}
	return out
}

// buildGrid берёт baseCandidates и крестит их с ADX-фильтром силы тренда и
// входом "на возврат к среднему" в боковике (MeanRevATRMultiplier) — обе идеи
// родились из находки 3-летнего walk-forward прогона (24.08.2026): стратегия
// слабеет на боковике, ADX должен эту слабину отсеивать, а возврат к среднему
// — зарабатывать именно там, где отсеянный пробойный вход бездействует.
//
// Funding rate (5 значений) и перенос в безубыток (0/1R/1.5R) уже
// перебирались раньше и почти не повлияли на результат (funding — см.
// .env.example; безубыток структурно бесполезен при RR=1.0, см.
// internal/strategy.Params) — оба зафиксированы, бюджет сетки ушёл на
// единственную реально непроверенную идею.
func buildGrid() []strategy.Params {
	adxThresholds := []float64{0, 20, 25} // 0 = фильтр выключен
	// MeanRevATRMultiplier сюда сознательно не включён: широкий прогон
	// (24.08.2026, 120 комбинаций) показал, что он не улучшает walk-forward
	// — топ-10 неизменно выбирали MeanRev=off. FundingCarryMinRate — наоборот,
	// в свежем прогоне (24.08.2026) ВСЕ топ-10 walk-forward выбрали carry
	// включённым (0.0005 или 0.001) — зафиксирован на найденном победителе,
	// а бюджет сетки ушёл на следующую непроверенную идею — таргетирование
	// волатильности (VolTargetPeriod).
	volTargetPeriods := []int{0, 20, 50} // 0 = выключено

	var grid []strategy.Params
	for _, base := range baseCandidates() {
		base.BreakevenTriggerR = decimal.Zero
		base.MeanRevATRMultiplier = decimal.Zero
		base.FundingCarryMinRate = decimal.NewFromFloat(0.001)
		for _, adx := range adxThresholds {
			for _, vt := range volTargetPeriods {
				p := base
				p.TrendStrengthMinADX = decimal.NewFromFloat(adx)
				p.VolTargetPeriod = vt
				grid = append(grid, p)
			}
		}
	}
	return grid
}

// buildCarryGrid изолирует вход "на funding carry" от всех остальных
// сигналов: BreakoutPct=1000 (1000%) делает пробойный триггер физически
// недостижимым любым реальным движением цены, MeanRevATRMultiplier=0
// выключает возврат к среднему явно (его собственное условие входа и так
// требует TrendStrengthMinADX>0, здесь он тоже 0 — вторая, независимая
// причина, почему он не сработает). Внутри switch в internal/strategy/
// brain.go (case longOK/shortOK/meanRevLongOK/meanRevShortOK/
// fundingCarryLongOK/fundingCarryShortOK) остаётся достижим только carry —
// проверяем его собственный эдж в изоляции, а не как довесок к пробою.
//
// ВАЖНО: это НЕ маркет-нейтральный carry (лонг спот + шорт перп) — это всё
// ещё направленная сделка (голый перп без хеджа), просто сигнал входа другой
// (экстремальность funding rate, а не пробой цены). Управляется тем же
// ATR-стопом/тейком и vol-targeting, что и пробой — ценовой риск НЕ снят.
//
// ИТОГ (25.08.2026, -days 1095 -holdout 180 -folds 3): НЕ ПРОВЕРЯЕМО прямо
// сейчас, а не "не работает" — это другой вывод. In-sample (2023-2025,
// ~2.5 года) сигнал реально торговал: у лучшей комбинации 15-54 сделки на
// символ, результат смешанный (ETH +7.6%, ADA +8.6%, но BNB -3.8%,
// SOL -5.6%). На holdout (последние 180 дней, 3 фолда × 7 символов × 10
// кандидатов = 210 ячеек) — 198 ячеек с 0 сделок, 12 с ровно 1, ни одной с
// 2+. Даже самый мягкий проверенный порог (0.0003 = 0.03%) почти не
// срабатывал. Экстремальный funding rate по этой корзине практически
// перестал случаться в последние ~6 месяцев (согласуется с общей находкой
// сессии — рынок ушёл в затишье/боковик, см. .env.example и
// cmd/regime/main.go) — статистики out-of-sample для ЛЮБОГО вывода
// (позитивного ИЛИ негативного) сейчас недостаточно. НЕ ДЕПЛОИТЬ. Не
// пересматривать раньше, чем наберётся реальная holdout-статистика —
// пересматривать сетку порогов сейчас бессмысленно, проблема не в пороге.
func buildCarryGrid() []strategy.Params {
	thresholds := []float64{0.0003, 0.0005, 0.0008, 0.001, 0.0015}
	atrMults := []float64{2.0, 3.0}
	rrs := []float64{1.0, 1.5, 2.0}

	var grid []strategy.Params
	for _, th := range thresholds {
		for _, am := range atrMults {
			for _, rr := range rrs {
				grid = append(grid, strategy.Params{
					LookbackBars:         30,
					BreakoutPct:          decimal.NewFromInt(1000), // недостижимо — пробойный вход выключен
					CooldownBars:         3,
					TrendEMAPeriod:       100,
					ATRPeriod:            14,
					VolumeAvgPeriod:      20,
					VolumeMultiplier:     decimal.NewFromFloat(2.0), // не важно, пробой всё равно недостижим
					ATRStopMultiplier:    decimal.NewFromFloat(am),
					RiskRewardRatio:      decimal.NewFromFloat(rr),
					RiskPerTradePct:      decimal.NewFromFloat(1.0),
					TrendStrengthMinADX:  decimal.Zero, // meanrev и так не сработает без этого; пробою всё равно не дойти
					MeanRevATRMultiplier: decimal.Zero,
					FundingCarryMinRate:  decimal.NewFromFloat(th),
					VolTargetPeriod:      50,
				})
			}
		}
	}
	return grid
}

// buildOrderFlowGrid — как buildCarryGrid: остальные параметры зафиксированы
// на уже найденном walk-forward победителе (см. .env.example), перебирается
// только OrderFlowMinRatio, чтобы честно ответить на один сфокусированный
// вопрос — "помогает ли подтверждение потоком ордеров ПОВЕРХ уже лучшей
// версии пробоя", а не размывать вывод по всей исторической сетке заново.
func buildOrderFlowGrid() []strategy.Params {
	ratios := []float64{0, 0.55, 0.6, 0.65, 0.7} // 0 = фильтр выключен (контрольная точка)

	var grid []strategy.Params
	for _, r := range ratios {
		grid = append(grid, strategy.Params{
			LookbackBars:        30,
			BreakoutPct:         decimal.NewFromFloat(0.05),
			CooldownBars:        3,
			TrendEMAPeriod:      100,
			ATRPeriod:           14,
			VolumeAvgPeriod:     20,
			VolumeMultiplier:    decimal.NewFromFloat(2.0),
			ATRStopMultiplier:   decimal.NewFromFloat(3.0),
			RiskRewardRatio:     decimal.NewFromFloat(1.0),
			RiskPerTradePct:     decimal.NewFromFloat(1.0),
			ADXPeriod:           14,
			TrendStrengthMinADX: decimal.NewFromFloat(25),
			FundingCarryMinRate: decimal.NewFromFloat(0.001),
			VolTargetPeriod:     50,
			OrderFlowMinRatio:   decimal.NewFromFloat(r),
		})
	}
	return grid
}

func loadOrFetch(ctx context.Context, client *futures.Client, cacheDir, symbol string) ([]domain.Candle, error) {
	path := filepath.Join(cacheDir, fmt.Sprintf("%s_%s_%dd.json", symbol, interval, totalDays))
	if data, err := os.ReadFile(path); err == nil {
		var candles []domain.Candle
		if err := json.Unmarshal(data, &candles); err == nil && len(candles) > 0 {
			return candles, nil
		}
	}

	end := time.Now()
	start := end.AddDate(0, 0, -totalDays)
	candles, err := exchange.LoadHistoricalCandles(ctx, client, symbol, interval, start, end)
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

func loadOrFetchFunding(ctx context.Context, client *futures.Client, cacheDir, symbol string) ([]exchange.FundingPoint, error) {
	path := filepath.Join(cacheDir, fmt.Sprintf("%s_funding_%dd.json", symbol, totalDays))
	if data, err := os.ReadFile(path); err == nil {
		var points []exchange.FundingPoint
		if err := json.Unmarshal(data, &points); err == nil && len(points) > 0 {
			return points, nil
		}
	}

	end := time.Now()
	start := end.AddDate(0, 0, -totalDays)
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

func maxInt(vals ...int) int {
	m := vals[0]
	for _, v := range vals[1:] {
		if v > m {
			m = v
		}
	}
	return m
}
