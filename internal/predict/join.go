package predict

import (
	"math"
	"strings"
)

// RecRow — строка CSV от cmd/predictrec. T и WindowStart — unix ms.
type RecRow struct {
	T, WindowStart int64
	Slug, Side     string // Side: UP / DOWN / RESULT
	Ask            float64
	HasAsk         bool
	Depth          float64 // USDT на лучшем ask
	Spot           float64
	Winner         string // только в RESULT: "Up"/"Down"
}

// Opp — момент, когда дальний исход (ставка на разворот) продавался дёшево.
type Opp struct {
	T                        int64
	Window, MinLeft          int
	Z, Ask, ImpliedP, ModelP float64
	EV                       float64 // ожидание на $1 по модели: ModelP·(1−Fee)/Ask − 1
	HourGroup                int     // см. HourGroup; ModelP уже с поправкой на неё
	Won, Resolved            bool
}

// MaxAsk — дороже этого «дальняя» доля уже не лотерея (кэф < ~6.5x).
// 0.15, а не 0.05: вживую до ≤0.05 дальняя сторона дешевеет почти только в
// последнюю минуту, а интересная зона — середина окна по $0.05–0.15.
const MaxAsk = 0.15

// WindowMinutes — длина окна по слагу ("...-5m"/"...-15m"), 0 если не распознан.
func WindowMinutes(slug string) int {
	switch {
	case strings.HasSuffix(slug, "-5m"):
		return 5
	case strings.HasSuffix(slug, "-15m"):
		return 15
	}
	return 0
}

// Opportunities находит в записи площадки моменты «дальняя сторона по ≤ MaxAsk,
// на лучшем ask ≥ $1» и оценивает их по таблице реальной вероятности.
// bars должны покрывать время записи (плюс 60 минут до него — для σ).
// lifts[окно][группа часа] — поправка на время суток (nil — без поправки).
func Opportunities(rows []RecRow, bars []Bar, table map[Key]*Cell, lifts map[int]map[int]*Lift) []Opp {
	idx := make(map[int64]int, len(bars))
	for i, b := range bars {
		idx[b.T] = i
	}
	type win struct {
		slug  string
		start int64
	}
	winners := map[win]string{}
	for _, r := range rows {
		if r.Side == "RESULT" {
			winners[win{r.Slug, r.WindowStart}] = r.Winner
		}
	}

	var out []Opp
	for _, r := range rows {
		n := WindowMinutes(r.Slug)
		if n == 0 || r.Side == "RESULT" || !r.HasAsk || r.Ask > MaxAsk || r.Depth < 1 {
			continue
		}
		oi, ok := idx[r.WindowStart/1000]
		if !ok {
			continue
		}
		open := bars[oi].O
		far := ""
		switch {
		case r.Spot < open:
			far = "UP"
		case r.Spot > open:
			far = "DOWN"
		}
		if r.Side != far {
			continue
		}
		left := float64(r.WindowStart+int64(n)*60_000-r.T) / 60_000
		// shortcut: модель знает только целые минуты — берём floor (меньше времени =
		// меньше шанс разворота, оценка консервативнее). Меньше минуты — пропуск.
		minLeft := int(left)
		if minLeft < 1 {
			continue
		}
		si, ok := idx[(r.T/1000)/60*60-60] // последняя закрытая минута
		if !ok {
			continue
		}
		sigma, ok := Sigma(bars, si)
		if !ok || sigma == 0 {
			continue
		}
		z := math.Log(r.Spot/open) / (sigma * math.Sqrt(left))
		c := table[Key{n, minLeft, Bucket(z)}]
		if c == nil || c.N == 0 {
			continue
		}
		w, resolved := winners[win{r.Slug, r.WindowStart}]
		hg := HourGroup(Sample{T: r.T / 1000})
		p := AdjustP(c.P(), lifts[n][hg])
		out = append(out, Opp{
			T: r.T, Window: n, MinLeft: minLeft, Z: z, Ask: r.Ask, ImpliedP: r.Ask, ModelP: p,
			EV:        p*(1-Fee)/r.Ask - 1,
			HourGroup: hg,
			Resolved:  resolved,
			Won:       resolved && strings.EqualFold(w, far),
		})
	}
	return out
}
