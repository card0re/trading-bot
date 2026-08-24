package indicator

import "github.com/shopspring/decimal"

// RollingAverage — простая скользящая средняя по фиксированному окну
// (используется для среднего объёма). O(1) на свечу за счёт кольцевого
// буфера и текущей суммы — без пересчёта суммы по всему окну на каждой
// свече.
type RollingAverage struct {
	window []decimal.Decimal
	pos    int
	filled int
	sum    decimal.Decimal
	period int
}

// NewRollingAverage создаёт среднюю с заданным окном.
func NewRollingAverage(period int) *RollingAverage {
	return &RollingAverage{window: make([]decimal.Decimal, period), period: period}
}

// Update добавляет новое значение и возвращает текущее среднее (по факту
// накопленных баров, пока окно не заполнилось целиком).
func (r *RollingAverage) Update(v decimal.Decimal) decimal.Decimal {
	if r.filled < r.period {
		r.window[r.pos] = v
		r.sum = r.sum.Add(v)
		r.filled++
	} else {
		old := r.window[r.pos]
		r.sum = r.sum.Sub(old).Add(v)
		r.window[r.pos] = v
	}
	r.pos = (r.pos + 1) % r.period
	return r.Value()
}

// Value возвращает текущее среднее. Смысл имеет только при Ready().
func (r *RollingAverage) Value() decimal.Decimal {
	if r.filled == 0 {
		return decimal.Zero
	}
	return r.sum.Div(decimal.NewFromInt(int64(r.filled)))
}

// Ready сообщает, заполнено ли окно целиком (period сэмплов).
func (r *RollingAverage) Ready() bool {
	return r.filled >= r.period
}
