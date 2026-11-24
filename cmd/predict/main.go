// Команда predict проверяет гипотезу для Binance Prediction (up/down 5m/15m):
// если цена внутри окна сильно ушла от цены открытия, как часто окно всё же
// закрывается по другую сторону — и при каком коэффициенте ставка на разворот
// выходит в ноль (с комиссией 2%). Сравнивается с гауссовой «честной»
// вероятностью Φ(−|z|): где реальность заметно выше — хвосты толще модели.
//
// Данные — спот 1m klines Binance (исход площадки считается по Chainlink
// BTC/USDT mid — близкий, но не точный прокси). Дизайн:
// docs/superpowers/specs/2026-11-24-predict-reversal-design.md
package main

import (
	"context"
	"encoding/gob"
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	exchange "trading-bot/internal/exchange/binance"
	"trading-bot/internal/predict"

	spotapi "github.com/adshao/go-binance/v2"
)

func main() {
	symbols := flag.String("symbols", "BTCUSDT,ETHUSDT", "символы через запятую")
	days := flag.Int("days", 730, "глубина истории, дней")
	cacheDir := flag.String("cache", filepath.Join(os.TempDir(), "trading-bot-predict-cache"), "каталог кэша минутных баров")
	odds := flag.Float64("odds", 50, "коэффициент входа для симуляции банкролла")
	bank := flag.Int("bank", 100, "банк в $ для симуляции (ставка $1)")
	minN := flag.Int("minn", 30, "не печатать ячейки, где точек меньше")
	rec := flag.String("rec", "", "каталог CSV от cmd/predictrec — сравнить реальные цены площадки с моделью")
	spotBase := flag.String("spot-base", "", "другой адрес спот-API, напр. https://data-api.binance.vision (зеркало рыночных данных: api.binance.com режет США)")
	flag.Parse()

	ctx := context.Background()
	client := spotapi.NewClient("", "")
	if *spotBase != "" {
		client.BaseURL = *spotBase
	}
	for _, sym := range strings.Split(*symbols, ",") {
		bars, err := loadBars(ctx, client, *cacheDir, sym, *days)
		if err != nil {
			log.Fatalf("❌ %s: %v", sym, err)
		}
		fmt.Printf("\n══════ %s: %d минутных баров (%s … %s)\n", sym, len(bars),
			time.Unix(bars[0].T, 0).UTC().Format("2006-01-02"), time.Unix(bars[len(bars)-1].T, 0).UTC().Format("2006-01-02"))
		table := map[predict.Key]*predict.Cell{}
		lifts := map[int]map[int]*predict.Lift{}
		for _, w := range []int{5, 15} {
			t, l := report(predict.Samples(bars, w), w, *minN, *odds, *bank)
			lifts[w] = l
			for k, c := range t {
				table[k] = c
			}
		}
		if *rec != "" {
			if err := reportRec(ctx, client, *rec, sym, table, lifts); err != nil {
				log.Fatalf("❌ -rec: %v", err)
			}
		}
	}
}

func report(s []predict.Sample, window, minN int, odds float64, bank int) (map[predict.Key]*predict.Cell, map[int]*predict.Lift) {
	if len(s) == 0 {
		fmt.Printf("\n— окно %dm: нет точек\n", window)
		return nil, nil
	}
	table := predict.Tabulate(s)
	keys := sortedKeys(table)

	fmt.Printf("\n── окно %dm, %d точек входа. p — реальная частота разворота, gaussP — по модели Φ(−|z|), K* — безубыточный кэф\n", window, len(s))
	fmt.Printf("%5s %9s %8s %6s %7s %17s %7s %7s %8s\n", "мин", "|z|", "n", "разв", "p", "CI95", "gaussP", "p/g", "K*")
	for _, k := range keys {
		c := table[k]
		if c.N < minN {
			continue
		}
		lo, hi := c.Wilson()
		mark := ""
		if lo > c.GaussP {
			mark = " ◀ хвост толще модели"
		}
		fmt.Printf("%5d %9s %8d %6d %6.2f%% [%5.2f%%,%6.2f%%] %6.2f%% %7.2f %8s%s\n",
			k.MinLeft, bucketLabel(k.Bucket), c.N, c.Rev, 100*c.P(), 100*lo, 100*hi, 100*c.GaussP, ratio(c.P(), c.GaussP), fmtK(c.Breakeven()), mark)
	}

	// Устойчивость: те же ячейки на первых 70% истории и на последних 30%.
	split := s[0].T + (s[len(s)-1].T-s[0].T)*7/10
	var first, last []predict.Sample
	for _, x := range s {
		if x.T < split {
			first = append(first, x)
		} else {
			last = append(last, x)
		}
	}
	tf, tl := predict.Tabulate(first), predict.Tabulate(last)
	fmt.Printf("\n   устойчивость (ячейки с K* 20…150): p первые 70%% → p последние 30%% (split %s)\n", time.Unix(split, 0).UTC().Format("2006-01-02"))
	for _, k := range keys {
		c, a, b := table[k], tf[k], tl[k]
		if c.N < minN || a == nil || b == nil {
			continue
		}
		if be := c.Breakeven(); be < 20 || be > 150 {
			continue
		}
		fmt.Printf("   %2d мин, |z| %-8s %6.2f%% (n=%d) → %6.2f%% (n=%d)\n", k.MinLeft, bucketLabel(k.Bucket), 100*a.P(), a.N, 100*b.P(), b.N)
	}

	// Время суток и день недели: в зоне «лотереи» (|z| ≥ 2, ≥ 2 мин до конца)
	// во сколько раз разворотов больше/меньше, чем ждёт таблица для тех же ячеек.
	// Рядом — то же на первых 70% и последних 30% истории: эффект, который есть
	// только в одной половине, скорее случайность (24 часа × 95% CI ≈ 1 ложная «▲/▼»).
	lift := func(group func(predict.Sample) int) [3]map[int]*predict.Lift {
		return [3]map[int]*predict.Lift{predict.LiftBy(s, table, group, predict.InZone), predict.LiftBy(first, table, group, predict.InZone), predict.LiftBy(last, table, group, predict.InZone)}
	}
	printLift("час UTC", lift(predict.HourUTC), 24, func(g int) string { return fmt.Sprintf("%02d:00", g) })
	printHourAdjustCheck(first, last, minN)
	days := []string{"вс", "пн", "вт", "ср", "чт", "пт", "сб"}
	printLift("день недели (UTC)", lift(predict.WeekdayUTC), 7, func(g int) string { return days[g] })

	// Банкролл: лучшая ячейка, где даже нижняя граница CI окупает кэф odds.
	need := 1 / ((1 - predict.Fee) * odds)
	var best *predict.Cell
	var bestK predict.Key
	for _, k := range keys {
		c := table[k]
		if lo, _ := c.Wilson(); c.N >= minN && lo >= need && (best == nil || c.P() < best.P()) {
			best, bestK = c, k // берём самую «дальнюю» (меньшая p) — именно там дают высокий кэф
		}
	}
	if best == nil {
		fmt.Printf("\n   ячеек, где разворот надёжно (CI95) чаще 1/%.0f, нет — при честном кэфе %.0fx преимущества не видно\n", 1/need, odds)
	} else {
		fmt.Printf("\n   ячейка %d мин, |z| %s: p=%.2f%% ≥ %.2f%% — ЕСЛИ площадка даёт там %.0fx:\n", bestK.MinLeft, bucketLabel(bestK.Bucket), 100*best.P(), 100*need, odds)
		printBankroll(best.P(), odds, bank)
	}
	fmt.Printf("   для сравнения, «1 из 40» при %.0fx:\n", odds)
	printBankroll(1.0/40, odds, bank)
	return table, predict.LiftBy(s, table, predict.HourGroup, predict.InZone)
}

func printLift(title string, ls [3]map[int]*predict.Lift, groups int, label func(int) string) {
	l := ls[0]
	fmt.Printf("\n   %s — зона |z|≥2, ≥2 мин: развороты / ожидание по таблице (>1 — чаще среднего)\n", title)
	for g := 0; g < groups; g++ {
		x := l[g]
		if x == nil || x.N == 0 {
			continue
		}
		lo, hi := x.RatioCI()
		mark := ""
		switch {
		case lo > 1:
			mark = " ▲ надёжно чаще"
		case hi < 1:
			mark = " ▼ надёжно реже"
		}
		half := func(m map[int]*predict.Lift) string {
			if m[g] == nil {
				return "  — "
			}
			return fmt.Sprintf("×%.2f", m[g].Ratio())
		}
		fmt.Printf("   %-6s n=%6d  разв %4d / ждали %6.1f  ×%.2f [%.2f–%.2f]  70%%/30%%: %s → %s%s\n", label(g), x.N, x.Rev, x.Exp, x.Ratio(), lo, hi, half(ls[1]), half(ls[2]), mark)
	}
}

func printBankroll(p, odds float64, bank int) {
	st := predict.SimulateBankroll(p, odds, bank, 1000, 2000, 1)
	fmt.Printf("     1000 ставок по $1, банк $%d: разорение %.1f%%, медианный итог $%.0f, макс. серия проигрышей медиана %d / 95%% %d\n",
		bank, 100*st.RuinProb, st.MedianFinal, st.MedianMaxLoseStreak, st.P95MaxLoseStreak)
}

func sortedKeys(t map[predict.Key]*predict.Cell) []predict.Key {
	keys := make([]predict.Key, 0, len(t))
	for k := range t {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].MinLeft != keys[j].MinLeft {
			return keys[i].MinLeft > keys[j].MinLeft
		}
		return keys[i].Bucket < keys[j].Bucket
	})
	return keys
}

func bucketLabel(b int) string {
	hi := predict.ZEdges[b+1]
	if math.IsInf(hi, 1) {
		return fmt.Sprintf("%g+", predict.ZEdges[b])
	}
	return fmt.Sprintf("%g–%g", predict.ZEdges[b], hi)
}

func ratio(a, b float64) float64 {
	if b == 0 {
		return math.Inf(1)
	}
	return a / b
}

func fmtK(k float64) string {
	if math.IsInf(k, 1) {
		return "∞"
	}
	return fmt.Sprintf("%.1f", k)
}

// loadBars — минутные бары спота за days дней, с gob-кэшем.
// shortcut: кэш привязан к числу дней, не к дате — чтобы освежить, удалить файл.
func loadBars(ctx context.Context, client *spotapi.Client, cacheDir, symbol string, days int) ([]predict.Bar, error) {
	path := filepath.Join(cacheDir, fmt.Sprintf("%s_1m_%dd.gob", symbol, days))
	if f, err := os.Open(path); err == nil {
		var bars []predict.Bar
		err := gob.NewDecoder(f).Decode(&bars)
		f.Close()
		if err == nil && len(bars) > 0 {
			return bars, nil
		}
	}

	end := time.Now().UTC().Truncate(time.Minute)
	start := end.AddDate(0, 0, -days)
	seen := make(map[int64]bool)
	var bars []predict.Bar
	// Кусками по 30 дней: decimal-свечи за 2 года целиком — сотни МБ.
	for from := start; from.Before(end); from = from.AddDate(0, 0, 30) {
		to := from.AddDate(0, 0, 30)
		if to.After(end) {
			to = end
		}
		candles, err := exchange.LoadHistoricalSpotCandles(ctx, client, symbol, "1m", from, to)
		if err != nil {
			return nil, err
		}
		for _, c := range candles {
			t := c.OpenTime.Unix()
			if seen[t] || !c.CloseTime.Before(end) {
				continue // дубли на стыках кусков и ещё не закрытая минута
			}
			seen[t] = true
			bars = append(bars, predict.Bar{T: t, O: c.Open.InexactFloat64(), C: c.Close.InexactFloat64()})
		}
		fmt.Printf("  📥 %s: %s, всего %d баров\n", symbol, to.Format("2006-01-02"), len(bars))
	}
	if len(bars) == 0 {
		return nil, fmt.Errorf("пустая история")
	}
	sort.Slice(bars, func(i, j int) bool { return bars[i].T < bars[j].T })

	if err := os.MkdirAll(cacheDir, 0o755); err == nil {
		if f, err := os.Create(path); err == nil {
			_ = gob.NewEncoder(f).Encode(bars)
			f.Close()
		}
	}
	return bars, nil
}

// printHourAdjustCheck — честная проверка поправки на час: таблица и коэффициенты
// часов строятся по первым 70% истории, прогноз проверяется на последних 30%.
func printHourAdjustCheck(first, last []predict.Sample, minN int) {
	tf := predict.Tabulate(first)
	lf := predict.LiftBy(first, tf, predict.HourGroup, predict.InZone)
	var zone []predict.Sample
	for _, x := range last {
		if predict.InZone(x) {
			zone = append(zone, x)
		}
	}
	cell := func(x predict.Sample) (float64, bool) {
		c := tf[predict.Key{Window: x.Window, MinLeft: x.MinLeft, Bucket: predict.Bucket(x.Z)}]
		if c == nil || c.N < minN {
			return 0, false
		}
		return c.P(), true
	}
	adj := func(x predict.Sample) (float64, bool) {
		p, ok := cell(x)
		return predict.AdjustP(p, lf[predict.HourGroup(x)]), ok
	}
	l0, n := predict.LogLoss(zone, cell)
	l1, _ := predict.LogLoss(zone, adj)
	verdict := "лучше"
	if l1 >= l0 {
		verdict = "НЕ лучше"
	}
	fmt.Printf("\n   поправка на группу часов 12–15/15–20/прочие (обучено на 70%%, проверено на 30%%, n=%d): лог-лосс %.5f → %.5f — с поправкой %s\n", n, l0, l1, verdict)

	// Горячие часы 12–15 UTC на проверочной части: факт против двух прогнозов.
	var rev int
	var e0, e1 float64
	for _, x := range zone {
		if h := predict.HourUTC(x); h < 12 || h >= 15 {
			continue
		}
		p, ok := cell(x)
		if !ok {
			continue
		}
		e0 += p
		q, _ := adj(x)
		e1 += q
		if x.Reversal {
			rev++
		}
	}
	fmt.Printf("   12–15 UTC на последних 30%%: разворотов %d, ждали без поправки %.1f, с поправкой %.1f\n", rev, e0, e1)
}
