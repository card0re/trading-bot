package main

import (
	"context"
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	exchange "trading-bot/internal/exchange/binance"
	"trading-bot/internal/predict"

	spotapi "github.com/adshao/go-binance/v2"
)

// reportRec сводит CSV от cmd/predictrec с таблицей реальной вероятности:
// сколько раз дальнюю сторону продавали дёшево, какое ожидание по модели,
// чем это кончилось на деле, и совпадает ли исход по спот-свечам с исходом площадки.
func reportRec(ctx context.Context, client *spotapi.Client, dir, symbol string, table map[predict.Key]*predict.Cell, lifts map[int]map[int]*predict.Lift) error {
	rows, err := readRec(dir, symbol)
	if err != nil {
		return err
	}
	fmt.Printf("\n── запись площадки %s: %d строк\n", symbol, len(rows))
	if len(rows) == 0 {
		return nil
	}
	minT, maxT := rows[0].T, rows[0].T
	for _, r := range rows {
		minT, maxT = min(minT, r.T), max(maxT, r.T)
	}
	end := time.Now().UTC().Truncate(time.Minute)
	candles, err := exchange.LoadHistoricalSpotCandles(ctx, client, symbol, "1m", time.UnixMilli(minT).Add(-2*time.Hour), time.UnixMilli(maxT).Add(20*time.Minute))
	if err != nil {
		return err
	}
	var bars []predict.Bar
	for _, c := range candles {
		if c.CloseTime.Before(end) {
			bars = append(bars, predict.Bar{T: c.OpenTime.Unix(), O: c.Open.InexactFloat64(), C: c.Close.InexactFloat64()})
		}
	}
	idx := make(map[int64]int, len(bars))
	for i, b := range bars {
		idx[b.T] = i
	}

	// Совпадение исходов: спот Binance vs Chainlink площадки.
	agree, total := 0, 0
	for _, r := range rows {
		n := predict.WindowMinutes(r.Slug)
		oi, ok1 := idx[r.WindowStart/1000]
		li, ok2 := idx[r.WindowStart/1000+int64(n-1)*60]
		if r.Side != "RESULT" || n == 0 || !ok1 || !ok2 {
			continue
		}
		label := "Up"
		if bars[li].C < bars[oi].O {
			label = "Down"
		}
		total++
		if strings.EqualFold(label, r.Winner) {
			agree++
		}
	}
	if total > 0 {
		fmt.Printf("   исход по спот-свечам совпал с площадкой в %d из %d окон (%.1f%%)\n", agree, total, 100*float64(agree)/float64(total))
	}

	opps := predict.Opportunities(rows, bars, table, lifts)
	fmt.Printf("   снимков «дальняя сторона ≤ $%.2f, на лучшем ask ≥ $1»: %d; p модели — с поправкой на группу часов\n", predict.MaxAsk, len(opps))

	// Ячейки отчёта: окно × ценовая полоса × минут до конца. Ставка — первый
	// момент в окне для своей ячейки: соседние снимки одной ситуации не независимы.
	bands := []float64{0.05, 0.10, 0.15}
	band := func(a float64) int {
		for i, b := range bands {
			if a <= b+1e-9 {
				return i
			}
		}
		return len(bands) - 1
	}
	leftGroup := func(m int) int {
		switch {
		case m <= 1:
			return 0
		case m <= 4:
			return 1
		}
		return 2
	}
	type cellKey struct{ window, band, left int }
	type betKey struct {
		cellKey
		start int64
	}
	bets := map[betKey]predict.Opp{}
	for _, o := range opps {
		ck := cellKey{o.Window, band(o.Ask), leftGroup(o.MinLeft)}
		bk := betKey{ck, o.T - o.T%(int64(o.Window)*60_000)}
		if _, ok := bets[bk]; !ok {
			bets[bk] = o
		}
	}
	type agg struct {
		n, resolved, won, hotN, hotWon, hotRes int
		ask, model, ev, pnl, hotPnl            float64
	}
	cells := map[cellKey]*agg{}
	for k, o := range bets {
		a := cells[k.cellKey]
		if a == nil {
			a = &agg{}
			cells[k.cellKey] = a
		}
		a.n++
		a.ask += o.Ask
		a.model += o.ModelP
		a.ev += o.EV
		if !o.Resolved {
			continue
		}
		res := -1.0
		if o.Won {
			res += (1 - predict.Fee) / o.Ask
		}
		a.resolved++
		a.pnl += res
		if o.Won {
			a.won++
		}
		if o.HourGroup == 1 {
			a.hotRes++
			a.hotPnl += res
			if o.Won {
				a.hotWon++
			}
		}
	}
	bandName := []string{"≤$0.05", "$0.05–0.10", "$0.10–0.15"}
	leftName := []string{"≤1 мин", "2–4 мин", "5+ мин"}
	fmt.Printf("   %4s %-11s %-7s %5s %7s %8s %9s %12s %10s %16s\n", "окно", "цена", "осталось", "окон", "ср.цена", "модель p", "ожид.мод", "сыграло", "итог $", "из них 12–15 UTC")
	for _, w := range []int{5, 15} {
		for b := range bands {
			for l := range leftName {
				a := cells[cellKey{w, b, l}]
				if a == nil {
					continue
				}
				n := float64(a.n)
				fmt.Printf("   %3dm %-11s %-7s %5d %7.3f %7.2f%% %+8.0f%% %5d из %-4d %+9.2f   %d из %d, $%+.2f\n",
					w, bandName[b], leftName[l], a.n, a.ask/n, 100*a.model/n, 100*a.ev/n, a.won, a.resolved, a.pnl, a.hotWon, a.hotRes, a.hotPnl)
			}
		}
	}
	return nil
}

func readRec(dir, symbol string) ([]predict.RecRow, error) {
	files, err := filepath.Glob(filepath.Join(dir, "predict_*.csv"))
	if err != nil {
		return nil, err
	}
	var rows []predict.RecRow
	for _, path := range files {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		recs, err := csv.NewReader(f).ReadAll()
		f.Close()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		for _, r := range recs {
			if len(r) != 9 || r[0] == "ts_ms" || strings.ToUpper(strings.SplitN(r[1], "-", 2)[0])+"USDT" != symbol {
				continue
			}
			row := predict.RecRow{Slug: r[1], Side: r[3], Winner: r[8]}
			row.T, _ = strconv.ParseInt(r[0], 10, 64)
			row.WindowStart, _ = strconv.ParseInt(r[2], 10, 64)
			if r[5] != "" {
				row.Ask, _ = strconv.ParseFloat(r[5], 64)
				row.HasAsk = true
			}
			row.Depth, _ = strconv.ParseFloat(r[6], 64)
			row.Spot, _ = strconv.ParseFloat(r[7], 64)
			rows = append(rows, row)
		}
	}
	return rows, nil
}
