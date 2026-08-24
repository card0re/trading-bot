package indicator

import "github.com/shopspring/decimal"

// ADX — Average Directional Index по Уайлдеру: мера силы тренда (не его
// направления). Низкий ADX — рынок в боковике, много ложных пробоев;
// высокий ADX — есть выраженный тренд. Используется как дополнительный
// фильтр входа поверх пробоя+тренда(EMA)+объёма: не входить, когда рынок
// "пилит", даже если формальный пробой случился.
//
// Как и ATR, требует два прохода сглаживания Уайлдера: сначала +DM/-DM/TR,
// потом сам DX. Поэтому Ready() наступает примерно через 2*period баров,
// а не через period, как у ATR/EMA — DX недоступен, пока не готовы DI.
type ADX struct {
	period int

	// Затравочные суммы (первая фаза сглаживания: +DM/-DM/TR).
	dmSumCount int
	plusDMSum  decimal.Decimal
	minusDMSum decimal.Decimal
	trSum      decimal.Decimal

	smoothedPlusDM  decimal.Decimal
	smoothedMinusDM decimal.Decimal
	smoothedTR      decimal.Decimal
	diReady         bool

	prevHigh, prevLow, prevClose decimal.Decimal
	hasPrev                      bool

	// Затравочная сумма второй фазы (сглаживание DX → ADX).
	dxSumCount int
	dxSum      decimal.Decimal

	value decimal.Decimal
	ready bool
}

func NewADX(period int) *ADX {
	return &ADX{period: period}
}

// Update принимает High/Low/Close закрытой свечи и возвращает текущее
// значение ADX. Пока Ready() == false, значение статистически бессмысленно.
func (a *ADX) Update(high, low, close decimal.Decimal) decimal.Decimal {
	if !a.hasPrev {
		a.prevHigh, a.prevLow, a.prevClose = high, low, close
		a.hasPrev = true
		return a.value
	}

	upMove := high.Sub(a.prevHigh)
	downMove := a.prevLow.Sub(low)

	plusDM := decimal.Zero
	if upMove.GreaterThan(downMove) && upMove.IsPositive() {
		plusDM = upMove
	}
	minusDM := decimal.Zero
	if downMove.GreaterThan(upMove) && downMove.IsPositive() {
		minusDM = downMove
	}

	tr := decimal.Max(high.Sub(low), high.Sub(a.prevClose).Abs(), low.Sub(a.prevClose).Abs())

	a.prevHigh, a.prevLow, a.prevClose = high, low, close

	if !a.diReady {
		a.plusDMSum = a.plusDMSum.Add(plusDM)
		a.minusDMSum = a.minusDMSum.Add(minusDM)
		a.trSum = a.trSum.Add(tr)
		a.dmSumCount++
		if a.dmSumCount < a.period {
			return a.value
		}
		periodDec := decimal.NewFromInt(int64(a.period))
		a.smoothedPlusDM = a.plusDMSum.Div(periodDec)
		a.smoothedMinusDM = a.minusDMSum.Div(periodDec)
		a.smoothedTR = a.trSum.Div(periodDec)
		a.diReady = true
	} else {
		periodDec := decimal.NewFromInt(int64(a.period))
		a.smoothedPlusDM = a.smoothedPlusDM.Add(plusDM.Sub(a.smoothedPlusDM).Div(periodDec))
		a.smoothedMinusDM = a.smoothedMinusDM.Add(minusDM.Sub(a.smoothedMinusDM).Div(periodDec))
		a.smoothedTR = a.smoothedTR.Add(tr.Sub(a.smoothedTR).Div(periodDec))
	}

	dx := a.dx()

	periodDec := decimal.NewFromInt(int64(a.period))
	if !a.ready {
		a.dxSum = a.dxSum.Add(dx)
		a.dxSumCount++
		if a.dxSumCount < a.period {
			return a.value
		}
		a.value = a.dxSum.Div(periodDec)
		a.ready = true
		return a.value
	}

	// ADX_t = ADX_{t-1} + (DX_t - ADX_{t-1}) / period — то же сглаживание
	// Уайлдера, что и у ATR, только над рядом DX вместо True Range.
	a.value = a.value.Add(dx.Sub(a.value).Div(periodDec))
	return a.value
}

// dx считает Directional Index текущего бара из уже сглаженных +DM/-DM/TR.
// Возвращает 0, если TR или сумма DI нулевые (боковик с нулевой
// волатильностью) — иначе Div panic'нул бы на делении на ноль.
func (a *ADX) dx() decimal.Decimal {
	if a.smoothedTR.IsZero() {
		return decimal.Zero
	}
	hundred := decimal.NewFromInt(100)
	plusDI := a.smoothedPlusDM.Div(a.smoothedTR).Mul(hundred)
	minusDI := a.smoothedMinusDM.Div(a.smoothedTR).Mul(hundred)

	sum := plusDI.Add(minusDI)
	if sum.IsZero() {
		return decimal.Zero
	}
	return plusDI.Sub(minusDI).Abs().Div(sum).Mul(hundred)
}

// Value возвращает текущее значение ADX (0..100). Смысл имеет только при Ready().
func (a *ADX) Value() decimal.Decimal {
	return a.value
}

// Ready сообщает, накоплено ли достаточно баров для обеих фаз сглаживания
// (примерно 2*period+1 баров).
func (a *ADX) Ready() bool {
	return a.ready
}
