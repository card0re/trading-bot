// Команда backtest прогоняет реальную стратегию (internal/strategy) на
// исторических данных без сети/сделок — быстрая проверка перед paper-тестом
// на testnet. Всегда тянет данные с боевой сети Binance (публичные
// эндпоинты, ключ не нужен): на testnet мало и нерепрезентативно истории.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"sort"
	"strings"
	"time"

	"trading-bot/internal/backtest"
	"trading-bot/internal/domain"
	exchange "trading-bot/internal/exchange/binance"
	"trading-bot/internal/risk"
	"trading-bot/internal/strategy"

	"github.com/adshao/go-binance/v2"
	"github.com/adshao/go-binance/v2/futures"
	"github.com/shopspring/decimal"
)

func main() {
	symbolsFlag := flag.String("symbols", "BTCUSDT,ETHUSDT,BNBUSDT,SOLUSDT,XRPUSDT,ADAUSDT,LINKUSDT", "символы через запятую")
	interval := flag.String("interval", "5m", "таймфрейм свечей")
	days := flag.Int("days", 180, "глубина истории, дней")
	equityFlag := flag.String("equity", "10000", "стартовый эквити на символ, USDT")
	riskPct := flag.String("risk-pct", "1.0", "риск на сделку, % от текущего эквити")
	mode := flag.String("mode", "strategy", `"strategy" — без портфельных лимитов, "full" — с общим risk.Manager`)
	flag.Parse()

	if err := run(*symbolsFlag, *interval, *days, *equityFlag, *riskPct, *mode); err != nil {
		log.Fatalf("❌ %v", err)
	}
}

func run(symbolsCSV, interval string, days int, equityStr, riskPctStr, mode string) error {
	if mode != "strategy" && mode != "full" {
		return fmt.Errorf("--mode должен быть \"strategy\" или \"full\", получено %q", mode)
	}

	symbols := parseSymbolList(symbolsCSV)
	startEquity, err := decimal.NewFromString(equityStr)
	if err != nil {
		return fmt.Errorf("--equity: %w", err)
	}
	riskPct, err := decimal.NewFromString(riskPctStr)
	if err != nil {
		return fmt.Errorf("--risk-pct: %w", err)
	}

	futures.UseTestnet = false
	client := binance.NewFuturesClient("", "") // публичные эндпоинты, ключ не нужен

	ctx := context.Background()
	infos, err := exchange.LoadAllSymbolInfo(ctx, client, symbols)
	if err != nil {
		return err
	}

	params := defaultParams()
	params.RiskPerTradePct = riskPct
	warmupBars := maxInt(params.LookbackBars, params.TrendEMAPeriod, params.ATRPeriod, params.VolumeAvgPeriod)

	end := time.Now()
	start := end.AddDate(0, 0, -days)

	var sharedRisk *risk.Manager
	if mode == "full" {
		sharedRisk = risk.NewManager(decimal.NewFromFloat(4.5), decimal.NewFromFloat(3.0), decimal.NewFromFloat(20))
	}

	fmt.Printf("📊 Бэктест | режим=%s | %s..%s | %s | символы: %v\n",
		mode, start.Format("2006-01-02"), end.Format("2006-01-02"), interval, symbols)

	// Стратегия логирует каждую закрытую свечу (нужно для живой торговли,
	// но за тысячи баров истории — просто шум) — глушим стандартный логгер
	// на время симуляции и печатаем только свой итог через fmt.
	log.SetOutput(io.Discard)

	// Сначала грузим историю и прогреваем всех — только потом проигрываем
	// свечи, чтобы в режиме "full" можно было пройтись по ним в едином
	// хронологическом порядке (см. ниже).
	runs := make([]*symbolRun, 0, len(symbols))
	for _, symbol := range symbols {
		info, ok := infos[symbol]
		if !ok {
			fmt.Printf("⚠️  %s: нет данных фильтров биржи, пропускаю\n", symbol)
			continue
		}

		history, err := exchange.LoadHistoricalCandles(ctx, client, symbol, interval, start, end)
		if err != nil {
			return fmt.Errorf("история %s: %w", symbol, err)
		}
		if len(history) <= warmupBars {
			fmt.Printf("⚠️  %s: маловато истории (%d свечей), пропускаю\n", symbol, len(history))
			continue
		}

		sim := backtest.NewSimExecutor(symbol, info, startEquity, backtest.TakerFeeRate, backtest.DefaultSlippagePct)

		var gate domain.RiskGate = backtest.AlwaysAllowRiskGate{}
		if mode == "full" {
			gate = sharedRisk
		}

		bot := strategy.NewBreakout(sim, sim, gate, nil, params)
		bot.Warmup(history[:warmupBars])

		runs = append(runs, &symbolRun{symbol: symbol, history: history[warmupBars:], sim: sim, bot: bot})
	}

	if mode == "full" {
		// Общий risk.Manager считает агрегированный риск и дневной лимит
		// корректно только если сделки по разным символам приходят в
		// РЕАЛЬНОМ хронологическом порядке — как в живом боте, где все
		// потоки свечей делят один канал. Последовательный проход по
		// символам одним за другим (сначала весь BTC, потом весь ETH...)
		// давал бы неверную картину: риск, занятый BTC, не освобождался бы
		// вовремя относительно сделок по ETH, и лимит на портфель считался
		// бы так, будто все сделки по BTC произошли раньше всех сделок по
		// остальным монетам — чего на самом деле не было.
		type tick struct {
			c   domain.Candle
			run *symbolRun
		}
		var timeline []tick
		for _, r := range runs {
			for _, c := range r.history {
				timeline = append(timeline, tick{c: c, run: r})
			}
		}
		sort.SliceStable(timeline, func(i, j int) bool {
			return timeline[i].c.CloseTime.Before(timeline[j].c.CloseTime)
		})

		for _, tk := range timeline {
			// risk.Manager по умолчанию берёт реальное время (нужно для
			// прода) — здесь подменяем его на время проигрываемой свечи,
			// иначе дневной лимит убытка засчитал бы весь бэктест как одни
			// сутки и намертво заблокировал бы входы после первого же
			// превышения лимита.
			simTime := tk.c.CloseTime
			sharedRisk.SetClock(func() time.Time { return simTime })

			if ev := tk.run.sim.OnCandle(tk.c); ev != nil {
				tk.run.bot.OnOrderEvent(ctx, *ev)
			}
			tk.run.bot.OnCandle(ctx, tk.c)
		}
	} else {
		for _, r := range runs {
			for _, c := range r.history {
				if ev := r.sim.OnCandle(c); ev != nil {
					r.bot.OnOrderEvent(ctx, *ev)
				}
				r.bot.OnCandle(ctx, c)
			}
		}
	}

	for _, r := range runs {
		stats := backtest.ComputeStats(r.sim.Trades(), startEquity)
		fmt.Printf("— %-8s | %d свечей | %s\n", r.symbol, len(r.history)+warmupBars, stats)
	}

	if mode == "full" {
		printPortfolioStats(runs, startEquity)
	}

	fmt.Println("ℹ️  Упрощения бэктеста: вход по close сигнальной свечи, плечо/ликвидация не " +
		"моделируются (комиссии и консервативное проскальзывание — учитываются, см. " +
		"backtest.TakerFeeRate/DefaultSlippagePct) — на реальном счёте результат обычно немного " +
		"хуже, особенно на резких стопах.")
	return nil
}

// symbolRun — прогон одного символа в бэктесте.
type symbolRun struct {
	symbol  string
	history []domain.Candle
	sim     *backtest.SimExecutor
	bot     *strategy.Breakout
}

// printPortfolioStats сводит сделки со всех символов в ОДНУ хронологическую
// последовательность на ОДНОМ общем эквити (не N независимых по $equity
// каждый, как в per-symbol выводе выше) — так видно просадку счёта целиком,
// если несколько коррелированных монет (а крипта вся коррелирована)
// одновременно словят стоп. Сайзинг позиций при этом всё равно считался от
// изолированного эквити каждого символа (SimExecutor.Equity — не общий), а
// не от этой сводной кривой — реальный размер позиций на просевшем общем
// счёте был бы чуть меньше, так что просадка здесь скорее консервативная
// оценка сверху, а не заниженная.
func printPortfolioStats(runs []*symbolRun, startEquity decimal.Decimal) {
	var all []backtest.Trade
	for _, r := range runs {
		all = append(all, r.sim.Trades()...)
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].ExitTime.Before(all[j].ExitTime) })

	stats := backtest.ComputeStats(all, startEquity)
	fmt.Printf("💼 ПОРТФЕЛЬ ЦЕЛИКОМ (все символы на одном общем эквити $%s) | %s\n", startEquity.StringFixed(0), stats)
}

// defaultParams — параметры стратегии по умолчанию (совпадают со значениями
// по умолчанию в internal/config, чтобы бэктест проверял то же самое, что
// будет торговать бот, если .env их не переопределяет).
// defaultParams — совпадает с боевыми умолчаниями internal/config (25.08.2026).
// ВАЖНО: FundingCarryMinRate здесь для документационной полноты, но в этом
// инструменте эффекта не даёт — run() ниже не подключает funding source
// (nil), в отличие от cmd/tune, который для честной проверки carry-входа
// использует FundingReplay. Для проверки carry-эффекта используйте cmd/tune.
func defaultParams() strategy.Params {
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
		FundingCarryMinRate: decimal.NewFromFloat(0.001),
		VolTargetPeriod:     50,
		OrderFlowMinRatio:   decimal.NewFromFloat(0.6),
	}
}

func parseSymbolList(csv string) []string {
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

func maxInt(vals ...int) int {
	m := vals[0]
	for _, v := range vals[1:] {
		if v > m {
			m = v
		}
	}
	return m
}
