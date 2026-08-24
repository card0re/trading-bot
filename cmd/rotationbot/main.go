// Команда rotationbot — живой (но виртуальный) forward-тест ротационной
// стратегии из cmd/rotation. НЕ отправляет реальных ордеров: если бы он
// торговал по-настоящему теми же 7 монетами, что и основной бот
// (internal/strategy.Breakout), оба писали бы в один и тот же position book
// одного testnet-аккаунта и конфликтовали бы друг с другом (позиция на
// символ в one-way режиме одна на аккаунт, а не одна на стратегию). Вместо
// этого ведёт отдельный виртуальный портфель, обновляемый по реальным ценам
// с боевой сети (публичные эндпоинты, ключ не нужен) — честный forward-тест
// без риска (даже testnet-рисков) и без конфликта с основным ботом.
//
// Параметры по умолчанию — лучшая по walk-forward комбинация на 7 монетах
// (24.08.2026, см. cmd/rotation): lookback=10д, topK=2, ребаланс раз в 7
// дней. Статистика тонкая (9 ребалансировок на 60-дневный период в
// бэктесте) — именно поэтому это второй, экспериментальный бот, а не замена
// основному.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"trading-bot/internal/domain"
	exchange "trading-bot/internal/exchange/binance"
	"trading-bot/internal/notify"
	"trading-bot/internal/rotation"

	"github.com/adshao/go-binance/v2"
	"github.com/adshao/go-binance/v2/futures"
	"github.com/joho/godotenv"
	"github.com/shopspring/decimal"
)

type params struct {
	symbols        []string
	lookbackDays   int
	topK           int
	rebalanceEvery int
	feeRate        decimal.Decimal
	slippagePct    decimal.Decimal
}

func main() {
	statePath := flag.String("state", "rotation_state.json", "путь к файлу состояния виртуального портфеля")
	symbolsFlag := flag.String("symbols", "BTCUSDT,ETHUSDT,BNBUSDT,SOLUSDT,XRPUSDT,ADAUSDT,LINKUSDT", "монеты через запятую")
	lookback := flag.Int("lookback", 10, "окно доходности для ранжирования, дней")
	topK := flag.Int("topk", 2, "сколько лонгов и сколько шортов одновременно")
	rebalanceDays := flag.Int("rebalance", 7, "раз в сколько дней пересобирать портфель")
	startEquityStr := flag.String("equity", "10000", "стартовый виртуальный эквити, USDT")
	checkEvery := flag.Duration("check-every", time.Hour, "как часто проверять, не пора ли ребалансировать")
	flag.Parse()

	if err := godotenv.Load(); err != nil && !os.IsNotExist(err) {
		fmt.Println("ℹ️  .env не найден, читаю системные переменные")
	}

	p := params{
		symbols:        parseSymbols(*symbolsFlag),
		lookbackDays:   *lookback,
		topK:           *topK,
		rebalanceEvery: *rebalanceDays,
		feeRate:        decimal.NewFromFloat(0.0005),
		slippagePct:    decimal.NewFromFloat(0.0002),
	}

	startEquity, err := decimal.NewFromString(*startEquityStr)
	if err != nil {
		log.Fatalf("❌ --equity: %v", err)
	}
	if p.topK < 1 {
		log.Fatal("❌ -topk должен быть >= 1 (0 делит на ноль при расчёте размера ноги)")
	}

	st, err := rotation.LoadState(*statePath, startEquity)
	if err != nil {
		log.Fatalf("❌ чтение состояния: %v", err)
	}

	var notifier domain.Notifier
	if token, chat := os.Getenv("TELEGRAM_BOT_TOKEN"), os.Getenv("TELEGRAM_CHAT_ID"); token != "" && chat != "" {
		notifier = notify.NewTelegram(token, chat)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	futures.UseTestnet = false // цены с боевой сети — портфель виртуальный, реальных ордеров нет
	client := binance.NewFuturesClient("", "")

	log.Printf("🚀 Rotation shadow-bot | %v | lookback=%dд topK=%d rebalance=%dд | эквити %s",
		p.symbols, p.lookbackDays, p.topK, p.rebalanceEvery, st.Equity.StringFixed(2))
	if notifier != nil {
		notifier.Notify(ctx, fmt.Sprintf("🔄 [РОТАЦИЯ] Бот запущен (виртуальный портфель, без реальных ордеров)\nЭквити: %s USDT", st.Equity.StringFixed(2)))
	}

	check := func() {
		due := st.LastRebalance.IsZero() || time.Since(st.LastRebalance) >= time.Duration(p.rebalanceEvery)*24*time.Hour
		if !due {
			return
		}
		if err := rebalance(ctx, client, &st, p, notifier); err != nil {
			log.Printf("⚠️  Ребаланс не удался: %v", err)
			// Сохраняем состояние даже при ошибке: rebalance мог успеть
			// закрыть старые позиции (мутировать st через указатель) до
			// того, как споткнулся дальше (например, эквити после закрытия
			// ушёл в 0 и открывать новые позиции не стали) — если это не
			// сохранить, следующая попытка перечитает старое состояние с
			// уже не открытыми на бирже позициями и попробует закрыть их
			// заново по другой цене.
		}
		if err := rotation.SaveState(*statePath, st); err != nil {
			log.Printf("⚠️  Не удалось сохранить состояние: %v", err)
		}
	}

	check() // сразу при старте — если это первый запуск или пропущенный срок

	ticker := time.NewTicker(*checkEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			check()
		case <-ctx.Done():
			log.Println("🛑 Остановка (виртуальные позиции сохранены как есть)")
			return
		}
	}
}

// rebalance закрывает текущие виртуальные позиции по свежим ценам, ранжирует
// монеты по доходности за lookbackDays и открывает новый равновзвешенный
// рыночно-нейтральный портфель — логика идентична simulate() в cmd/rotation,
// только на живых данных вместо исторических.
func rebalance(ctx context.Context, client *futures.Client, st *rotation.State, p params, notifier domain.Notifier) error {
	end := time.Now()
	start := end.AddDate(0, 0, -(p.lookbackDays + 5))

	type quote struct {
		symbol string
		ret    decimal.Decimal
		price  decimal.Decimal
	}
	prices := make(map[string]decimal.Decimal, len(p.symbols))
	var quotes []quote
	for _, s := range p.symbols {
		candles, err := exchange.LoadHistoricalCandles(ctx, client, s, "1d", start, end)
		if err != nil || len(candles) < p.lookbackDays+1 {
			log.Printf("⚠️  %s: недостаточно данных для ребаланса (%v)", s, err)
			continue
		}
		last := candles[len(candles)-1].Close
		base := candles[len(candles)-1-p.lookbackDays].Close
		prices[s] = last
		if base.IsZero() {
			continue
		}
		quotes = append(quotes, quote{symbol: s, ret: last.Sub(base).Div(base), price: last})
	}
	if len(quotes) < 2*p.topK {
		return fmt.Errorf("данных достаточно только по %d монетам, нужно минимум %d", len(quotes), 2*p.topK)
	}

	// 1. Закрываем всё, что было открыто с прошлого ребаланса.
	var closedLines []string
	for symbol, pos := range st.Positions {
		if pos.Qty.IsZero() {
			continue
		}
		price, ok := prices[symbol]
		if !ok {
			price = pos.EntryPrice // не удалось получить свежую цену — закрываем по цене входа (PnL=0), это лучше, чем упасть
		}
		execPrice := price
		if pos.Qty.IsPositive() {
			execPrice = price.Mul(decimal.NewFromInt(1).Sub(p.slippagePct))
		} else {
			execPrice = price.Mul(decimal.NewFromInt(1).Add(p.slippagePct))
		}
		fee := pos.Qty.Abs().Mul(execPrice).Mul(p.feeRate)
		pnl := pos.Qty.Mul(execPrice.Sub(pos.EntryPrice)).Sub(fee)
		st.Equity = st.Equity.Add(pnl)
		closedLines = append(closedLines, fmt.Sprintf("%s: %s USDT", symbol, pnl.StringFixed(2)))
	}
	st.Positions = make(map[string]rotation.Position)

	if !st.Equity.IsPositive() {
		return fmt.Errorf("виртуальный эквити обнулился (%s) — портфель не открываю", st.Equity.StringFixed(2))
	}

	sort.Slice(quotes, func(i, j int) bool { return quotes[i].ret.GreaterThan(quotes[j].ret) })
	k := p.topK
	longs := quotes[:k]
	shorts := quotes[len(quotes)-k:]

	notionalPerLeg := st.Equity.Div(decimal.NewFromInt(int64(2 * k)))
	var openedLines []string
	open := func(q quote, long bool) {
		var execPrice, qty decimal.Decimal
		if long {
			execPrice = q.price.Mul(decimal.NewFromInt(1).Add(p.slippagePct))
			qty = notionalPerLeg.Div(execPrice)
		} else {
			execPrice = q.price.Mul(decimal.NewFromInt(1).Sub(p.slippagePct))
			qty = notionalPerLeg.Div(execPrice).Neg()
		}
		fee := qty.Abs().Mul(execPrice).Mul(p.feeRate)
		st.Equity = st.Equity.Sub(fee)
		st.Positions[q.symbol] = rotation.Position{Qty: qty, EntryPrice: execPrice}
		side := "ШОРТ"
		if long {
			side = "ЛОНГ"
		}
		openedLines = append(openedLines, fmt.Sprintf("%s %s (%s%%)", q.symbol, side, q.ret.Mul(decimal.NewFromInt(100)).StringFixed(1)))
	}
	for _, l := range longs {
		open(l, true)
	}
	for _, sh := range shorts {
		open(sh, false)
	}

	if st.Equity.GreaterThan(st.PeakEquity) {
		st.PeakEquity = st.Equity
	}
	drawdownPct := decimal.Zero
	if st.PeakEquity.IsPositive() {
		drawdownPct = st.PeakEquity.Sub(st.Equity).Div(st.PeakEquity).Mul(decimal.NewFromInt(100))
	}
	st.LastRebalance = time.Now()

	log.Printf("🔄 Ребаланс | закрыто: %s | открыто: %s | эквити %s | просадка от пика %s%%",
		strings.Join(closedLines, ", "), strings.Join(openedLines, ", "), st.Equity.StringFixed(2), drawdownPct.StringFixed(2))

	if notifier != nil {
		msg := "🔄 [РОТАЦИЯ] Ребаланс\n"
		if len(closedLines) > 0 {
			msg += "Закрыто: " + strings.Join(closedLines, " | ") + "\n"
		}
		msg += "Открыто: " + strings.Join(openedLines, " | ") + "\n"
		msg += fmt.Sprintf("Эквити: %s USDT (просадка от пика %s%%)", st.Equity.StringFixed(2), drawdownPct.StringFixed(2))
		notifier.Notify(ctx, msg)
	}
	return nil
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
