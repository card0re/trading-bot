package predict

import "time"

// Lift — сколько разворотов было в группе (час, день недели…) против
// ожидаемого по общей таблице. Ratio > 1 — в эти часы разворачивается чаще
// среднего при той же дистанции и том же времени до конца.
type Lift struct {
	N, Rev int
	Exp    float64 // Σ p ячейки по точкам группы
}

func (l *Lift) Ratio() float64 {
	if l.Exp == 0 {
		return 0
	}
	return float64(l.Rev) / l.Exp
}

// RatioCI — 95% интервал Ratio: Wilson для доли разворотов, пересчитанный в разы.
func (l *Lift) RatioCI() (lo, hi float64) {
	c := Cell{N: l.N, Rev: l.Rev}
	plo, phi := c.Wilson()
	if l.Exp == 0 {
		return 0, 0
	}
	k := float64(l.N) / l.Exp
	return plo * k, phi * k
}

// InZone — «лотерейная» зона: уход ≥ 2σ и до конца окна ≥ 2 минут (там K* ~20–150).
func InZone(x Sample) bool { return x.MinLeft >= 2 && Bucket(x.Z) >= 4 }

func HourUTC(x Sample) int    { return time.Unix(x.T, 0).UTC().Hour() }
func WeekdayUTC(x Sample) int { return int(time.Unix(x.T, 0).UTC().Weekday()) }

// LiftBy группирует точки зоны и сравнивает развороты с ожиданием по table.
func LiftBy(s []Sample, table map[Key]*Cell, group func(Sample) int, in func(Sample) bool) map[int]*Lift {
	out := map[int]*Lift{}
	for _, x := range s {
		if !in(x) {
			continue
		}
		c := table[Key{x.Window, x.MinLeft, Bucket(x.Z)}]
		if c == nil {
			continue
		}
		g := group(x)
		l := out[g]
		if l == nil {
			l = &Lift{}
			out[g] = l
		}
		l.N++
		l.Exp += c.P()
		if x.Reversal {
			l.Rev++
		}
	}
	return out
}

// HourGroup — грубая группа часа: 1 — 12–15 UTC (развороты чаще), 2 — 15–20 UTC
// (реже), 0 — остальное. По 24 часам поправка переобучается на шум.
func HourGroup(x Sample) int {
	switch h := HourUTC(x); {
	case h >= 12 && h < 15:
		return 1
	case h >= 15 && h < 20:
		return 2
	}
	return 0
}
