package indicator

import "github.com/shopspring/decimal"

// ATR — Average True Range по Уайлдеру (Wilder's smoothing), O(1) на свечу.
//
// Важно: сглаживание Уайлдера — это НЕ обычная EMA. Коэффициент сглаживания
// у него alpha = 1/period (а не 2/(period+1)), и затравочное значение — это
// простое среднее первых period значений True Range, а не первый TR сам по
// себе. Это стандартное определение ATR; отклонение от него дало бы
// заниженную/завышенную оценку волатильности на старте.
type ATR struct {
	period int

	trSum decimal.Decimal // сумма TR, пока идёт затравочная фаза
	count int             // сколько баров (TR-сэмплов) обработано

	value decimal.Decimal
	ready bool

	prevClose    decimal.Decimal
	hasPrevClose bool
}

// NewATR создаёт ATR с заданным периодом (обычно 14 — общепринятое
// значение, менять без причины не стоит).
func NewATR(period int) *ATR {
	return &ATR{period: period}
}

// Update принимает High/Low/Close ЗАКРЫТОЙ свечи и возвращает новое
// значение ATR. Пока Ready() == false, возвращаемое значение статистически
// бессмысленно (недостаточно данных для затравки).
func (a *ATR) Update(high, low, close decimal.Decimal) decimal.Decimal {
	tr := high.Sub(low)
	if a.hasPrevClose {
		tr = decimal.Max(tr, high.Sub(a.prevClose).Abs(), low.Sub(a.prevClose).Abs())
	}
	a.prevClose = close
	a.hasPrevClose = true
	a.count++

	if !a.ready {
		a.trSum = a.trSum.Add(tr)
		if a.count >= a.period {
			a.value = a.trSum.Div(decimal.NewFromInt(int64(a.period)))
			a.ready = true
		}
		return a.value
	}

	// ATR_t = ATR_{t-1} + (TR_t - ATR_{t-1}) / period
	a.value = a.value.Add(tr.Sub(a.value).Div(decimal.NewFromInt(int64(a.period))))
	return a.value
}

// Value возвращает текущее значение ATR. Смысл имеет только при Ready().
func (a *ATR) Value() decimal.Decimal {
	return a.value
}

// Ready сообщает, накоплено ли достаточно баров для затравки (period штук).
func (a *ATR) Ready() bool {
	return a.ready
}
