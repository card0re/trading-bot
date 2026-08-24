// Package strategy содержит торговую логику. Он не знает про Binance —
// вся работа с биржей идёт через domain.Executor/EquitySource/RiskGate,
// поэтому стратегию можно тестировать на фейковых реализациях.
package strategy

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"trading-bot/internal/domain"
	"trading-bot/internal/indicator"

	"github.com/shopspring/decimal"
)

// state — состояние автомата. Промежуточный Opening нужен, чтобы событие из
// приватного потока не начало вторую сделку, пока летит первая. Закрытие
// позиции (стоп/тейк/ручной выход) всегда переводит автомат сразу из
// InPosition в Idle без отдельного промежуточного состояния — сама
// закрывающая сделка исполняется биржей асинхронно (ордер уже стоит), а не
// отправляется нашим кодом синхронно, так что ловить гонку тут не на чем.
type state int

const (
	stateIdle state = iota
	stateOpening
	stateInPosition
)

func (s state) String() string {
	switch s {
	case stateOpening:
		return "OPENING"
	case stateInPosition:
		return "IN_POSITION"
	default:
		return "IDLE"
	}
}

// posSide — направление позиции. Пробой торгуется в обе стороны: пробой
// вверх с подтверждением восходящим трендом — лонг, пробой вниз с
// подтверждением нисходящим трендом — симметрично шорт. Это не отдельная
// стратегия, а тот же сигнал, отражённый по цене — один растущий, один
// падающий рынок бот использует одинаково.
type posSide int

const (
	sideLong posSide = iota
	sideShort
)

func (s posSide) String() string {
	if s == sideShort {
		return "ШОРТ"
	}
	return "ЛОНГ"
}

// Params — настройки стратегии: пробой, подтверждённый трендом и объёмом,
// с ATR-адаптивными стопом/тейком и риск-ориентированным размером позиции.
type Params struct {
	LookbackBars int             // размер окна для поиска пробоя
	BreakoutPct  decimal.Decimal // насколько close должен превысить максимум окна, %
	CooldownBars int             // сколько свечей не входить после выхода

	TrendEMAPeriod   int             // период EMA-фильтра тренда (вход только выше неё)
	ATRPeriod        int             // период ATR (волатильность для стопа/сайзинга)
	VolumeAvgPeriod  int             // период скользящей средней объёма
	VolumeMultiplier decimal.Decimal // объём пробойной свечи должен превышать среднюю в это число раз

	ATRStopMultiplier decimal.Decimal // расстояние до стопа = ATR * этот множитель
	RiskRewardRatio   decimal.Decimal // расстояние до тейка = расстояние до стопа * это отношение

	RiskPerTradePct decimal.Decimal // риск на одну сделку, % от эквити счёта

	// ADXPeriod — период индикатора силы тренда ADX.
	ADXPeriod int
	// TrendStrengthMinADX — не входить, если ADX ниже этого порога (рынок в
	// боковике, пробой скорее ложный). 0 — фильтр выключен.
	TrendStrengthMinADX decimal.Decimal

	// BreakevenTriggerR — после того как незафиксированная прибыль достигла
	// этого множителя от риска сделки (R), стоп переносится на цену входа
	// (один раз за сделку) — риск на сделке становится нулевым, а тейк
	// остаётся как был. 0 — выключено (стоп/тейк ставятся один раз и не
	// трогаются, как раньше).
	BreakevenTriggerR decimal.Decimal

	// MeanRevATRMultiplier — дополнительный вход "на возврат к среднему",
	// работающий ТОЛЬКО когда ADX ниже TrendStrengthMinADX (рынок в
	// боковике — именно там, где обычный пробойный вход выключен фильтром
	// силы тренда). Лонг — когда close ушёл ниже EMA-тренда более чем на
	// MeanRevATRMultiplier*ATR (перепродан, ставка на отскок вверх к
	// средней); шорт — симметрично выше средней. Использует тот же
	// ATR-стоп/тейк и риск-сайзинг, что и пробойный вход — это не отдельная
	// стратегия по инфраструктуре, только другое условие входа. 0 —
	// выключено. Требует TrendStrengthMinADX > 0 (иначе "боковик" не
	// определён и функция неактивна независимо от этого значения).
	MeanRevATRMultiplier decimal.Decimal

	// FundingCarryMinRate — дополнительный вход "на funding carry": лонг,
	// когда funding rate меньше -FundingCarryMinRate (шорты платят лонгам),
	// шорт — когда funding rate больше +FundingCarryMinRate (лонги платят
	// шортам). Независим от тренда/боковика (в отличие от MeanRev), но
	// проверяется в switch последним — уступает пробою и возврату к
	// среднему, если те тоже дали сигнал на этой свече. Требует не-nil
	// FundingRateSource в NewBreakout — иначе неактивен независимо от
	// значения. 0 — выключено.
	FundingCarryMinRate decimal.Decimal

	// VolTargetPeriod — таргетирование волатильности: если ATR в момент
	// входа выше своего среднего за VolTargetPeriod баров, риск на сделку
	// пропорционально уменьшается (никогда не увеличивается сверх обычного
	// RiskPerTradePct). ATR-стоп и так уравнивает ДОЛЛАРОВЫЙ риск на сделку
	// между спокойными и волатильными периодами — это про другое: во время
	// вспышки волатильности текущий ATR может быть временно завышен/шумным
	// относительно его же обычного уровня, и это отдельно снижает размер
	// позиции, а не просто раздвигает стоп. 0 — выключено.
	VolTargetPeriod int
}

// Breakout входит в лонг по пробою максимума последних LookbackBars свечей,
// подтверждённому направлением тренда (EMA) и всплеском объёма — это отсекает
// большинство ложных пробоев, которые ловила бы «голая» проверка максимума.
// Стоп и тейк считаются от текущей волатильности (ATR), а не от фиксированного
// процента, поэтому автоматически адаптируются под каждый символ. Размер
// позиции считается от риска в процентах эквити счёта, поэтому адаптируется
// под любой баланс.
type Breakout struct {
	exec    domain.Executor
	equity  domain.EquitySource
	risk    domain.RiskGate
	funding domain.FundingRateSource // может быть nil — тогда фильтр по funding rate отключён
	notify  domain.Notifier          // может быть nil — тогда уведомления не отправляются
	params  Params

	mu           sync.Mutex
	st           state
	highs        []decimal.Decimal // кольцевой буфер максимумов закрытых свечей (уровень пробоя вверх, для лонга)
	lows         []decimal.Decimal // кольцевой буфер минимумов закрытых свечей (уровень пробоя вниз, для шорта)
	cooldownLeft int

	trendEMA *indicator.EMA
	atr      *indicator.ATR
	adx      *indicator.ADX
	volAvg   *indicator.RollingAverage
	atrAvg   *indicator.RollingAverage // средний ATR за VolTargetPeriod — база для таргетирования волатильности

	// Данные последней открытой сделки — нужны при закрытии позиции, чтобы
	// освободить зарезервированный в RiskGate риск.
	lastRiskDollars decimal.Decimal

	// Снимок текущей открытой позиции — нужен переносу стопа в безубыток
	// (BreakevenTriggerR), который проверяется на каждой свече, пока
	// позиция открыта, а не только в момент входа.
	posSideActive   posSide
	posEntry        decimal.Decimal
	posStop         decimal.Decimal
	posTake         decimal.Decimal
	posRiskDistance decimal.Decimal // |entry-исходный стоп| в цене (не в $)
	breakevenDone   bool
}

// funding может быть nil — тогда вход по funding carry (FundingCarryMinRate)
// просто не проверяется.
func NewBreakout(exec domain.Executor, equity domain.EquitySource, risk domain.RiskGate, funding domain.FundingRateSource, params Params) *Breakout {
	// ADXPeriod по умолчанию 14 (общепринятое значение), если не задан —
	// защита от panic на Div(0) внутри ADX, а не только "разумный дефолт":
	// тесты и старые вызовы, собирающие Params без этого поля, не должны
	// падать даже если TrendStrengthMinADX==0 (фильтр всё равно выключен).
	adxPeriod := params.ADXPeriod
	if adxPeriod <= 0 {
		adxPeriod = 14
	}
	// Тот же приём, что и для ADXPeriod: RollingAverage(0) паникует на
	// модуло нулю при первом Update. Пока VolTargetPeriod<=0 (выключено),
	// сам период индикатора значения не имеет — таргетирование гасится
	// проверкой params.VolTargetPeriod > 0 в openPosition, а не тут.
	volTargetPeriod := params.VolTargetPeriod
	if volTargetPeriod <= 0 {
		volTargetPeriod = 50
	}

	return &Breakout{
		exec:     exec,
		equity:   equity,
		risk:     risk,
		funding:  funding,
		params:   params,
		st:       stateIdle,
		highs:    make([]decimal.Decimal, 0, params.LookbackBars),
		lows:     make([]decimal.Decimal, 0, params.LookbackBars),
		trendEMA: indicator.NewEMA(params.TrendEMAPeriod),
		atr:      indicator.NewATR(params.ATRPeriod),
		adx:      indicator.NewADX(adxPeriod),
		volAvg:   indicator.NewRollingAverage(params.VolumeAvgPeriod),
		atrAvg:   indicator.NewRollingAverage(volTargetPeriod),
	}
}

// SetNotifier подключает канал уведомлений (например, Telegram) о сделках
// и критических ошибках. Необязателен — без вызова бот просто не шлёт
// уведомления, только логирует, как раньше.
func (b *Breakout) SetNotifier(n domain.Notifier) {
	b.mu.Lock()
	b.notify = n
	b.mu.Unlock()
}

func (b *Breakout) notifyf(format string, args ...any) {
	b.mu.Lock()
	n := b.notify
	b.mu.Unlock()
	if n == nil {
		return
	}
	n.Notify(context.Background(), fmt.Sprintf(format, args...))
}

// Warmup заполняет буфер и индикаторы историей, чтобы бот не ждал полного
// набора свечей после старта. Свечи должны идти в хронологическом порядке.
func (b *Breakout) Warmup(candles []domain.Candle) {
	b.mu.Lock()
	defer b.mu.Unlock()

	for _, c := range candles {
		b.pushHigh(c.High)
		b.pushLow(c.Low)
		b.volAvg.Update(c.Volume)
		b.trendEMA.Update(c.Close)
		b.updateATR(c.High, c.Low, c.Close)
		b.adx.Update(c.High, c.Low, c.Close)
	}
	slog.Info(fmt.Sprintf("📚 Прогрев: скормлено %d свечей | окно пробоя %d/%d",
		len(candles), len(b.highs), b.params.LookbackBars))
}

// updateATR обновляет ATR и (только когда он уже Ready) — среднее ATR для
// таргетирования волатильности. Общий для Warmup и OnCandle, чтобы гарантию
// "не кормить atrAvg нулевыми затравочными значениями ATR" нельзя было
// забыть в одном из двух мест: RollingAverage.Update() безусловно
// подмешивает переданное значение в скользящее среднее, а ATR.Update()
// возвращает ровно decimal.Zero на каждом баре до собственной готовности
// (см. indicator/atr.go) — без этой проверки те нули занижали бы avgATR на
// всё время, пока они остаются в окне VolTargetPeriod. Сейчас это скрыто
// тем, что прогрев (warmupBars) всегда длиннее ATRPeriod, но не длиннее
// VolTargetPeriod ни при каких условиях не гарантировалось. Возвращает
// текущее значение ATR (готовое или нет — вызывающий сам решает, что
// делать с неготовым). Вызывается под удерживаемым b.mu.
func (b *Breakout) updateATR(high, low, close decimal.Decimal) decimal.Decimal {
	atrVal := b.atr.Update(high, low, close)
	if b.atr.Ready() {
		b.atrAvg.Update(atrVal)
	}
	return atrVal
}

// pushHigh/pushLow вызываются под удерживаемым b.mu.
func (b *Breakout) pushHigh(high decimal.Decimal) {
	b.highs = append(b.highs, high)
	if len(b.highs) > b.params.LookbackBars {
		b.highs = b.highs[len(b.highs)-b.params.LookbackBars:]
	}
}

func (b *Breakout) pushLow(low decimal.Decimal) {
	b.lows = append(b.lows, low)
	if len(b.lows) > b.params.LookbackBars {
		b.lows = b.lows[len(b.lows)-b.params.LookbackBars:]
	}
}

// OnCandle — единственная точка входа для рыночных данных.
func (b *Breakout) OnCandle(ctx context.Context, c domain.Candle) {
	if !c.IsClosed || c.Symbol != b.exec.Symbol() {
		return
	}

	// Funding rate для carry-входа (см. ниже) нужен только когда автомат
	// реально будет искать сигнал (свободен и не в cooldown) — короткая
	// проверка состояния перед сетевым вызовом, чтобы не тратить его впустую
	// на каждой свече, пока позиция уже открыта (там funding без надобности:
	// в этой ветке switch ниже даже не выполняется). Между этим peek и
	// реальным Lock() ниже состояние теоретически может смениться через
	// OnOrderEvent на другой горутине — тогда funding просто окажется
	// заранее не нужен, что безопасно (не гонка данных, тот же b.st ещё раз
	// проверяется под настоящей блокировкой).
	b.mu.Lock()
	mayNeedSignal := b.st == stateIdle && b.cooldownLeft == 0
	b.mu.Unlock()

	var fundingRate decimal.Decimal
	haveFunding := false
	if mayNeedSignal && b.funding != nil && b.params.FundingCarryMinRate.IsPositive() {
		if rate, err := b.funding.FundingRate(ctx); err == nil {
			fundingRate = rate
			haveFunding = true
		} else {
			slog.Warn(fmt.Sprintf("⚠️  Funding rate для carry-входа недоступен: %v", err))
		}
	}

	b.mu.Lock()

	// Уровень пробоя и объёмный фильтр сравниваются с историей ДО этой
	// свечи (иначе резкий всплеск объёма/цены завышал бы собственный порог
	// сравнения) — тот же принцип, что и раньше для prevHigh/pushHigh.
	prevHigh, rangeReady := b.highestHigh()
	prevLow, _ := b.lowestLow() // буферы highs/lows всегда одной длины — readiness общий
	prevVolAvg, volReady := b.volAvg.Value(), b.volAvg.Ready()
	// Средний ATR ДО этой свечи — база для сравнения "волатильнее ли сейчас
	// обычного", тот же принцип, что и у prevHigh/prevVolAvg: если включить
	// текущий бар в свою же базу сравнения, вспышка волатильности отчасти
	// маскирует сама себя.
	prevAvgATR := b.atrAvg.Value()

	b.pushHigh(c.High)
	b.pushLow(c.Low)
	b.volAvg.Update(c.Volume)
	trendVal := b.trendEMA.Update(c.Close)
	trendReady := b.trendEMA.Ready()
	atrVal := b.updateATR(c.High, c.Low, c.Close)
	atrReady := b.atr.Ready()
	adxVal := b.adx.Update(c.High, c.Low, c.Close)

	if b.st != stateIdle {
		st := b.st
		// Перенос стопа в безубыток проверяется, пока позиция открыта —
		// в отличие от поиска сигналов, это не разовое действие при входе,
		// а проверка на каждой свече, пока сделка не закрылась.
		checkBE := st == stateInPosition && b.params.BreakevenTriggerR.IsPositive() && !b.breakevenDone
		var beSide posSide
		var beEntry, beStop, beTake, beRiskDist decimal.Decimal
		if checkBE {
			beSide, beEntry, beStop, beTake, beRiskDist = b.posSideActive, b.posEntry, b.posStop, b.posTake, b.posRiskDistance
		}
		b.mu.Unlock()

		if checkBE {
			b.maybeMoveToBreakeven(ctx, c, beSide, beEntry, beStop, beTake, beRiskDist)
		} else {
			slog.Info(fmt.Sprintf("📊 %s close=%s | состояние %s, сигналы не ищу", c.Symbol, c.Close, st))
		}
		return
	}
	if b.cooldownLeft > 0 {
		b.cooldownLeft--
		left := b.cooldownLeft
		b.mu.Unlock()
		slog.Info(fmt.Sprintf("⏳ Пауза после сделки: осталось %d свечей", left))
		return
	}
	if !rangeReady || !volReady || !trendReady || !atrReady {
		have := len(b.highs)
		b.mu.Unlock()
		slog.Info(fmt.Sprintf("📚 Набираю историю индикаторов: %d/%d свечей", have, b.params.LookbackBars))
		return
	}

	longTrigger := prevHigh.Mul(decimal.NewFromInt(1).Add(b.params.BreakoutPct.Div(decimal.NewFromInt(100))))
	shortTrigger := prevLow.Mul(decimal.NewFromInt(1).Sub(b.params.BreakoutPct.Div(decimal.NewFromInt(100))))
	volumeTrigger := prevVolAvg.Mul(b.params.VolumeMultiplier)
	volumeOK := c.Volume.GreaterThan(volumeTrigger)
	// TrendStrengthMinADX==0 — фильтр выключен, любой ADX проходит. Пока
	// сам ADX не прогрелся, его Value() — нулевой decimal, что при включённом
	// фильтре (порог > 0) закономерно блокирует вход, а не отдельная
	// проверка Ready() — совпадает с тем, как это уже сделано для readiness
	// остальных индикаторов через zero-value.
	adxOK := b.params.TrendStrengthMinADX.IsZero() || adxVal.GreaterThanOrEqual(b.params.TrendStrengthMinADX)

	longOK := c.Close.GreaterThan(longTrigger) && c.Close.GreaterThan(trendVal) && volumeOK && adxOK
	shortOK := c.Close.LessThan(shortTrigger) && c.Close.LessThan(trendVal) && volumeOK && adxOK

	// Вход "на возврат к среднему" — зеркальная идея пробою, но работает
	// ТОЛЬКО там, где пробойный фильтр силы тренда его выключил (ADX ниже
	// порога, т.е. boковик): цена ушла далеко от EMA-тренда — ставим на
	// возврат, а не на продолжение. См. Params.MeanRevATRMultiplier.
	meanRevLongOK, meanRevShortOK := false, false
	if b.params.MeanRevATRMultiplier.IsPositive() && b.params.TrendStrengthMinADX.IsPositive() && adxVal.LessThan(b.params.TrendStrengthMinADX) {
		stretch := atrVal.Mul(b.params.MeanRevATRMultiplier)
		meanRevLongOK = c.Close.LessThan(trendVal.Sub(stretch)) && volumeOK
		meanRevShortOK = c.Close.GreaterThan(trendVal.Add(stretch)) && volumeOK
	}

	// Carry по funding rate — третий, независимый от цены источник сигнала:
	// не направление рынка и не боковик, а сама плата за удержание позиции.
	// Лонг, когда funding сильно отрицательный (шорты платят лонгам), шорт —
	// когда сильно положительный (лонги платят шортам). Использует тот же
	// ATR-стоп/тейк, что и остальные входы; отдельного выхода "закрыть при
	// нормализации funding" сознательно нет — упрощение, не гарантия
	// оптимальности (см. Params.FundingCarryMinRate).
	fundingCarryLongOK := haveFunding && fundingRate.LessThan(b.params.FundingCarryMinRate.Neg())
	fundingCarryShortOK := haveFunding && fundingRate.GreaterThan(b.params.FundingCarryMinRate)

	var openSide posSide
	var reason string
	switch {
	case longOK:
		openSide, reason = sideLong, "ПРОБОЙ"
	case shortOK:
		openSide, reason = sideShort, "ПРОБОЙ"
	case meanRevLongOK:
		openSide, reason = sideLong, "ВОЗВРАТ К СРЕДНЕЙ"
	case meanRevShortOK:
		openSide, reason = sideShort, "ВОЗВРАТ К СРЕДНЕЙ"
	case fundingCarryLongOK:
		openSide, reason = sideLong, "FUNDING CARRY"
	case fundingCarryShortOK:
		openSide, reason = sideShort, "FUNDING CARRY"
	default:
		b.mu.Unlock()
		slog.Info(fmt.Sprintf("🧊 Сигнала нет | close=%s | лонг(нужен >%s, тренд>%s) шорт(нужен <%s, тренд<%s) объём(%v, нужен >%s) ADX(%v, нужен >=%s)",
			c.Close, longTrigger.StringFixed(2), trendVal.StringFixed(2),
			shortTrigger.StringFixed(2), trendVal.StringFixed(2),
			volumeOK, volumeTrigger.StringFixed(4), adxOK, b.params.TrendStrengthMinADX.StringFixed(1)))
		return
	}

	// Занимаем автомат до сетевых вызовов: параллельное событие из приватного
	// потока увидит OPENING и не откроет вторую позицию.
	b.st = stateOpening
	b.mu.Unlock()

	slog.Info(fmt.Sprintf("🔥 %s %s с подтверждением | close=%s | тренд EMA%d=%s | ADX=%s | объём %s > %s",
		reason, openSide, c.Close, b.params.TrendEMAPeriod, trendVal.StringFixed(2), adxVal.StringFixed(1),
		c.Volume.StringFixed(4), volumeTrigger.StringFixed(4)))
	b.openPosition(ctx, openSide, reason, c.Close, atrVal, prevAvgATR)
}

// highestHigh/lowestLow возвращают максимум/минимум буфера. Вызываются под
// удерживаемым b.mu.
func (b *Breakout) highestHigh() (decimal.Decimal, bool) {
	if len(b.highs) < b.params.LookbackBars {
		return decimal.Zero, false
	}
	max := b.highs[0]
	for _, h := range b.highs[1:] {
		if h.GreaterThan(max) {
			max = h
		}
	}
	return max, true
}

func (b *Breakout) lowestLow() (decimal.Decimal, bool) {
	if len(b.lows) < b.params.LookbackBars {
		return decimal.Zero, false
	}
	min := b.lows[0]
	for _, l := range b.lows[1:] {
		if l.LessThan(min) {
			min = l
		}
	}
	return min, true
}

// openPosition считает размер позиции от риска и эквити, проверяет
// портфельный риск-лимит и только потом идёт на биржу. side определяет
// направление входа — код общий для лонга и шорта. reason — только для
// логов/уведомлений ("ПРОБОЙ" / "ВОЗВРАТ К СРЕДНЕЙ"), на исполнение не влияет.
// avgATR — среднее ATR за VolTargetPeriod баров ДО этой свечи (0, если
// таргетирование волатильности выключено или ещё не прогрелось).
func (b *Breakout) openPosition(ctx context.Context, side posSide, reason string, refPrice, atrValue, avgATR decimal.Decimal) {
	stopDistance := atrValue.Mul(b.params.ATRStopMultiplier)
	if stopDistance.LessThanOrEqual(decimal.Zero) {
		slog.Error(fmt.Sprintf("❌ Нулевое расстояние стопа (ATR=%s) — сделку не открываю", atrValue))
		b.setState(stateIdle)
		return
	}

	equity, err := b.equity.Equity(ctx)
	if err != nil {
		slog.Error(fmt.Sprintf("❌ Не удалось получить эквити счёта: %v", err))
		b.setState(stateIdle)
		return
	}

	if b.risk.DailyLossLimitHit(equity) {
		slog.Warn(fmt.Sprintf("🛑 Дневной лимит убытка достигнут — новые сделки не открываю"))
		b.notifyf("🛑 %s: дневной лимит убытка достигнут, новые сделки не открываю", b.exec.Symbol())
		b.setState(stateIdle)
		return
	}

	if b.risk.MaxDrawdownHit(equity) {
		slog.Warn(fmt.Sprintf("🛑 Просадка портфеля от пика превысила лимит — новые сделки не открываю"))
		b.notifyf("🛑 %s: просадка портфеля от пика превысила лимит, новые сделки не открываю", b.exec.Symbol())
		b.setState(stateIdle)
		return
	}

	riskDollars := equity.Mul(b.params.RiskPerTradePct).Div(decimal.NewFromInt(100))

	// Таргетирование волатильности: если сейчас ATR выше своего среднего за
	// VolTargetPeriod баров, риск пропорционально уменьшаем (никогда не
	// увеличиваем сверх обычного) — ATR-стоп уже уравнивает риск на сделку
	// между спокойными и волатильными периодами, это про другое: сама
	// вспышка волатильности может означать, что текущий ATR временно
	// завышен/шумный относительно обычного уровня символа.
	if b.params.VolTargetPeriod > 0 && avgATR.IsPositive() && atrValue.GreaterThan(avgATR) {
		scale := avgATR.Div(atrValue)
		riskDollars = riskDollars.Mul(scale)
		slog.Info(fmt.Sprintf("📉 Волатильность выше обычной (ATR=%s, среднее=%s) — риск уменьшен в %s раз",
			atrValue.StringFixed(4), avgATR.StringFixed(4), atrValue.Div(avgATR).StringFixed(2)))
	}

	if allowed, blockReason := b.risk.CanOpen(equity, riskDollars); !allowed {
		slog.Warn(fmt.Sprintf("🚫 Портфельный риск-лимит не позволяет открыть сделку: %s", blockReason))
		b.setState(stateIdle)
		return
	}

	qtyRaw := riskDollars.Div(stopDistance)
	qty, err := b.exec.NormalizeQuantity(qtyRaw, refPrice)
	if err != nil {
		slog.Error(fmt.Sprintf("❌ Объём отклонён фильтрами биржи: %v", err))
		b.setState(stateIdle)
		return
	}

	// Подчищаем хвосты прошлой сделки, чтобы чужой стоп не закрыл новый вход.
	if err := b.exec.CancelAll(ctx); err != nil {
		slog.Warn(fmt.Sprintf("⚠️  Очистка ордеров перед входом: %v", err))
	}

	var entry decimal.Decimal
	if side == sideLong {
		entry, err = b.exec.OpenLong(ctx, qty)
	} else {
		entry, err = b.exec.OpenShort(ctx, qty)
	}
	if err != nil {
		slog.Error(fmt.Sprintf("❌ Вход (%s) не удался: %v", side, err))
		// Ордер мог всё же исполниться до обрыва связи — проверяем позицию,
		// иначе она осталась бы висеть без стопа и без учёта в автомате.
		b.reconcile(ctx)
		return
	}

	stop, take, err := b.protect(ctx, side, entry, stopDistance)
	if err != nil {
		slog.Error(fmt.Sprintf("🚨 Позиция без защиты (%v) — закрываю по рынку", err))
		if cerr := b.exec.ClosePositionMarket(ctx); cerr != nil {
			slog.Error(fmt.Sprintf("🚨🚨 НЕ УДАЛОСЬ ЗАКРЫТЬ ПОЗИЦИЮ: %v — ТРЕБУЕТСЯ РУЧНОЕ ВМЕШАТЕЛЬСТВО", cerr))
			b.notifyf("🚨🚨 %s: НЕ УДАЛОСЬ ЗАКРЫТЬ ПОЗИЦИЮ (%v) — ТРЕБУЕТСЯ РУЧНОЕ ВМЕШАТЕЛЬСТВО", b.exec.Symbol(), cerr)
		}
		b.setState(stateIdle)
		return
	}

	actualRisk := qty.Mul(entry.Sub(stop)).Abs()
	b.risk.Reserve(b.exec.Symbol(), actualRisk)
	b.mu.Lock()
	b.lastRiskDollars = actualRisk
	b.posSideActive = side
	b.posEntry = entry
	b.posStop = stop
	b.posTake = take
	b.posRiskDistance = entry.Sub(stop).Abs()
	b.breakevenDone = false
	b.mu.Unlock()

	b.setState(stateInPosition)
	riskPct := decimal.Zero
	if equity.IsPositive() {
		riskPct = actualRisk.Div(equity).Mul(decimal.NewFromInt(100))
	}
	slog.Info(fmt.Sprintf("🟢 В позиции (%s %s) | вход %s | объём %s | стоп %s | тейк %s | риск $%s (%s%% от эквити $%s)",
		reason, side, entry.StringFixed(2), qty, stop.StringFixed(2), take.StringFixed(2),
		actualRisk.StringFixed(2), riskPct.StringFixed(2), equity.StringFixed(2)))
	b.notifyf("🟢 %s: %s %s\nВход %s | объём %s\nСтоп %s | Тейк %s\nРиск $%s (%s%% от эквити)",
		b.exec.Symbol(), reason, side, entry.StringFixed(2), qty,
		stop.StringFixed(2), take.StringFixed(2), actualRisk.StringFixed(2), riskPct.StringFixed(2))
}

// maybeMoveToBreakeven переносит стоп на цену входа, если незафиксированная
// прибыль (лучшая цена внутри текущей свечи в пользу позиции) достигла
// BreakevenTriggerR от исходного риска сделки. Тейк не трогается. Делает
// сетевые вызовы — вызывается вне блокировки b.mu.
func (b *Breakout) maybeMoveToBreakeven(ctx context.Context, c domain.Candle, side posSide, entry, stop, take, riskDist decimal.Decimal) {
	if riskDist.LessThanOrEqual(decimal.Zero) {
		return
	}
	trigger := b.params.BreakevenTriggerR.Mul(riskDist)

	var favorable decimal.Decimal
	if side == sideLong {
		favorable = c.High.Sub(entry)
	} else {
		favorable = entry.Sub(c.Low)
	}
	if favorable.LessThan(trigger) {
		return
	}

	// Стоп в безубыток допускается ровно на цене входа (нулевой риск) —
	// строгое неравенство не нужно, опасна только не та сторона от входа
	// (стоп выше входа для лонга и т.п.), которую и отсекаем ниже.
	newStop := entry
	if side == sideLong {
		newStop = b.exec.NormalizePrice(newStop, true)
		if newStop.GreaterThan(entry) {
			return // округление к шагу цены увело стоп не на ту сторону — не переставляем
		}
	} else {
		newStop = b.exec.NormalizePrice(newStop, false)
		if newStop.LessThan(entry) {
			return
		}
	}

	if err := b.exec.CancelAll(ctx); err != nil {
		slog.Warn(fmt.Sprintf("⚠️  Снятие ордеров перед переносом в безубыток: %v", err))
	}
	if err := b.exec.PlaceStopLoss(ctx, newStop); err != nil {
		slog.Warn(fmt.Sprintf("⚠️  Не удалось перенести стоп в безубыток (%v) — пробую восстановить исходный стоп", err))
		if rerr := b.exec.PlaceStopLoss(ctx, stop); rerr != nil {
			slog.Error(fmt.Sprintf("🚨 Позиция без защиты после неудачного переноса в безубыток (%v) — закрываю по рынку", rerr))
			if cerr := b.exec.ClosePositionMarket(ctx); cerr != nil {
				slog.Error(fmt.Sprintf("🚨🚨 НЕ УДАЛОСЬ ЗАКРЫТЬ ПОЗИЦИЮ: %v — ТРЕБУЕТСЯ РУЧНОЕ ВМЕШАТЕЛЬСТВО", cerr))
				b.notifyf("🚨🚨 %s: НЕ УДАЛОСЬ ЗАКРЫТЬ ПОЗИЦИЮ (%v) — ТРЕБУЕТСЯ РУЧНОЕ ВМЕШАТЕЛЬСТВО", b.exec.Symbol(), cerr)
			}
			// Если позиция уже была закрыта своим стопом/тейком до нашей
			// попытки переноса (гонка) — ClosePositionMarket молча
			// не сделает ничего, а reconcile приведёт автомат в идле корректно.
			b.reconcile(ctx)
			return
		}
		if terr := b.exec.PlaceTakeProfit(ctx, take); terr != nil {
			slog.Warn(fmt.Sprintf("⚠️  Тейк не восстановлен (%v); позиция защищена стопом, выход только по нему", terr))
		}
		return
	}
	if err := b.exec.PlaceTakeProfit(ctx, take); err != nil {
		slog.Warn(fmt.Sprintf("⚠️  Тейк не восстановлен после переноса стопа в безубыток (%v); позиция защищена стопом", err))
	}

	b.mu.Lock()
	b.posStop = newStop
	b.breakevenDone = true
	b.mu.Unlock()

	slog.Info(fmt.Sprintf("🔒 %s: стоп перенесён в безубыток (%s) после движения на %sR", b.exec.Symbol(), newStop.StringFixed(2), b.params.BreakevenTriggerR))
	b.notifyf("🔒 %s: стоп перенесён в безубыток (%s)", b.exec.Symbol(), newStop.StringFixed(2))
}

// protect ставит стоп и тейк вокруг фактической цены входа на расстоянии,
// заданном волатильностью (ATR) — направление зависит от side. Ошибка
// стопа фатальна для сделки, ошибка тейка — нет: риск и так ограничен.
func (b *Breakout) protect(ctx context.Context, side posSide, entry, stopDistance decimal.Decimal) (stop, take decimal.Decimal, err error) {
	takeDistance := stopDistance.Mul(b.params.RiskRewardRatio)

	if side == sideLong {
		stop = entry.Sub(stopDistance)
		take = entry.Add(takeDistance)
		// Стоп округляем вниз, тейк вверх — так они не подъезжают к цене
		// входа вплотную и не срабатывают мгновенно после выставления.
		stop = b.exec.NormalizePrice(stop, true)
		take = b.exec.NormalizePrice(take, false)
	} else {
		stop = entry.Add(stopDistance)
		take = entry.Sub(takeDistance)
		// Симметрично: округляем в сторону, дальнюю от входа.
		stop = b.exec.NormalizePrice(stop, false)
		take = b.exec.NormalizePrice(take, true)
	}

	valid := stop.LessThan(entry) && take.GreaterThan(entry)
	if side == sideShort {
		valid = stop.GreaterThan(entry) && take.LessThan(entry)
	}
	if !valid {
		return decimal.Zero, decimal.Zero, fmt.Errorf(
			"после округления к шагу цены стоп %s / тейк %s не по разные стороны от входа %s (%s)",
			stop, take, entry, side)
	}

	if err := b.exec.PlaceStopLoss(ctx, stop); err != nil {
		return decimal.Zero, decimal.Zero, err
	}
	if err := b.exec.PlaceTakeProfit(ctx, take); err != nil {
		slog.Warn(fmt.Sprintf("⚠️  Тейк не выставлен (%v); позиция защищена стопом, выход только по нему", err))
	}
	return stop, take, nil
}

// OnOrderEvent реагирует на приватный поток. Событие — лишь повод сверить
// состояние с биржей: тип сработавшего ордера подсказывает причину, но
// источником истины остаётся реальный размер позиции.
func (b *Breakout) OnOrderEvent(ctx context.Context, ev domain.OrderEvent) {
	if ev.Symbol != b.exec.Symbol() {
		return
	}
	if ev.Status != "FILLED" && ev.Status != "CANCELED" && ev.Status != "EXPIRED" {
		return
	}

	if ev.Status == "FILLED" {
		// R-мультипл — во сколько раз реализованный PnL больше/меньше
		// изначально рискованной суммы на этой сделке — нагляднее голого
		// доллара в уведомлении: сразу видно, тейк это (обычно ~+1R) или
		// частичный/полный стоп.
		b.mu.Lock()
		riskBase := b.lastRiskDollars
		b.mu.Unlock()
		rMultiple := decimal.Zero
		if riskBase.IsPositive() {
			rMultiple = ev.RealizedPnL.Div(riskBase)
		}

		switch ev.Type {
		case "STOP_MARKET":
			slog.Info(fmt.Sprintf("🔔 Сработал СТОП по %s | PnL $%s (%sR)", ev.AvgPrice.StringFixed(2), ev.RealizedPnL.StringFixed(2), rMultiple.StringFixed(2)))
			b.notifyf("🔔 %s: СТОП по %s\nPnL $%s (%sR)", b.exec.Symbol(), ev.AvgPrice.StringFixed(2), ev.RealizedPnL.StringFixed(2), rMultiple.StringFixed(2))
		case "TAKE_PROFIT_MARKET":
			slog.Info(fmt.Sprintf("🎯 Сработал ТЕЙК по %s | PnL $%s (%sR)", ev.AvgPrice.StringFixed(2), ev.RealizedPnL.StringFixed(2), rMultiple.StringFixed(2)))
			b.notifyf("🎯 %s: ТЕЙК по %s\nPnL $%s (%sR)", b.exec.Symbol(), ev.AvgPrice.StringFixed(2), ev.RealizedPnL.StringFixed(2), rMultiple.StringFixed(2))
		}
		// rp ненулевой только на закрывающих/уменьшающих позицию филах —
		// записываем сразу, не дожидаясь reconcile: так корректно
		// учитываются и частичные закрытия несколькими филами подряд.
		if !ev.RealizedPnL.IsZero() {
			b.risk.RecordRealizedPnL(ev.RealizedPnL)
		}
	}

	b.reconcile(ctx)
}

// Reconcile сверяет автомат с биржей. Вызывается по событиям и по таймеру —
// таймер страхует от потерянных событий, когда приватный поток был в обрыве.
func (b *Breakout) Reconcile(ctx context.Context) { b.reconcile(ctx) }

func (b *Breakout) reconcile(ctx context.Context) {
	b.mu.Lock()
	if b.st == stateOpening {
		// Операция ещё в полёте — её результат сам обновит состояние.
		b.mu.Unlock()
		return
	}
	current := b.st
	b.mu.Unlock()

	pos, err := b.exec.Position(ctx)
	if err != nil {
		slog.Warn(fmt.Sprintf("⚠️  Сверка позиции не удалась: %v", err))
		return
	}

	switch {
	case pos.IsFlat() && current == stateInPosition:
		// Позиция закрылась — снимаем осиротевший парный ордер и освобождаем
		// зарезервированный риск. Реализованный PnL к этому моменту уже
		// учтён в OnOrderEvent по полю rp — здесь его пересчитывать не нужно.
		if err := b.exec.CancelAll(ctx); err != nil {
			slog.Warn(fmt.Sprintf("⚠️  Снятие ордеров после закрытия: %v", err))
		}
		b.risk.Release(b.exec.Symbol())
		b.mu.Lock()
		b.st = stateIdle
		b.cooldownLeft = b.params.CooldownBars
		b.mu.Unlock()
		slog.Info(fmt.Sprintf("🟢 Позиция закрыта. Пауза %d свечей, затем снова ищу сигналы", b.params.CooldownBars))

	case !pos.IsFlat() && current == stateIdle:
		// Позиция есть, а автомат считал себя плоским: пропущенное событие или
		// ручная сделка. Принимаем её, чтобы не открыть вторую поверх.
		slog.Warn(fmt.Sprintf("⚠️  Обнаружена позиция %s вне учёта бота — беру под контроль", pos.Amount))
		b.adopt(ctx, pos)
	}
}

// Recover вызывается при старте: приводит биржу и автомат к согласию.
// Должен вызываться после Warmup — если индикаторы не прогреты (в первую
// очередь ATR), adopt() не сможет посчитать стоп для существующей позиции.
func (b *Breakout) Recover(ctx context.Context) error {
	pos, err := b.exec.Position(ctx)
	if err != nil {
		return fmt.Errorf("чтение позиции при старте: %w", err)
	}

	if pos.IsFlat() {
		if err := b.exec.CancelAll(ctx); err != nil {
			return fmt.Errorf("очистка ордеров при старте: %w", err)
		}
		b.setState(stateIdle)
		slog.Info("✅ Стартовое состояние: позиций нет, книга ордеров чиста")
		return nil
	}

	slog.Warn(fmt.Sprintf("⚠️  При старте найдена позиция: %s @ %s", pos.Amount, pos.EntryPrice))
	b.adopt(ctx, pos)
	return nil
}

// adopt берёт существующую позицию (лонг или шорт) под управление:
// переставляет защитные ордера от её реальной цены входа.
func (b *Breakout) adopt(ctx context.Context, pos domain.Position) {
	side := sideLong
	if !pos.IsLong() {
		side = sideShort
	}

	// Пересоздаём защиту: какие ордера остались от прошлого запуска — неизвестно.
	if err := b.exec.CancelAll(ctx); err != nil {
		slog.Warn(fmt.Sprintf("⚠️  Снятие старых ордеров: %v", err))
	}

	b.mu.Lock()
	stopDistance := b.atr.Value().Mul(b.params.ATRStopMultiplier)
	b.mu.Unlock()

	stop, take, err := b.protect(ctx, side, pos.EntryPrice, stopDistance)
	if err != nil {
		slog.Error(fmt.Sprintf("🚨 Не удалось защитить принятую позицию (%v) — закрываю по рынку", err))
		if cerr := b.exec.ClosePositionMarket(ctx); cerr != nil {
			slog.Error(fmt.Sprintf("🚨🚨 НЕ УДАЛОСЬ ЗАКРЫТЬ ПОЗИЦИЮ: %v — ТРЕБУЕТСЯ РУЧНОЕ ВМЕШАТЕЛЬСТВО", cerr))
			b.notifyf("🚨🚨 %s: НЕ УДАЛОСЬ ЗАКРЫТЬ ПОЗИЦИЮ (%v) — ТРЕБУЕТСЯ РУЧНОЕ ВМЕШАТЕЛЬСТВО", b.exec.Symbol(), cerr)
		}
		b.setState(stateIdle)
		return
	}

	actualRisk := pos.Amount.Abs().Mul(pos.EntryPrice.Sub(stop)).Abs()
	b.risk.Reserve(b.exec.Symbol(), actualRisk)
	b.mu.Lock()
	b.lastRiskDollars = actualRisk
	b.posSideActive = side
	b.posEntry = pos.EntryPrice
	b.posStop = stop
	b.posTake = take
	b.posRiskDistance = pos.EntryPrice.Sub(stop).Abs()
	b.breakevenDone = false
	b.mu.Unlock()

	b.setState(stateInPosition)
	slog.Info(fmt.Sprintf("🟢 Позиция (%s) принята под управление, защита переставлена от %s", side, pos.EntryPrice.StringFixed(2)))
}

// Shutdown готовит бота к остановке.
//
// Ордера снимаются, только если позиции нет. При открытой позиции стоп и тейк
// намеренно остаются на бирже: они продолжают защищать позицию, пока процесс
// не работает. Закрывать саму позицию из-за перезапуска — решение трейдера,
// не бота, поэтому она остаётся как есть.
func (b *Breakout) Shutdown(ctx context.Context) {
	pos, err := b.exec.Position(ctx)
	if err != nil {
		slog.Warn(fmt.Sprintf("⚠️  Не удалось прочитать позицию при остановке: %v — ордера не трогаю", err))
		return
	}

	if pos.IsFlat() {
		if err := b.exec.CancelAll(ctx); err != nil {
			slog.Warn(fmt.Sprintf("⚠️  Снятие ордеров при остановке: %v", err))
		}
		slog.Info("✅ Позиций нет, ордера сняты")
		return
	}

	slog.Warn(fmt.Sprintf("⚠️  Позиция %s @ %s остаётся открытой; защитные ордера оставлены на бирже",
		pos.Amount, pos.EntryPrice.StringFixed(2)))
}

func (b *Breakout) setState(s state) {
	b.mu.Lock()
	b.st = s
	b.mu.Unlock()
}

// State отдаёт текущее состояние (для логов и тестов).
func (b *Breakout) State() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.st.String()
}

// Status отдаёт человекочитаемую сводку по символу — для команд статуса
// (например, /status в Telegram), не для торговой логики.
func (b *Breakout) Status() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.st != stateInPosition {
		return fmt.Sprintf("%s: %s", b.exec.Symbol(), b.st)
	}
	beMark := ""
	if b.breakevenDone {
		beMark = " 🔒безубыток"
	}
	return fmt.Sprintf("%s: %s | вход %s | стоп %s | тейк %s%s",
		b.exec.Symbol(), b.posSideActive, b.posEntry.StringFixed(4), b.posStop.StringFixed(4), b.posTake.StringFixed(4), beMark)
}
