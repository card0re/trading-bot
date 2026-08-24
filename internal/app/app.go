// Package app собирает клиента биржи, риск-менеджер и по одному
// исполнителю+стратегии на каждый торгуемый символ, и гоняет их до отмены
// контекста. cmd/bot/main.go остаётся тонкой обвязкой (сигналы, конфиг).
package app

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"trading-bot/internal/config"
	"trading-bot/internal/domain"
	exchange "trading-bot/internal/exchange/binance"
	"trading-bot/internal/metrics"
	"trading-bot/internal/notify"
	"trading-bot/internal/risk"
	"trading-bot/internal/rotation"
	"trading-bot/internal/strategy"

	"github.com/adshao/go-binance/v2/futures"
	"github.com/shopspring/decimal"
)

// Runner держит состояние работающего бота: общего клиента биржи, общий
// риск-менеджер и по одной паре Executor+Breakout на каждый символ.
type Runner struct {
	cfg       *config.AppConfig
	client    *futures.Client
	bots      map[string]*strategy.Breakout
	equitySrc domain.EquitySource
	notifier  domain.Notifier // может быть nil — тогда /status в Telegram не слушаем
}

// New приводит аккаунт и все символы в рабочее состояние: пингует биржу,
// проверяет one-way режим, выставляет плечо/маржу по каждому символу,
// прогревает историю и восстанавливает состояние стратегии. Символы
// настраиваются последовательно, не параллельно: несколько REST-вызовов на
// старте не создают заметной задержки, а последовательный код проще читать
// и не требует агрегации ошибок из горутин.
func New(ctx context.Context, cfg *config.AppConfig) (*Runner, error) {
	client, err := exchange.InitClient(ctx, cfg)
	if err != nil {
		return nil, err
	}
	slog.Info("✅ REST API подключен")

	infos, err := exchange.LoadAllSymbolInfo(ctx, client, cfg.Symbols)
	if err != nil {
		return nil, err
	}

	equitySrc := exchange.NewAccountEquitySource(client)
	riskMgr := risk.NewManager(cfg.PortfolioRiskCapPct, cfg.DailyLossLimitPct, cfg.MaxDrawdownPct)

	var notifier domain.Notifier
	if cfg.TelegramBotToken != "" && cfg.TelegramChatID != "" {
		notifier = notify.NewTelegram(cfg.TelegramBotToken, cfg.TelegramChatID)
		notifier.Notify(ctx, fmt.Sprintf("🚀 Бот запущен | символы: %v", cfg.Symbols))
	}

	// Индикаторам нужно набрать самый длинный из настроенных периодов,
	// прежде чем Ready() станет true.
	warmupBars := maxInt(cfg.LookbackBars, cfg.TrendEMAPeriod, cfg.ATRPeriod, cfg.VolumeAvgPeriod)

	bots := make(map[string]*strategy.Breakout, len(cfg.Symbols))
	for _, symbol := range cfg.Symbols {
		if err := exchange.SetupSymbol(ctx, client, symbol, cfg.Leverage, cfg.MarginType); err != nil {
			return nil, err
		}

		info := infos[symbol]
		slog.Info(fmt.Sprintf("📐 %s: шаг цены %s, шаг объёма %s, мин. номинал %s",
			info.Symbol, info.TickSize, info.StepSize, info.MinNotional))

		executor := exchange.NewOrderExecutor(client, info)
		fundingSrc := exchange.NewLiveFundingRateSource(client, symbol)
		bot := strategy.NewBreakout(executor, equitySrc, riskMgr, fundingSrc, strategy.Params{
			LookbackBars:         cfg.LookbackBars,
			BreakoutPct:          cfg.BreakoutPct,
			CooldownBars:         cfg.CooldownBars,
			TrendEMAPeriod:       cfg.TrendEMAPeriod,
			ATRPeriod:            cfg.ATRPeriod,
			VolumeAvgPeriod:      cfg.VolumeAvgPeriod,
			VolumeMultiplier:     cfg.VolumeMultiplier,
			ATRStopMultiplier:    cfg.ATRStopMultiplier,
			RiskRewardRatio:      cfg.RiskRewardRatio,
			RiskPerTradePct:      cfg.RiskPerTradePct,
			ADXPeriod:            cfg.ADXPeriod,
			TrendStrengthMinADX:  cfg.TrendStrengthMinADX,
			BreakevenTriggerR:    cfg.BreakevenTriggerR,
			MeanRevATRMultiplier: cfg.MeanRevATRMultiplier,
			FundingCarryMinRate:  cfg.FundingCarryMinRate,
			VolTargetPeriod:      cfg.VolTargetPeriod,
			OrderFlowMinRatio:    cfg.OrderFlowMinRatio,
		})
		if notifier != nil {
			bot.SetNotifier(notifier)
		}

		history, err := exchange.LoadRecentCandles(ctx, client, symbol, cfg.Interval, warmupBars)
		if err != nil {
			return nil, fmt.Errorf("прогрев истории %s: %w", symbol, err)
		}
		bot.Warmup(history)

		if err := bot.Recover(ctx); err != nil {
			return nil, fmt.Errorf("восстановление состояния %s: %w", symbol, err)
		}

		bots[symbol] = bot
	}

	return &Runner{cfg: cfg, client: client, bots: bots, equitySrc: equitySrc, notifier: notifier}, nil
}

// statusText собирает сводку по каждому символу и текущему эквити — ответ
// на команду /status в Telegram. Всегда свежий (не кэш): эквити и позиции
// запрашиваются заново на каждый вызов.
func (r *Runner) statusText(ctx context.Context) string {
	var b strings.Builder
	b.WriteString("📟 Статус бота\n")
	for _, symbol := range r.cfg.Symbols {
		if bot, ok := r.bots[symbol]; ok {
			b.WriteString("• " + bot.Status() + "\n")
		}
	}
	if equity, err := r.equitySrc.Equity(ctx); err == nil {
		fmt.Fprintf(&b, "Эквити счёта: %s USDT\n", equity.StringFixed(2))
	} else {
		fmt.Fprintf(&b, "Эквити счёта: не удалось получить (%v)\n", err)
	}

	if r.cfg.RotationStatePath != "" {
		b.WriteString(rotationStatusText(r.cfg.RotationStatePath))
	}
	return b.String()
}

// rotationStatusText читает файл состояния cmd/rotationbot (internal/rotation)
// и форматирует его для того же /status — так одной командой видно оба бота,
// а не только направленный. Отсутствие файла — не ошибка (ротация ещё не
// сделала первый ребаланс или процесс не запущен) — раздел просто это скажет.
func rotationStatusText(path string) string {
	// Стартовый эквити здесь не важен: если файла нет, ниже сразу видно
	// текст "нет данных", а не подставится тихо какое-то число.
	st, err := rotation.LoadState(path, decimal.Zero)
	if err != nil {
		return fmt.Sprintf("\n🔄 Ротация: не удалось прочитать состояние (%v)\n", err)
	}
	if st.LastRebalance.IsZero() {
		return "\n🔄 Ротация: ещё не было ни одного ребаланса\n"
	}

	var b strings.Builder
	b.WriteString("\n🔄 Ротация (виртуальный портфель)\n")
	if len(st.Positions) == 0 {
		b.WriteString("• позиций нет\n")
	}
	for symbol, pos := range st.Positions {
		side := "ЛОНГ"
		if pos.Qty.IsNegative() {
			side = "ШОРТ"
		}
		fmt.Fprintf(&b, "• %s %s %s @ %s\n", symbol, side, pos.Qty.Abs().StringFixed(4), pos.EntryPrice.StringFixed(4))
	}
	ddPct := decimal.Zero
	if st.PeakEquity.IsPositive() {
		ddPct = st.PeakEquity.Sub(st.Equity).Div(st.PeakEquity).Mul(decimal.NewFromInt(100))
	}
	fmt.Fprintf(&b, "Эквити: %s USDT (просадка от пика %s%%) | последний ребаланс %s",
		st.Equity.StringFixed(2), ddPct.StringFixed(2), st.LastRebalance.Format("2006-01-02 15:04 UTC"))
	return b.String()
}

// MetricsHandler отдаёт /metrics в формате Prometheus (см. internal/metrics).
// Собирает значения заново на каждый запрос — по одному REST-вызову эквити
// плюс локальные значения по каждому символу, не кэш.
func (r *Runner) MetricsHandler() http.Handler {
	return metrics.Handler(r.collectMetrics)
}

func (r *Runner) collectMetrics(ctx context.Context) []metrics.Metric {
	out := make([]metrics.Metric, 0, len(r.bots)+1)
	if equity, err := r.equitySrc.Equity(ctx); err == nil {
		out = append(out, metrics.Metric{
			Name: "tradingbot_equity_usdt", Help: "Account equity, USDT", Type: "gauge",
			Value: equity.InexactFloat64(),
		})
	}
	for symbol, bot := range r.bots {
		out = append(out, metrics.Metric{
			Name: "tradingbot_position_state", Help: "0=idle 1=opening 2=in_position", Type: "gauge",
			Value: positionStateValue(bot.State()), Labels: map[string]string{"symbol": symbol},
		})
	}
	return out
}

func positionStateValue(state string) float64 {
	switch state {
	case "OPENING":
		return 1
	case "IN_POSITION":
		return 2
	default:
		return 0
	}
}

// Run запускает потоки биржи и обрабатывает события до отмены ctx.
func (r *Runner) Run(ctx context.Context) error {
	// Буферы масштабируются числом символов — один общий канал на все
	// потоки свечей проще, чем фан-ин из N отдельных каналов, и не требует
	// перестройки select-цикла при изменении списка символов.
	candles := make(chan domain.Candle, 256*len(r.cfg.Symbols))
	events := make(chan domain.OrderEvent, 256*len(r.cfg.Symbols))

	// Приватный поток общий на весь аккаунт (один listenKey, не по одному на
	// символ) — поднимаем первым, чтобы не пропустить событие по ордеру,
	// выставленному сразу после старта.
	userDataDone := exchange.StreamUserData(ctx, r.client, r.cfg.KeepaliveInterval, events)

	klinesDone := make([]<-chan struct{}, 0, len(r.cfg.Symbols))
	for _, symbol := range r.cfg.Symbols {
		klinesDone = append(klinesDone, exchange.StreamKlines(ctx, symbol, r.cfg.Interval, candles))
	}

	reconcile := time.NewTicker(r.cfg.ReconcileInterval)
	defer reconcile.Stop()

	// /status и /help по запросу в Telegram — независимо от основного
	// цикла, отдельная горутина: подвисший long-poll не должен задерживать
	// обработку свечей/ордеров.
	if tg, ok := r.notifier.(*notify.Telegram); ok {
		go tg.ListenCommands(ctx, r.statusText)
	}

	if r.cfg.MetricsAddr != "" {
		srv := &http.Server{Addr: r.cfg.MetricsAddr, Handler: r.MetricsHandler()}
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
		slog.Info(fmt.Sprintf("📊 Метрики на http://%s/metrics", r.cfg.MetricsAddr))
	}

	slog.Info(fmt.Sprintf("✅ Бот в работе | %d символ(ов) %v | %s | окно %d свечей",
		len(r.cfg.Symbols), r.cfg.Symbols, r.cfg.Interval, r.cfg.LookbackBars))

	for {
		select {
		case c := <-candles:
			if bot, ok := r.bots[c.Symbol]; ok {
				bot.OnCandle(ctx, c)
			}

		case ev := <-events:
			if bot, ok := r.bots[ev.Symbol]; ok {
				bot.OnOrderEvent(ctx, ev)
			}

		case <-reconcile.C:
			// Страховка от потерянных событий: пока приватный поток был в
			// обрыве, стоп мог сработать, и без сверки бот навсегда остался
			// бы «в позиции» по этому символу.
			for _, bot := range r.bots {
				bot.Reconcile(ctx)
			}

		case <-ctx.Done():
			slog.Info("🛑 Получен сигнал остановки...")

			// Контекст уже отменён — для завершающих запросов нужен свежий.
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			var wg sync.WaitGroup
			for _, bot := range r.bots {
				wg.Add(1)
				go func(b *strategy.Breakout) {
					defer wg.Done()
					b.Shutdown(shutdownCtx)
				}(bot)
			}
			wg.Wait()
			cancel()

			waitClosed(append(klinesDone, userDataDone)...)
			return nil
		}
	}
}

// waitClosed ждёт фактического завершения потоков, но не дольше 10 секунд,
// чтобы зависший сокет не заблокировал выход навсегда.
func waitClosed(chans ...<-chan struct{}) {
	deadline := time.After(10 * time.Second)
	for _, ch := range chans {
		select {
		case <-ch:
		case <-deadline:
			slog.Warn("⚠️  Поток не завершился вовремя, выхожу")
			return
		}
	}
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
