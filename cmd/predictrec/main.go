// Команда predictrec записывает стакан Binance Prediction (up/down 5m/15m) в
// CSV, чтобы потом сравнить цены площадки с реальной вероятностью разворота
// (cmd/predict -rec). Только чтение: никаких ставок, ключ не нужен.
//
// Строки: side=UP/DOWN — лучший bid/ask исхода и сколько USDT стоит выкупить
// всё, что лежит на лучшем ask; side=RESULT — победитель окна.
//
//	go run ./cmd/predictrec -every 2s -out ./data/predict
package main

import (
	"context"
	"encoding/csv"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	exchange "trading-bot/internal/exchange/binance"
	"trading-bot/internal/predict"

	spotapi "github.com/adshao/go-binance/v2"
)

var header = []string{"ts_ms", "slug", "window_start_ms", "side", "best_bid", "best_ask", "ask_best_usdt", "spot", "winner"}

func windowMinutes(slug string) (int, error) {
	if n := predict.WindowMinutes(slug); n != 0 {
		return n, nil
	}
	return 0, fmt.Errorf("окно %q не поддерживается (нужен суффикс -5m/-15m)", slug)
}

func spotSymbol(slug string) string {
	return strings.ToUpper(strings.SplitN(slug, "-", 2)[0]) + "USDT"
}

type market struct {
	slug    string
	minutes int
	cur     exchange.PredWindow
	pending []exchange.PredWindow // закрытые окна, ждём победителя
	checked time.Time             // когда последний раз спрашивали победителя
}

func main() {
	slugs := flag.String("slugs", "btc-up-or-down-5m,btc-up-or-down-15m,eth-up-or-down-5m,eth-up-or-down-15m", "рынки через запятую")
	every := flag.Duration("every", 2*time.Second, "период опроса стакана")
	outDir := flag.String("out", "./data/predict", "каталог CSV")
	dump := flag.Bool("dump", false, "напечатать текущее окно первого рынка и выйти")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	hc := &http.Client{Timeout: 5 * time.Second}

	var markets []*market
	syms := map[string]bool{}
	for _, s := range strings.Split(*slugs, ",") {
		m, err := windowMinutes(s)
		if err != nil {
			log.Fatalf("❌ %v", err)
		}
		markets = append(markets, &market{slug: s, minutes: m})
		syms[spotSymbol(s)] = true
	}
	if *dump {
		m := markets[0]
		w, err := exchange.FetchPredWindow(ctx, hc, m.slug, time.Now().UTC().Truncate(time.Duration(m.minutes)*time.Minute).UnixMilli())
		fmt.Printf("%+v %v\n", w, err)
		return
	}
	var symList []string
	for s := range syms {
		symList = append(symList, s)
	}
	spot := spotapi.NewClient("", "")

	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		log.Fatalf("❌ %v", err)
	}
	w := &dayWriter{dir: *outDir}
	defer w.close()

	// Cloud Run шлёт трафик/health-check на $PORT — отвечаем временем последнего тика.
	var lastTick atomic.Int64
	if port := os.Getenv("PORT"); port != "" {
		go func() {
			http.HandleFunc("/", func(rw http.ResponseWriter, _ *http.Request) {
				fmt.Fprintf(rw, "predictrec ok, последний тик %s\n", time.UnixMilli(lastTick.Load()).UTC().Format(time.RFC3339))
			})
			log.Print(http.ListenAndServe(":"+port, nil))
		}()
	}

	log.Printf("▶ пишу %d рынков каждые %s в %s (Ctrl+C — стоп)", len(markets), *every, *outDir)
	tick := time.NewTicker(*every)
	defer tick.Stop()
	for {
		now := time.Now().UTC()
		prices := map[string]string{}
		if ps, err := spot.NewListPricesService().Symbols(symList).Do(ctx); err == nil {
			for _, p := range ps {
				prices[p.Symbol] = p.Price
			}
		} else {
			log.Printf("⚠️ спот: %v", err)
		}
		var wg sync.WaitGroup // рынки параллельно: последовательно тик шёл ~4 с вместо 2
		for _, m := range markets {
			wg.Go(func() { poll(ctx, hc, w, m, now, prices[spotSymbol(m.slug)]) })
		}
		wg.Wait()
		lastTick.Store(now.UnixMilli())
		if err := w.flush(); err != nil {
			log.Printf("⚠️ запись CSV: %v", err)
		}
		select {
		case <-ctx.Done():
			log.Print("■ стоп")
			return
		case <-tick.C:
		}
	}
}

func poll(ctx context.Context, hc *http.Client, w *dayWriter, m *market, now time.Time, spot string) {
	start := now.Truncate(time.Duration(m.minutes) * time.Minute).UnixMilli()
	if m.cur.Start != start {
		if m.cur.MarketID != 0 {
			m.pending = append(m.pending, m.cur)
		}
		win, err := exchange.FetchPredWindow(ctx, hc, m.slug, start)
		if err != nil {
			log.Printf("⚠️ %s окно %d: %v", m.slug, start, err)
			m.cur = exchange.PredWindow{} // повторим на следующем тике
		} else {
			m.cur = win
		}
	}
	if m.cur.MarketID != 0 {
		for _, side := range []struct{ name, token string }{{"UP", m.cur.UpToken}, {"DOWN", m.cur.DownToken}} {
			b, err := exchange.FetchPredBook(ctx, hc, m.cur.MarketID, side.token)
			if err != nil {
				log.Printf("⚠️ %s %s стакан: %v", m.slug, side.name, err)
				continue
			}
			w.write(now, []string{m.slug, i64(m.cur.Start), side.name, price(b.BestBid, b.HasBid), price(b.BestAsk, b.HasAsk),
				strconv.FormatFloat(b.AskBestUSDT, 'f', 2, 64), spot, ""})
		}
	}

	// Победителя проверяем раз в ~10 с после конца окна + 30 с; через 15 мин сдаёмся.
	if now.Sub(m.checked) < 10*time.Second {
		return
	}
	m.checked = now
	keep := m.pending[:0]
	for _, p := range m.pending {
		endAt := time.UnixMilli(p.End)
		switch {
		case now.Before(endAt.Add(30 * time.Second)):
			keep = append(keep, p)
		case now.After(endAt.Add(15 * time.Minute)):
			log.Printf("⚠️ %s окно %d: победитель так и не пришёл", m.slug, p.Start)
		default:
			res, err := exchange.FetchPredWindow(ctx, hc, m.slug, p.Start)
			if err != nil || res.Winner == "" {
				keep = append(keep, p)
				continue
			}
			w.write(now, []string{m.slug, i64(p.Start), "RESULT", "", "", "", "", res.Winner})
		}
	}
	m.pending = keep
}

func i64(v int64) string { return strconv.FormatInt(v, 10) }

// price пишет пусто, если стороны нет: 0 читался бы как реальная цена.
func price(p float64, ok bool) string {
	if !ok {
		return ""
	}
	return strconv.FormatFloat(p, 'f', 4, 64)
}

// dayWriter — CSV на каждый UTC-час, дописывает, заголовок только в новый файл.
type dayWriter struct {
	mu   sync.Mutex
	dir  string
	day  string
	f    *os.File
	csvw *csv.Writer
}

func (d *dayWriter) write(now time.Time, row []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	// Файл на каждый час: на Cloud Run каталог — бакет GCS (gcsfuse), файл уходит
	// в бакет при закрытии, так что при падении теряется не больше часа.
	day := now.Format("2006-01-02_15")
	if day != d.day {
		d.close()
		path := filepath.Join(d.dir, "predict_"+day+".csv")
		_, statErr := os.Stat(path)
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			log.Printf("⚠️ %v", err)
			return
		}
		d.day, d.f, d.csvw = day, f, csv.NewWriter(f)
		if os.IsNotExist(statErr) {
			_ = d.csvw.Write(header)
		}
	}
	_ = d.csvw.Write(append([]string{i64(now.UnixMilli())}, row...))
}

func (d *dayWriter) flush() error {
	if d.csvw == nil {
		return nil
	}
	d.csvw.Flush()
	return d.csvw.Error()
}

func (d *dayWriter) close() {
	if d.f != nil {
		_ = d.flush()
		d.f.Close()
		d.f, d.csvw, d.day = nil, nil, ""
	}
}
