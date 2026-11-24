package predict

import "math"

// AdjustP поправляет вероятность ячейки на коэффициент часа (Lift.Ratio).
// Без данных по часу — p как есть; результат не больше 1.
func AdjustP(p float64, l *Lift) float64 {
	if l == nil || l.Exp == 0 {
		return p
	}
	return math.Min(1, p*l.Ratio())
}

// LogLoss — средний лог-лосс прогноза p(x) разворота (меньше — точнее).
// Точки, где p вернул false, не считаются.
func LogLoss(s []Sample, p func(Sample) (float64, bool)) (float64, int) {
	var sum float64
	n := 0
	for _, x := range s {
		q, ok := p(x)
		if !ok {
			continue
		}
		q = math.Min(math.Max(q, 1e-4), 1-1e-4)
		if x.Reversal {
			sum -= math.Log(q)
		} else {
			sum -= math.Log(1 - q)
		}
		n++
	}
	if n == 0 {
		return 0, 0
	}
	return sum / float64(n), n
}
