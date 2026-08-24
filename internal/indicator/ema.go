// Package indicator содержит скользящие индикаторы, используемые стратегией
// для фильтрации сигналов и расчёта стопов/тейков. Все расчёты — на
// decimal.Decimal, чтобы не тащить погрешность float64 в цены и объёмы.
package indicator

import "github.com/shopspring/decimal"

// EMA — экспоненциальная скользящая средняя с O(1) обновлением на свечу.
//
// Затравка: ema_0 = первая цена (не SMA первых N баров). Это стандартный
// упрощённый способ инициализации; смещение на старте нивелируется за
// ~2*period баров, что не критично, так как перед живой торговлей буфер
// прогревается через Warmup на исторических свечах.
type EMA struct {
	period int
	alpha  decimal.Decimal
	value  decimal.Decimal
	ready  bool
}

// NewEMA создаёт EMA с заданным периодом. alpha = 2/(period+1) считается
// один раз с полной точностью и переиспользуется, чтобы не накапливать
// ошибку округления на тысячах последующих обновлений.
func NewEMA(period int) *EMA {
	alpha := decimal.NewFromInt(2).Div(decimal.NewFromInt(int64(period) + 1))
	return &EMA{period: period, alpha: alpha}
}

// roundPrecision — на сколько знаков после запятой округляется внутреннее
// состояние после каждого Update. decimal.Decimal.Mul не округляет результат
// (в отличие от Div): при умножении e.value само на себя каждую свечу без
// этого округления экспонента числа растёт линейно с числом баров, и после
// нескольких десятков тысяч свечей (например, полгода истории на 5-минутках)
// арифметика с big.Int на числах с сотнями тысяч цифр становится
// катастрофически медленной. Round после каждого шага держит точность
// постоянной — с огромным запасом относительно любой реальной цены.
const roundPrecision = 16

// Update добавляет новую цену закрытой свечи и возвращает новое значение EMA.
func (e *EMA) Update(price decimal.Decimal) decimal.Decimal {
	if !e.ready {
		e.value = price.Round(roundPrecision)
		e.ready = true
		return e.value
	}
	// ema_t = alpha*price + (1-alpha)*ema_{t-1}
	e.value = price.Mul(e.alpha).Add(e.value.Mul(decimal.NewFromInt(1).Sub(e.alpha))).Round(roundPrecision)
	return e.value
}

// Value возвращает текущее значение. Смысл имеет только при Ready() == true.
func (e *EMA) Value() decimal.Decimal {
	return e.value
}

// Ready сообщает, было ли хотя бы одно обновление.
func (e *EMA) Ready() bool {
	return e.ready
}
