// Команда carrybot — живой (но виртуальный) forward-тест delta-neutral
// funding-rate arbitrage (internal/carry). НЕ отправляет реальных ордеров —
// ни на споте, ни на фьючерсах: держит независимый виртуальный счёт на
// символ (та же методология, что и в бэктесте cmd/carry — 7 независимых
// $X-симуляций, не один общий пул), обновляемый по реальным funding-ставкам
// и ценам с боевой сети (публичные эндпоинты, ключ не нужен).
//
// Зачем виртуальный, а не сразу реальный: бэктест (см. doc-комментарий
// cmd/carry/main.go) показал структурно чистый результат in-sample, но
// почти пустой holdout (funding сейчас редко бывает достаточно экстремальным)
// — прежде чем занимать капитал на ДВУХ рынках сразу под непроверенное
// живое исполнение, разумно сначала увидеть, что бот правильно ловит
// следующий всплеск funding на реальных живых данных.
//
// Параметры по умолчанию — лучшая по walk-forward комбинация (25.08.2026,
// см. cmd/carry): Lookback=3 периода (1 день), EntryAnnualRate=15%,
// ExitAnnualRate=0%, MaxHold выключен.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"trading-bot/internal/carry"
	"trading-bot/internal/domain"
	exchange "trading-bot/internal/exchange/binance"
	"trading-bot/internal/logging"
	"trading-bot/internal/metrics"
	"trading-bot/internal/notify"

	spotapi "github.com/adshao/go-binance/v2"
	"github.com/adshao/go-binance/v2/futures"
	"github.com/joho/godotenv"
	"github.com/shopspring/decimal"
)

const (
	spotFeeRate    = 0.001  // Binance Spot taker без BNB-скидки/VIP
	perpFeeRate    = 0.0005 // backtest.TakerFeeRate
	maxRecentRates = 20     // с запасом над любым разумным Lookback
)

func main() {
	statePath := flag.String("state", "carry_state.json", "путь к файлу состояния виртуального портфеля")
	symbolsFlag := flag.String("symbols", "BTCUSDT,ETHUSDT,BNBUSDT,SOLUSDT,XRPUSDT,ADAUSDT,LINKUSDT", "монеты через запятую")
	lookback := flag.Int("lookback", 3, "периодов (по 8ч) для скользящего среднего ставки funding")
	entryRate := flag.Float64("entry-rate", 15, "войти, когда сглаженная ставка annualized >= это, %")
	exitRate := flag.Float64("exit-rate", 0, "выйти, когда упала ниже, %")
	maxHold := flag.Int("max-hold", 0, "макс. периодов удержания, 0 = без ограничения")
	totalEquityStr := flag.String("equity", "10000", "стартовый виртуальный эквити НА ВЕСЬ портфель (делится поровну между символами), USDT")
	checkEvery := flag.Duration("check-every", 15*time.Minute, "как часто проверять новую funding-точку")
	flag.Parse()
	logging.Setup()

	if err := godotenv.Load(); err != nil && !os.IsNotExist(err) {
		fmt.Println("ℹ️  .env не найден, читаю системные переменные")
	}

	symbols := parseSymbols(*symbolsFlag)
	totalEquity, err := decimal.NewFromString(*totalEquityStr)
	if err != nil {
		log.Fatalf("❌ --equity: %v", err)
	}
	equityPerSymbol := totalEquity.Div(decimal.NewFromInt(int64(len(symbols))))

	p := carry.Params{
		Lookback:        *lookback,
		EntryAnnualRate: decimal.NewFromFloat(*entryRate),
		ExitAnnualRate:  decimal.NewFromFloat(*exitRate),
		MaxHoldPeriods:  *maxHold,
		SpotFeeRate:     decimal.NewFromFloat(spotFeeRate),
		PerpFeeRate:     decimal.NewFromFloat(perpFeeRate),
	}

	st, err := carry.LoadState(*statePath, symbols, equityPerSymbol)
	if err != nil {
		log.Fatalf("❌ чтение состояния: %v", err)
	}

	var notifier domain.Notifier
	if token, chat := os.Getenv("TELEGRAM_BOT_TOKEN"), os.Getenv("TELEGRAM_CHAT_ID"); token != "" && chat != "" {
		notifier = notify.NewTelegram(token, chat)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	futures.UseTestnet = false // цены/funding с боевой сети — портфель виртуальный, реальных ордеров нет
	perpClient := futures.NewClient("", "")
	spotClient := spotapi.NewClient("", "")

	slog.Info(fmt.Sprintf("🚀 Carry shadow-bot | %v | lookback=%dп entry=%.0f%% exit=%.0f%% | эквити %s/символ",
		symbols, p.Lookback, *entryRate, *exitRate, equityPerSymbol.StringFixed(2)))
	if notifier != nil {
		notifier.Notify(ctx, fmt.Sprintf("💰 [CARRY] Бот запущен (виртуальный delta-neutral, без реальных ордеров)\nЭквити: %s USDT/символ × %d", equityPerSymbol.StringFixed(2), len(symbols)))
	}

	// st мутируется только внутри check() (единственный вызывающий — таймер
	// в цикле ниже, один и тот же горутин), но /metrics читает её из
	// горутины net/http — без мьютекса это была бы гонка.
	var stMu sync.Mutex

	check := func() {
		stMu.Lock()
		defer stMu.Unlock()
		for _, s := range symbols {
			if err := checkSymbol(ctx, perpClient, spotClient, &st, s, p, notifier); err != nil {
				slog.Warn(fmt.Sprintf("⚠️  %s: %v", s, err))
			}
		}
		if err := carry.SaveState(*statePath, st); err != nil {
			slog.Warn(fmt.Sprintf("⚠️  Не удалось сохранить состояние: %v", err))
		}
	}

	if addr := os.Getenv("METRICS_ADDR"); addr != "" {
		collect := func(context.Context) []metrics.Metric {
			stMu.Lock()
			snapshot := st
			stMu.Unlock()
			return carryMetrics(snapshot, symbols)
		}
		srv := &http.Server{Addr: addr, Handler: metrics.Handler(collect)}
		go func() {
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				slog.Warn(fmt.Sprintf("⚠️  Metrics-сервер остановился: %v", err))
			}
		}()
		go func() {
			<-ctx.Done()
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			srv.Shutdown(shutdownCtx)
		}()
		slog.Info(fmt.Sprintf("📊 Метрики на http://%s/metrics", addr))
	}

	check()

	ticker := time.NewTicker(*checkEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			check()
		case <-ctx.Done():
			slog.Info("🛑 Остановка (виртуальные позиции сохранены как есть)")
			return
		}
	}
}

// checkSymbol — если появилась новая (ещё не обработанная) funding-точка,
// прогоняет её через carry.ApplyEvent — ТУ ЖЕ функцию, что использует
// бэктест cmd/carry, так что живая логика гарантированно не разошлась с
// walk-forward-проверенной.
func checkSymbol(ctx context.Context, perpClient *futures.Client, spotClient *spotapi.Client, st *carry.State, symbol string, p carry.Params, notifier domain.Notifier) error {
	latest, err := latestFundingPoint(ctx, perpClient, symbol)
	if err != nil {
		return fmt.Errorf("funding rate: %w", err)
	}

	sym := st.Symbols[symbol]
	if !latest.Time.After(sym.LastFundingTime) {
		return nil // точка уже обработана, новых пока нет
	}

	spotPrice, err := lastPrice(ctx, spotClient, symbol)
	if err != nil {
		return fmt.Errorf("спот-цена: %w", err)
	}
	perpPrice, err := lastPricePerp(ctx, perpClient, symbol)
	if err != nil {
		return fmt.Errorf("перп-цена: %w", err)
	}

	sym.RecentRates = append(sym.RecentRates, latest.Rate)
	if len(sym.RecentRates) > maxRecentRates {
		sym.RecentRates = sym.RecentRates[len(sym.RecentRates)-maxRecentRates:]
	}
	annualized := carry.Annualize(carry.TrailingAvg(sym.RecentRates, p.Lookback))

	ev := carry.FundingEvent{Time: latest.Time, Rate: latest.Rate, SpotPrice: spotPrice, PerpPrice: perpPrice}
	res := carry.ApplyEvent(&sym.Equity, &sym.Position, ev, annualized, p)
	sym.LastFundingTime = latest.Time
	if sym.Equity.GreaterThan(sym.PeakEquity) {
		sym.PeakEquity = sym.Equity
	}
	st.Symbols[symbol] = sym

	switch {
	case res.Entered:
		msg := fmt.Sprintf("💰 [CARRY] %s: вход (лонг спот + шорт перп)\nСглаженная ставка: %s%% годовых | спот=%s перп=%s | эквити %s",
			symbol, annualized.StringFixed(1), spotPrice.StringFixed(4), perpPrice.StringFixed(4), sym.Equity.StringFixed(2))
		slog.Info(msg)
		if notifier != nil {
			notifier.Notify(ctx, msg)
		}
	case res.Exited:
		msg := fmt.Sprintf("💰 [CARRY] %s: выход\nСглаженная ставка: %s%% годовых | funding за период %s | базис %s | комиссия %s | эквити %s",
			symbol, annualized.StringFixed(1), res.Funding.StringFixed(2), res.BasisPnL.StringFixed(2), res.Fee.StringFixed(2), sym.Equity.StringFixed(2))
		slog.Info(msg)
		if notifier != nil {
			notifier.Notify(ctx, msg)
		}
	default:
		if sym.Position != nil {
			slog.Info(fmt.Sprintf("💰 [CARRY] %s: в позиции, funding за период %s, эквити %s", symbol, res.Funding.StringFixed(2), sym.Equity.StringFixed(2)))
		}
	}
	return nil
}

// latestFundingPoint — последняя РЕАЛИЗОВАННАЯ (не предсказанная) точка
// funding rate. Намеренно не предсказанная ставка (доступна через premium
// index непрерывно) — бэктест cmd/carry проверял решения по факту
// реализованных точек, живой бот должен смотреть на то же самое, иначе это
// была бы другая, непроверенная стратегия под тем же именем.
func latestFundingPoint(ctx context.Context, client *futures.Client, symbol string) (exchange.FundingPoint, error) {
	end := time.Now()
	start := end.Add(-3 * 24 * time.Hour) // с запасом на случай простоя бота дольше суток
	points, err := exchange.LoadFundingRateHistory(ctx, client, symbol, start, end)
	if err != nil {
		return exchange.FundingPoint{}, err
	}
	if len(points) == 0 {
		return exchange.FundingPoint{}, fmt.Errorf("нет точек funding rate за последние 3 дня")
	}
	return points[len(points)-1], nil
}

func lastPrice(ctx context.Context, client *spotapi.Client, symbol string) (decimal.Decimal, error) {
	prices, err := client.NewListPricesService().Symbol(symbol).Do(ctx)
	if err != nil || len(prices) == 0 {
		return decimal.Zero, err
	}
	return decimal.NewFromString(prices[0].Price)
}

func lastPricePerp(ctx context.Context, client *futures.Client, symbol string) (decimal.Decimal, error) {
	prices, err := client.NewListPricesService().Symbol(symbol).Do(ctx)
	if err != nil || len(prices) == 0 {
		return decimal.Zero, err
	}
	return decimal.NewFromString(prices[0].Price)
}

func carryMetrics(st carry.State, symbols []string) []metrics.Metric {
	var out []metrics.Metric
	for _, s := range symbols {
		sym, ok := st.Symbols[s]
		if !ok {
			continue
		}
		ddPct := 0.0
		if sym.PeakEquity.IsPositive() {
			ddPct, _ = sym.PeakEquity.Sub(sym.Equity).Div(sym.PeakEquity).Mul(decimal.NewFromInt(100)).Float64()
		}
		inPos := 0.0
		if sym.Position != nil {
			inPos = 1
		}
		out = append(out,
			metrics.Metric{Name: "carrybot_equity_usdt", Help: "Virtual per-symbol equity, USDT", Type: "gauge", Value: sym.Equity.InexactFloat64(), Labels: map[string]string{"symbol": s}},
			metrics.Metric{Name: "carrybot_drawdown_pct", Help: "Drawdown from peak equity, %", Type: "gauge", Value: ddPct, Labels: map[string]string{"symbol": s}},
			metrics.Metric{Name: "carrybot_in_position", Help: "1 if a virtual carry position is open", Type: "gauge", Value: inPos, Labels: map[string]string{"symbol": s}},
		)
	}
	return out
}

func parseSymbols(csv string) []string {
	parts := strings.Split(csv, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.ToUpper(strings.TrimSpace(p))
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
