// Команда regime проверяет одну конкретную гипотезу: даёт ли переключение
// ВСЕГО капитала между двумя уже провалидированными стратегиями —
// internal/strategy.Breakout (направленный пробой, см. cmd/tune) и
// internal/rotation.Simulate (кросс-секционная ротация, см. cmd/rotation) —
// по режиму рынка (тренд/боковик) прирост относительно того, чтобы держать
// обе включёнными одновременно всегда (нынешнее состояние — оба бота живут
// параллельно на testnet, см. cmd/bot и cmd/rotationbot).
//
// Почему это не открытие с нуля, а прямое продолжение уже найденного:
// 3-летний walk-forward прогон (24.08.2026, см. .env.example) показал, что
// рынок ушёл из тренда в боковик, и Breakout перестал показывать эдж — а
// Rotation, ставящая на разброс между монетами, а не на направление, была
// устойчивее. ADX — уже используемый в Breakout индикатор силы тренда —
// здесь берётся по всей корзине монет (не по одной), чтобы решать: сейчас
// корзина в целом трендовая (капитал → Breakout) или боковая (капитал →
// Rotation).
//
// Оба набора параметров стратегий ЗАФИКСИРОВАНЫ на уже найденных walk-forward
// победителях (bestBreakoutParams/bestRotationParams ниже) и НЕ перебираются
// здесь заново — сетка этого инструмента перебирает только НОВЫЕ параметры
// самого переключения (порог ADX, длина блока). Так вопрос остаётся
// сфокусированным: "помогает ли переключение при уже лучших версиях обеих
// стратегий", а не размывается в комбинаторику по всем параметрам сразу.
//
// ИТОГ (25.08.2026, -days 1095 -holdout 180 -folds 3): гипотеза НЕ
// подтвердилась. In-sample топ переключения (+43.8%) проигрывает чистой
// Rotation без всякого переключения (+51.6%) — переключение уже на
// тренировочных данных не добавляет ничего сверх простого выбора одной
// стратегии. На walk-forward holdout результат ещё хуже: у ЛЮБОЙ комбинации
// с реальным переключением (ADX-порог 20-35) score отрицательный и хуже
// обеих базовых линий; единственная комбинация, обогнавшая always-Breakout
// (-1.97 против -2.84), почти никогда не переключалась (4 блока ротации из
// 23 за весь holdout) — статистически неотличима от простого always-Breakout,
// не настоящий сигнал режима. Дневной ADX корзины слишком шумный/грубый
// индикатор для этой задачи на проверенных порогах. НЕ ДЕПЛОИТЬ. Если
// возвращаться к идее регим-свитчинга — нужен другой сигнал режима
// (например реализованная волатильность или дисперсия доходностей между
// монетами, а не ADX), не косметическая правка порога/длины блока в этой же
// сетке — она уже перебрана достаточно широко, чтобы не быть причиной.
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
	"trading-bot/internal/indicator"
	"trading-bot/internal/rotation"
	"trading-bot/internal/strategy"

	"github.com/adshao/go-binance/v2"
	"github.com/adshao/go-binance/v2/futures"
	"github.com/shopspring/decimal"
)

// symbols — тот же семисимвольный набор, что уже живёт на testnet у обоих
// ботов (cmd/bot и cmd/rotationbot -symbols по умолчанию): сигнал режима и
// обе стратегии смотрят на одну и ту же корзину.
var symbols = []string{"BTCUSDT", "ETHUSDT", "BNBUSDT", "SOLUSDT", "XRPUSDT", "ADAUSDT", "LINKUSDT"}

const startEquityF = 10000.0

var (
	totalDays   = 730
	holdoutDays = 180
	interval    = "1h"
	adxPeriod   = 14
)

// bestBreakoutParams — walk-forward победитель из cmd/tune (25.08.2026, см.
// .env.example). FundingCarryMinRate здесь сознательно 0: funding rate
// история в этом инструменте не подгружается (усложнило бы блочную нарезку
// без веской причины для проверки именно идеи переключения), а ненулевой
// порог без funding source тихо не сработал бы ни разу — оставлять его
// ненулевым было бы обманчивее, чем честно выключить.
func bestBreakoutParams() strategy.Params {
	return strategy.Params{
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
		VolTargetPeriod:     50,
	}
}

// bestRotationParams — walk-forward победитель из cmd/rotation, тот же, что
// сейчас крутится в cmd/rotationbot по умолчанию (см. его doc-комментарий).
func bestRotationParams() rotation.Params {
	return rotation.Params{LookbackDays: 10, TopK: 2, RebalanceEvery: 7}
}

// regimeParams — единственное, что перебирает сетка этого инструмента.
type regimeParams struct {
	adxThreshold float64 // ADX корзины выше — торгуем Breakout, ниже — Rotation
	blockDays    int     // на сколько дней вперёд решение фиксируется без пересмотра
}

func (rp regimeParams) String() string {
	return fmt.Sprintf("ADX>=%.0f blockDays=%d", rp.adxThreshold, rp.blockDays)
}

// policy — какую стратегию гоняет chainEquity: switch — по регуляции,
// alwaysBreakout/alwaysRotation — базовые линии для честного сравнения,
// посчитанные ЧЕРЕЗ ТОТ ЖЕ харнесс (та же нарезка на блоки, тот же учёт
// комиссии/просадки), а не через отдельные более ранние прогоны cmd/tune и
// cmd/rotation с другой методологией — иначе сравнение было бы нечестным.
type policy int

const (
	policySwitch policy = iota
	policyAlwaysBreakout
	policyAlwaysRotation
)

func main() {
	cacheDir := flag.String("cache", filepath.Join(os.TempDir(), "trading-bot-regime-cache"), "каталог кэша исторических свечей")
	daysFlag := flag.Int("days", totalDays, "глубина истории, дней")
	holdoutFlag := flag.Int("holdout", holdoutDays, "размер отложенного окна (out-of-sample), дней")
	foldsFlag := flag.Int("folds", 3, "число периодов walk-forward")
	flag.Parse()
	totalDays = *daysFlag
	holdoutDays = *holdoutFlag
	if *foldsFlag < 1 {
		log.Fatal("❌ -folds должен быть >= 1")
	}

	if err := run(*cacheDir, *foldsFlag); err != nil {
		log.Fatalf("❌ %v", err)
	}
}

func run(cacheDir string, folds int) error {
	log.SetOutput(io.Discard) // глушим лог стратегии — блоков много, это гигабайты шума

	futures.UseTestnet = false
	client := binance.NewFuturesClient("", "")
	ctx := context.Background()

	infos, err := exchange.LoadAllSymbolInfo(ctx, client, symbols)
	if err != nil {
		return err
	}

	end := time.Now()
	start := end.AddDate(0, 0, -totalDays)

	fmt.Printf("📥 %d дней истории, holdout — последние %d дней\n", totalDays, holdoutDays)
	hourly := make(map[string][]domain.Candle, len(symbols))
	dailyRaw := make(map[string][]domain.Candle, len(symbols))
	for _, s := range symbols {
		h, err := loadOrFetch(ctx, client, cacheDir, s, interval, totalDays, start, end)
		if err != nil {
			return fmt.Errorf("часовая история %s: %w", s, err)
		}
		hourly[s] = h
		d, err := loadOrFetch(ctx, client, cacheDir, s, "1d", totalDays, start, end)
		if err != nil {
			return fmt.Errorf("дневная история %s: %w", s, err)
		}
		dailyRaw[s] = d
		fmt.Printf("  %s: %d часовых, %d дневных свечей\n", s, len(h), len(d))
	}

	// Дневные ряды выравниваем по минимальной длине с конца (как в
	// cmd/rotation) — без этого индексы между символами разъезжаются.
	n := len(dailyRaw[symbols[0]])
	for _, s := range symbols {
		if len(dailyRaw[s]) < n {
			n = len(dailyRaw[s])
		}
	}
	daily := make(map[string][]domain.Candle, len(symbols))
	dailyBars := make(map[string][]rotation.DailyBar, len(symbols))
	for _, s := range symbols {
		trimmed := dailyRaw[s][len(dailyRaw[s])-n:]
		daily[s] = trimmed
		bars := make([]rotation.DailyBar, n)
		for i, c := range trimmed {
			bars[i] = rotation.DailyBar{T: c.CloseTime, Close: c.Close}
		}
		dailyBars[s] = bars
	}
	dayTimes := make([]time.Time, n)
	for i := range dayTimes {
		dayTimes[i] = daily[symbols[0]][i].CloseTime
	}

	basketADX, firstReady := computeBasketADX(daily, symbols, adxPeriod)
	fmt.Printf("📈 ADX корзины готов с %s (прогрев %d дней)\n\n", dayTimes[firstReady].Format("2006-01-02"), firstReady)

	splitIdx := sort.Search(n, func(i int) bool { return !dayTimes[i].Before(end.AddDate(0, 0, -holdoutDays)) })
	startEquity := decimal.NewFromFloat(startEquityF)

	env := chainEnv{
		hourly: hourly, daily: daily, dailyBars: dailyBars, dayTimes: dayTimes,
		basketADX: basketADX, infos: infos, startEquity: startEquity,
	}

	grid := buildGrid()
	fmt.Printf("🔍 Комбинаций переключения в сетке: %d\n\n", len(grid))

	type scored struct {
		rp    regimeParams
		stats chainResult
	}
	var results []scored
	for _, rp := range grid {
		res := chainEquity(env, firstReady, splitIdx, rp, policySwitch)
		if res.breakoutBlocks+res.rotationBlocks < 4 {
			continue // слишком мало блоков — статистика ничего не значит
		}
		results = append(results, scored{rp, res})
	}
	if len(results) == 0 {
		fmt.Println("⚠️  Ни одна комбинация не набрала минимум блоков.")
		return nil
	}
	sort.Slice(results, func(i, j int) bool { return results[i].stats.Score().GreaterThan(results[j].stats.Score()) })

	fmt.Println("🏆 Топ переключения по in-sample:")
	for i, r := range results {
		fmt.Printf("%2d. %s | %s\n", i+1, r.rp, r.stats)
	}

	baseBreakout := chainEquity(env, firstReady, splitIdx, regimeParams{}, policyAlwaysBreakout)
	baseRotation := chainEquity(env, firstReady, splitIdx, regimeParams{}, policyAlwaysRotation)
	fmt.Printf("\n📊 Базовые линии (тот же харнесс, in-sample):\n")
	fmt.Printf("   всегда Breakout: %s\n", baseBreakout)
	fmt.Printf("   всегда Rotation: %s\n", baseRotation)

	foldBounds := buildFoldBounds(dayTimes, splitIdx, n, folds)
	fmt.Printf("\n🧪 Walk-forward на holdout (последние %d дней, %d период(ов)):\n", holdoutDays, folds)

	type validated struct {
		rp         regimeParams
		foldStats  []chainResult
		foldScores []decimal.Decimal
		agg        decimal.Decimal
	}
	validate := func(rp regimeParams, p policy) validated {
		v := validated{rp: rp}
		for _, fb := range foldBounds {
			st := chainEquity(env, fb.start, fb.end, rp, p)
			v.foldStats = append(v.foldStats, st)
			v.foldScores = append(v.foldScores, st.Score())
		}
		v.agg = meanMinusStdev(v.foldScores)
		return v
	}

	var withHoldout []validated
	for _, r := range results {
		withHoldout = append(withHoldout, validate(r.rp, policySwitch))
	}
	sort.Slice(withHoldout, func(i, j int) bool { return withHoldout[i].agg.GreaterThan(withHoldout[j].agg) })

	printValidated := func(label string, v validated) {
		fmt.Printf("\n%s | walk-forward score=%s\n", label, v.agg.StringFixed(2))
		for fi, st := range v.foldStats {
			fmt.Printf("    фолд %d (%s..%s): %s\n",
				fi+1, dayTimes[foldBounds[fi].start].Format("2006-01-02"), dayTimes[foldBounds[fi].end-1].Format("2006-01-02"), st)
		}
	}

	for i, v := range withHoldout {
		printValidated(fmt.Sprintf("%2d. %s", i+1, v.rp), v)
	}

	fmt.Println("\n📊 Те же базовые линии на holdout, walk-forward:")
	printValidated("   всегда Breakout", validate(regimeParams{}, policyAlwaysBreakout))
	printValidated("   всегда Rotation", validate(regimeParams{}, policyAlwaysRotation))

	best := withHoldout[0]
	fmt.Printf("\n✅ Лучшее переключение по walk-forward: %s\n", best.rp)
	fmt.Println("   Сравнивай walk-forward score выше с двумя базовыми линиями — переключение " +
		"оправдано только если оно устойчиво ЛУЧШЕ обеих, а не просто положительно.")
	return nil
}

// chainEnv — общие только-для-чтения данные одного прогона; передаётся по
// значению (map — ссылочный тип, копия дешёвая) через все вызовы chainEquity.
type chainEnv struct {
	hourly      map[string][]domain.Candle
	daily       map[string][]domain.Candle
	dailyBars   map[string][]rotation.DailyBar
	dayTimes    []time.Time
	basketADX   []decimal.Decimal
	infos       map[string]*exchange.SymbolInfo
	startEquity decimal.Decimal
}

// chainResult — итог одной цепочки блоков (по духу как rotation.Stats, но с
// разбивкой по тому, сколько блоков ушло на какую стратегию).
type chainResult struct {
	startEquity, finalEquity, maxDrawdown decimal.Decimal
	breakoutBlocks, rotationBlocks        int
}

func (r chainResult) ReturnPct() decimal.Decimal {
	if !r.startEquity.IsPositive() {
		return decimal.Zero
	}
	return r.finalEquity.Sub(r.startEquity).Div(r.startEquity).Mul(decimal.NewFromInt(100))
}

func (r chainResult) Score() decimal.Decimal {
	return r.ReturnPct().Sub(r.maxDrawdown.Mul(decimal.NewFromInt(100)).Mul(decimal.NewFromFloat(0.5)))
}

func (r chainResult) String() string {
	return fmt.Sprintf("блоков breakout=%d/rotation=%d | эквити %s → %s (%s%%) | макс. просадка %s%%",
		r.breakoutBlocks, r.rotationBlocks, r.startEquity.StringFixed(2), r.finalEquity.StringFixed(2),
		r.ReturnPct().StringFixed(2), r.maxDrawdown.Mul(decimal.NewFromInt(100)).StringFixed(2))
}

// chainEquity нарезает [startDay, endDay) на блоки по rp.blockDays и гоняет
// каждый блок целиком через одну из двух стратегий — итог одного блока
// становится стартовым эквити следующего, какая бы стратегия его ни вела.
// Решение "какая стратегия ведёт блок" принимается ОДИН раз в начале блока
// по значению ADX корзины на этот день — дальше блок не пересматривается,
// это и есть защита от дёрганого переключения (гистерезис через длину блока,
// не через отдельный параметр).
func chainEquity(env chainEnv, startDay, endDay int, rp regimeParams, p policy) chainResult {
	equity := env.startEquity
	peak := equity
	maxDD := decimal.Zero
	var nBreakout, nRotation int

	blockDays := rp.blockDays
	if blockDays < 1 {
		blockDays = 7
	}

	for d := startDay; d < endDay; d += blockDays {
		blockEnd := d + blockDays
		if blockEnd > endDay {
			blockEnd = endDay
		}
		if blockEnd <= d || !equity.IsPositive() {
			break
		}

		useBreakout := decidePolicy(p, env.basketADX[d], rp.adxThreshold)
		if useBreakout {
			equity = runBreakoutBlock(env, d, blockEnd, equity)
			nBreakout++
		} else {
			st := rotation.Simulate(env.dailyBars, symbols, d, blockEnd, bestRotationParams(),
				equity, backtest.TakerFeeRate, backtest.DefaultSlippagePct)
			equity = st.FinalEquity
			nRotation++
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

	return chainResult{startEquity: env.startEquity, finalEquity: equity, maxDrawdown: maxDD,
		breakoutBlocks: nBreakout, rotationBlocks: nRotation}
}

func decidePolicy(p policy, basketADXAtStart decimal.Decimal, threshold float64) bool {
	switch p {
	case policyAlwaysBreakout:
		return true
	case policyAlwaysRotation:
		return false
	default:
		if basketADXAtStart.IsNegative() {
			return false // ADX ещё не прогрет на этот день — по умолчанию безопаснее в Rotation (есть стоп по ребалансу, не по цене)
		}
		return basketADXAtStart.GreaterThanOrEqual(decimal.NewFromFloat(threshold))
	}
}

// runBreakoutBlock прогоняет Breakout по всем 7 символам за [dayStart,
// dayEnd) часовых свечей, поровну разделив totalEquity между символами
// (равновзвешенный риск-бюджет), и возвращает сумму их финального эквити.
// Символу, для которого в блоке не хватило часовых данных (после прогрева),
// эквити оставляется без изменений — блок для него просто не торговался.
func runBreakoutBlock(env chainEnv, dayStart, dayEnd int, totalEquity decimal.Decimal) decimal.Decimal {
	params := bestBreakoutParams()
	warmupBars := maxInt(params.LookbackBars, params.TrendEMAPeriod, params.ATRPeriod, params.VolumeAvgPeriod)
	perSymbol := totalEquity.Div(decimal.NewFromInt(int64(len(symbols))))
	blockStartT := env.dayTimes[dayStart]
	var blockEndT time.Time
	if dayEnd < len(env.dayTimes) {
		blockEndT = env.dayTimes[dayEnd]
	} else {
		blockEndT = env.dayTimes[len(env.dayTimes)-1].Add(24 * time.Hour)
	}

	sum := decimal.Zero
	ctx := context.Background()
	for _, sym := range symbols {
		full := env.hourly[sym]
		startIdx := sort.Search(len(full), func(i int) bool { return !full[i].CloseTime.Before(blockStartT) })
		endIdx := sort.Search(len(full), func(i int) bool { return !full[i].CloseTime.Before(blockEndT) })
		from := startIdx - warmupBars
		if from < 0 {
			from = 0
		}
		if endIdx > len(full) {
			endIdx = len(full)
		}
		wu := startIdx - from
		if endIdx <= from+wu {
			sum = sum.Add(perSymbol) // данных на блок не хватило — эквити без изменений
			continue
		}

		hist := full[from:endIdx]
		sim := backtest.NewSimExecutor(sym, env.infos[sym], perSymbol, backtest.TakerFeeRate, backtest.DefaultSlippagePct)
		bot := strategy.NewBreakout(sim, sim, backtest.AlwaysAllowRiskGate{}, nil, params)
		bot.Warmup(hist[:wu])
		for _, c := range hist[wu:] {
			if ev := sim.OnCandle(c); ev != nil {
				bot.OnOrderEvent(ctx, *ev)
			}
			bot.OnCandle(ctx, c)
		}
		eq, err := sim.Equity(ctx)
		if err != nil {
			eq = perSymbol
		}
		sum = sum.Add(eq)
	}
	return sum
}

// computeBasketADX считает дневной ADX(period) по каждому символу и
// усредняет по корзине на каждый день. До того как ВСЕ символы прогрелись,
// значение -1 (валидный ADX всегда >= 0) — сигнальный маркер "ещё не готово".
// firstReady — первый индекс, начиная с которого корзина полностью прогрета.
func computeBasketADX(daily map[string][]domain.Candle, syms []string, period int) (out []decimal.Decimal, firstReady int) {
	n := len(daily[syms[0]])
	out = make([]decimal.Decimal, n)
	adxes := make(map[string]*indicator.ADX, len(syms))
	for _, s := range syms {
		adxes[s] = indicator.NewADX(period)
	}
	firstReady = -1
	for i := 0; i < n; i++ {
		sum := decimal.Zero
		allReady := true
		for _, s := range syms {
			c := daily[s][i]
			v := adxes[s].Update(c.High, c.Low, c.Close)
			if !adxes[s].Ready() {
				allReady = false
				continue
			}
			sum = sum.Add(v)
		}
		if allReady {
			out[i] = sum.Div(decimal.NewFromInt(int64(len(syms))))
			if firstReady < 0 {
				firstReady = i
			}
		} else {
			out[i] = decimal.NewFromInt(-1)
		}
	}
	if firstReady < 0 {
		firstReady = n // никогда не прогрелось — вызывающий код упадёт на нехватке блоков, а не панике
	}
	return out, firstReady
}

// foldBound — индексы [start, end) в общем массиве dayTimes.
type foldBound struct{ start, end int }

func buildFoldBounds(dayTimes []time.Time, splitIdx, n, folds int) []foldBound {
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

func buildGrid() []regimeParams {
	thresholds := []float64{15, 20, 25, 30, 35}
	blockDays := []int{7, 14, 30}
	var grid []regimeParams
	for _, th := range thresholds {
		for _, bd := range blockDays {
			grid = append(grid, regimeParams{adxThreshold: th, blockDays: bd})
		}
	}
	return grid
}

func loadOrFetch(ctx context.Context, client *futures.Client, cacheDir, symbol, ivl string, days int, start, end time.Time) ([]domain.Candle, error) {
	path := filepath.Join(cacheDir, fmt.Sprintf("%s_%s_%dd.json", symbol, ivl, days))
	if data, err := os.ReadFile(path); err == nil {
		var candles []domain.Candle
		if err := json.Unmarshal(data, &candles); err == nil && len(candles) > 0 {
			return candles, nil
		}
	}

	candles, err := exchange.LoadHistoricalCandles(ctx, client, symbol, ivl, start, end)
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

func maxInt(vals ...int) int {
	m := vals[0]
	for _, v := range vals[1:] {
		if v > m {
			m = v
		}
	}
	return m
}
