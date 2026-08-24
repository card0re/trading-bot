package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
	"github.com/shopspring/decimal"
)

// AppConfig — вся настройка бота. Значения по умолчанию рассчитаны на testnet.
// Параметры стратегии и риска общие для всех символов из Symbols — бот
// анализирует и торгует каждый независимо, но по одинаковой логике.
type AppConfig struct {
	APIKey     string
	SecretKey  string
	UseTestnet bool

	Symbols  []string
	Interval string

	Leverage   int
	MarginType string // ISOLATED | CROSSED

	// Параметры стратегии.
	LookbackBars      int             // размер окна для поиска пробоя
	BreakoutPct       decimal.Decimal // насколько close должен превысить максимум окна, %
	CooldownBars      int             // сколько свечей не входить после выхода
	TrendEMAPeriod    int             // период EMA-фильтра тренда
	ATRPeriod         int             // период ATR (волатильность для стопа/сайзинга)
	VolumeAvgPeriod   int             // период скользящей средней объёма
	VolumeMultiplier  decimal.Decimal // объём пробойной свечи должен превышать среднюю в это число раз
	ATRStopMultiplier decimal.Decimal // расстояние до стопа = ATR * этот множитель
	RiskRewardRatio   decimal.Decimal // расстояние до тейка = расстояние до стопа * это отношение

	// ADXPeriod/TrendStrengthMinADX — фильтр силы тренда: не входить, если
	// ADX ниже порога (рынок в боковике). TrendStrengthMinADX=0 — выключен.
	ADXPeriod           int
	TrendStrengthMinADX decimal.Decimal
	// BreakevenTriggerR — перенос стопа в безубыток после этого профита в R.
	// 0 — выключено. См. internal/strategy.Params — при BreakevenTriggerR >=
	// RiskRewardRatio перенос почти никогда не успевает сработать раньше
	// тейка, так что практический смысл есть только при значении < RR.
	BreakevenTriggerR decimal.Decimal

	// MeanRevATRMultiplier — доп. вход "на возврат к среднему", работающий
	// только когда ADX ниже TrendStrengthMinADX (боковик). 0 — выключено.
	// См. internal/strategy.Params.
	MeanRevATRMultiplier decimal.Decimal

	// FundingCarryMinRate — доп. вход "на funding carry", независимый от
	// тренда/боковика. 0 — выключено. См. internal/strategy.Params.
	FundingCarryMinRate decimal.Decimal

	// VolTargetPeriod — таргетирование волатильности: уменьшает риск на
	// сделку, если ATR выше своего среднего за этот период. 0 — выключено.
	// См. internal/strategy.Params.
	VolTargetPeriod int

	// OrderFlowMinRatio — подтверждение пробоя дисбалансом потока ордеров
	// (доля объёма от агрессивных покупателей). 0 — выключено. См.
	// internal/strategy.Params.
	OrderFlowMinRatio decimal.Decimal

	// Риск-менеджмент — общий на все символы сразу (internal/risk.Manager).
	RiskPerTradePct     decimal.Decimal // риск на одну сделку, % от эквити
	PortfolioRiskCapPct decimal.Decimal // максимум суммарного открытого риска по всем символам, % от эквити
	DailyLossLimitPct   decimal.Decimal // дневной лимит убытка, % от эквити — новые входы блокируются при превышении
	// MaxDrawdownPct — максимальная просадка от исторического пика эквити,
	// % — новые входы блокируются, пока не отыграется. В отличие от
	// DailyLossLimitPct не сбрасывается каждые сутки: ловит растянутую во
	// времени серию убыточных дней, которую дневной лимит по одиночке не
	// видит (см. cmd/backtest -mode full: реальная просадка портфеля на
	// общем эквити была глубже, чем по каждому символу отдельно). 0 — выключено.
	MaxDrawdownPct decimal.Decimal

	// Служебные интервалы.
	ReconcileInterval time.Duration // сверка состояния с биржей
	KeepaliveInterval time.Duration // продление listenKey

	// Уведомления в Telegram о сделках и критических ошибках. Пусто —
	// уведомления отключены, бот работает как раньше, только в лог.
	TelegramBotToken string
	TelegramChatID   string

	// RotationStatePath — путь к файлу состояния виртуального портфеля
	// cmd/rotationbot (см. internal/rotation), если он запущен на этой же
	// машине. Пусто — команда /status просто не покажет раздел про ротацию,
	// это не ошибка (ротация — отдельный необязательный процесс).
	RotationStatePath string

	// MetricsAddr — адрес (например ":9090"), на котором отдавать /metrics
	// в формате Prometheus. Пусто (по умолчанию) — эндпоинт не поднимается,
	// бот не слушает порт, пока явно не попросили.
	MetricsAddr string
}

// Load читает .env (если есть) и системные переменные, валидируя результат.
func Load() (*AppConfig, error) {
	if err := godotenv.Load(); err != nil && !os.IsNotExist(err) {
		// Отсутствие .env — норма в проде, где переменные приходят из окружения.
		fmt.Println("ℹ️  .env не найден, читаю системные переменные")
	}

	cfg := &AppConfig{
		UseTestnet: envBool("BINANCE_TESTNET", true),
		// Список расширен после добавления шортов в стратегию (24.08.2026,
		// см. internal/strategy — вход теперь симметричный: пробой вверх с
		// растущим трендом это лонг, пробой вниз с падающим трендом — шорт).
		// Прежний прогон был лонг-only и сузил список до BNB+ETH.
		//
		// ВАЖНО (24.08.2026, walk-forward на 3 годах истории, holdout —
		// последние 6 месяцев, разбитые на 3 независимых периода): более
		// ранний 90-дневный тест показывал плюс по всем 7 монетам — на
		// walk-forward это не подтвердилось, все проверенные комбинации
		// параметров, включая ADX-фильтр и перенос в безубыток ниже, дают
		// ОТРИЦАТЕЛЬНЫЙ результат на каждом из 3 последних ~2-месячных
		// периодов почти по всем монетам одновременно — похоже на смену
		// рыночного режима (боковик), а не на проблему конкретной монеты.
		// Список из 7 не сужен заново специально: сама стратегия сейчас не
		// показывает устойчивого эджа НИ на каком поднаборе монет за
		// последние полгода — сужение списка ничего бы не исправило.
		// На реальные деньги переводить рано; testnet продолжает торговать
		// для сбора данных, монторинг — ежедневная сводка в Telegram.
		Symbols: envStrList("SYMBOLS", []string{
			"BTCUSDT", "ETHUSDT", "BNBUSDT", "SOLUSDT", "XRPUSDT", "ADAUSDT", "LINKUSDT",
		}),
		Interval:   envStr("INTERVAL", "1h"),
		MarginType: envStr("MARGIN_TYPE", "ISOLATED"),

		ReconcileInterval: 60 * time.Second,
		KeepaliveInterval: 30 * time.Minute,
	}

	// Ключи testnet и прода живут в разных переменных, чтобы их нельзя было
	// перепутать и случайно отправить боевой ордер.
	if cfg.UseTestnet {
		cfg.APIKey = os.Getenv("BINANCE_TESTNET_API_KEY")
		cfg.SecretKey = os.Getenv("BINANCE_TESTNET_SECRET_KEY")
	} else {
		cfg.APIKey = os.Getenv("BINANCE_API_KEY")
		cfg.SecretKey = os.Getenv("BINANCE_SECRET_KEY")
	}
	if cfg.APIKey == "" || cfg.SecretKey == "" {
		return nil, fmt.Errorf("не заданы ключи API (testnet=%v)", cfg.UseTestnet)
	}

	// setInt/setDecimal — первая ошибка парсинга побеждает и дальше просто
	// ничего не делает (err уже не nil), поэтому ниже можно писать один
	// вызов на переменную вместо if/err на каждую — Load читал одинаковый
	// трёхстрочный блок ~20 раз подряд.
	var err error
	setInt := func(dst *int, key string, def int) {
		if err != nil {
			return
		}
		*dst, err = envInt(key, def)
	}
	setDecimal := func(dst *decimal.Decimal, key, def string) {
		if err != nil {
			return
		}
		*dst, err = envDecimal(key, def)
	}

	setInt(&cfg.Leverage, "LEVERAGE", 3)
	// Значения ниже — победитель walk-forward подбора параметров (7 монет
	// выше, 3 года истории, 1h, holdout — последние 6 месяцев в 3 отдельных
	// периодах, честные комиссии + проскальзывание). MinADX, FundingCarry и
	// VolTarget — единственные три добавки, которые реально улучшили
	// walk-forward score в честном переборе (25.08.2026): FundingCarryMinRate
	// вывел лучший результат с -5.86 до -5.11 (все топ-10 walk-forward
	// выбрали carry включённым — не совпадение), VolTargetPeriod добавил ещё
	// немного (-5.11 → -4.79) и заметно сузил просадки в бэктесте (были
	// 4-8%+ на сделку в периоде, стали 1-6%). MeanRevATRMultiplier и
	// BreakevenTriggerR проверялись отдельно и НЕ дали улучшения ни разу —
	// оставлены выключенными. Итог всё ещё отрицательный на последних 6
	// месяцах (см. предупреждение у Symbols) — это лучшее из проверенного,
	// не гарантия.
	setInt(&cfg.LookbackBars, "LOOKBACK_BARS", 30)
	setInt(&cfg.CooldownBars, "COOLDOWN_BARS", 3)
	setInt(&cfg.TrendEMAPeriod, "TREND_EMA_PERIOD", 100)
	setInt(&cfg.ATRPeriod, "ATR_PERIOD", 14)
	setInt(&cfg.VolumeAvgPeriod, "VOLUME_AVG_PERIOD", 20)
	setInt(&cfg.ADXPeriod, "ADX_PERIOD", 14)

	setDecimal(&cfg.BreakoutPct, "BREAKOUT_PCT", "0.05")
	setDecimal(&cfg.VolumeMultiplier, "VOLUME_MULTIPLIER", "2.0")
	setDecimal(&cfg.ATRStopMultiplier, "ATR_STOP_MULTIPLIER", "3.0")
	setDecimal(&cfg.RiskRewardRatio, "RISK_REWARD_RATIO", "1.0")
	setDecimal(&cfg.TrendStrengthMinADX, "TREND_STRENGTH_MIN_ADX", "25")
	setDecimal(&cfg.BreakevenTriggerR, "BREAKEVEN_TRIGGER_R", "0")
	setDecimal(&cfg.MeanRevATRMultiplier, "MEAN_REV_ATR_MULTIPLIER", "0")
	setDecimal(&cfg.FundingCarryMinRate, "FUNDING_CARRY_MIN_RATE", "0.001")
	setInt(&cfg.VolTargetPeriod, "VOL_TARGET_PERIOD", 50)
	// 0.6 — walk-forward победитель (25.08.2026, cmd/tune -mode orderflow, 3
	// года, holdout 6 месяцев): любой порог 0.55-0.7 дал ОДИНАКОВЫЙ набор
	// сделок (граница потока ордеров между "да" и "нет" резкая, не размытая
	// в этом диапазоне) — 0.6 взят серединой найденного плато, не краем.
	// Улучшил walk-forward score с -5.72 до -5.30 (всё ещё отрицательно —
	// не делает стратегию прибыльной сам по себе, но заметно и стабильно
	// лучше без фильтра, тот же характер эффекта, что у ADX/VolTarget).
	setDecimal(&cfg.OrderFlowMinRatio, "ORDER_FLOW_MIN_RATIO", "0.6")
	setDecimal(&cfg.RiskPerTradePct, "RISK_PER_TRADE_PCT", "1.0")
	setDecimal(&cfg.PortfolioRiskCapPct, "PORTFOLIO_RISK_CAP_PCT", "4.5")
	setDecimal(&cfg.DailyLossLimitPct, "DAILY_LOSS_LIMIT_PCT", "3.0")
	// 20% — калибровано по бэктесту (24.08.2026, cmd/backtest -mode full,
	// 3 года, 7 монет на общем эквити): реальная просадка портфеля целиком
	// доходила до 32%, заметно глубже, чем по каждому символу отдельно
	// (10-18%) — портфельный риск-кап и дневной лимит защищают от разных
	// вещей (см. комментарий у MaxDrawdownPct), ни один не поймал бы это.
	setDecimal(&cfg.MaxDrawdownPct, "MAX_DRAWDOWN_PCT", "20")
	if err != nil {
		return nil, err
	}

	cfg.TelegramBotToken = envStr("TELEGRAM_BOT_TOKEN", "")
	cfg.TelegramChatID = envStr("TELEGRAM_CHAT_ID", "")
	cfg.RotationStatePath = envStr("ROTATION_STATE_PATH", "")
	cfg.MetricsAddr = envStr("METRICS_ADDR", "")

	return cfg, cfg.validate()
}

func (c *AppConfig) validate() error {
	if len(c.Symbols) == 0 {
		return fmt.Errorf("SYMBOLS не может быть пустым")
	}
	seen := make(map[string]bool, len(c.Symbols))
	for _, s := range c.Symbols {
		if s == "" {
			return fmt.Errorf("SYMBOLS содержит пустое значение")
		}
		if seen[s] {
			return fmt.Errorf("SYMBOLS содержит повтор: %s", s)
		}
		seen[s] = true
	}
	if c.Leverage < 1 || c.Leverage > 125 {
		return fmt.Errorf("LEVERAGE вне диапазона 1..125: %d", c.Leverage)
	}
	if c.MarginType != "ISOLATED" && c.MarginType != "CROSSED" {
		return fmt.Errorf("MARGIN_TYPE должен быть ISOLATED или CROSSED, получено %q", c.MarginType)
	}
	if c.LookbackBars < 2 {
		return fmt.Errorf("LOOKBACK_BARS должен быть >= 2")
	}
	if c.CooldownBars < 0 {
		return fmt.Errorf("COOLDOWN_BARS не может быть отрицательным")
	}
	if c.TrendEMAPeriod < 1 {
		return fmt.Errorf("TREND_EMA_PERIOD должен быть >= 1")
	}
	if c.ATRPeriod < 1 {
		return fmt.Errorf("ATR_PERIOD должен быть >= 1")
	}
	if c.VolumeAvgPeriod < 1 {
		return fmt.Errorf("VOLUME_AVG_PERIOD должен быть >= 1")
	}
	if c.BreakoutPct.IsNegative() {
		return fmt.Errorf("BREAKOUT_PCT не может быть отрицательным")
	}
	if c.VolumeMultiplier.LessThanOrEqual(decimal.Zero) {
		return fmt.Errorf("VOLUME_MULTIPLIER должен быть > 0")
	}
	if c.ATRStopMultiplier.LessThanOrEqual(decimal.Zero) {
		return fmt.Errorf("ATR_STOP_MULTIPLIER должен быть > 0")
	}
	if c.RiskRewardRatio.LessThanOrEqual(decimal.Zero) {
		return fmt.Errorf("RISK_REWARD_RATIO должен быть > 0")
	}
	if c.ADXPeriod < 1 {
		return fmt.Errorf("ADX_PERIOD должен быть >= 1")
	}
	if c.TrendStrengthMinADX.IsNegative() {
		return fmt.Errorf("TREND_STRENGTH_MIN_ADX не может быть отрицательным")
	}
	if c.BreakevenTriggerR.IsNegative() {
		return fmt.Errorf("BREAKEVEN_TRIGGER_R не может быть отрицательным")
	}
	if c.MeanRevATRMultiplier.IsNegative() {
		return fmt.Errorf("MEAN_REV_ATR_MULTIPLIER не может быть отрицательным")
	}
	if c.FundingCarryMinRate.IsNegative() {
		return fmt.Errorf("FUNDING_CARRY_MIN_RATE не может быть отрицательным")
	}
	if c.VolTargetPeriod < 0 {
		return fmt.Errorf("VOL_TARGET_PERIOD не может быть отрицательным")
	}
	if c.OrderFlowMinRatio.IsNegative() || c.OrderFlowMinRatio.GreaterThan(decimal.NewFromInt(1)) {
		return fmt.Errorf("ORDER_FLOW_MIN_RATIO должен быть в диапазоне 0..1 (0 — выключено)")
	}
	if c.RiskPerTradePct.LessThanOrEqual(decimal.Zero) {
		return fmt.Errorf("RISK_PER_TRADE_PCT должен быть > 0")
	}
	if c.PortfolioRiskCapPct.LessThan(c.RiskPerTradePct) {
		return fmt.Errorf(
			"PORTFOLIO_RISK_CAP_PCT (%s) меньше RISK_PER_TRADE_PCT (%s) — тогда ни одна сделка не пройдёт лимит портфеля",
			c.PortfolioRiskCapPct, c.RiskPerTradePct)
	}
	if c.DailyLossLimitPct.LessThanOrEqual(decimal.Zero) {
		return fmt.Errorf("DAILY_LOSS_LIMIT_PCT должен быть > 0")
	}
	if c.MaxDrawdownPct.IsNegative() {
		return fmt.Errorf("MAX_DRAWDOWN_PCT не может быть отрицательным")
	}
	return nil
}

func envStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envStrList разбирает список через запятую (например SYMBOLS=BTCUSDT,ETHUSDT).
// Пробелы вокруг элементов обрезаются, регистр приводится к верхнему —
// символы Binance всегда в верхнем регистре.
func envStrList(key string, def []string) []string {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.ToUpper(strings.TrimSpace(p))
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func envBool(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		fmt.Printf("⚠️  %s: не булево значение (%q), использую значение по умолчанию %v\n", key, v, def)
		return def
	}
	return b
}

func envInt(key string, def int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: не число (%q)", key, v)
	}
	return n, nil
}

func envDecimal(key, def string) (decimal.Decimal, error) {
	v := os.Getenv(key)
	if v == "" {
		v = def
	}
	d, err := decimal.NewFromString(v)
	if err != nil {
		return decimal.Zero, fmt.Errorf("%s: не число (%q)", key, v)
	}
	return d, nil
}
