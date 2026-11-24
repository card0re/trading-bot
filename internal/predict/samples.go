// Package predict — анализ up/down-прогнозов (Binance Prediction): как часто
// цена, ушедшая внутри 5m/15m-окна от цены открытия, всё же закрывает окно по
// другую сторону. Без сети: на вход — минутные бары.
package predict

import "math"

// Bar — минутная свеча: T — время открытия (unix-секунды), O/C — open/close.
type Bar struct {
	T    int64
	O, C float64
}

// Sample — одна точка входа: конец минуты k внутри окна длиной Window минут.
type Sample struct {
	T               int64 // время точки входа (конец минуты k), unix-секунды
	Window, MinLeft int
	Z, Sigma        float64
	Reversal, Tie   bool // Tie — финал ровно на open (у площадки 50/50, для нас проигрыш)
}

const sigmaBars = 60

// Sigma — std минутных лог-доходностей за 60 баров, заканчивающихся на i
// (включительно). false, если перед i нет 60 непрерывных баров.
func Sigma(bars []Bar, i int) (float64, bool) {
	if i < sigmaBars || bars[i].T-bars[i-sigmaBars].T != sigmaBars*60 {
		return 0, false
	}
	var sum, sq float64
	for j := i - sigmaBars + 1; j <= i; j++ {
		r := math.Log(bars[j].C / bars[j-1].C)
		sum += r
		sq += r * r
	}
	mean := sum / sigmaBars
	return math.Sqrt(math.Max(sq/sigmaBars-mean*mean, 0)), true
}

// Samples размечает все полные окна длиной window минут, выровненные по UTC.
// bars должны быть отсортированы по T без дублей; окна с пропусками пропускаются.
func Samples(bars []Bar, window int) []Sample {
	idx := make(map[int64]int, len(bars))
	for i, b := range bars {
		idx[b.T] = i
	}
	step := int64(window) * 60
	var out []Sample
	at := make([]int, window)
	for _, b := range bars {
		if b.T%step != 0 {
			continue
		}
		complete := true
		for k := 0; k < window; k++ {
			i, ok := idx[b.T+int64(k)*60]
			if !ok {
				complete = false
				break
			}
			at[k] = i
		}
		if !complete {
			continue
		}
		open, final := b.O, bars[at[window-1]].C
		for k := 1; k < window; k++ {
			i := at[k-1]
			price := bars[i].C
			sigma, ok := Sigma(bars, i)
			if !ok || sigma == 0 || price == open {
				continue
			}
			left := window - k
			z := math.Log(price/open) / (sigma * math.Sqrt(float64(left)))
			out = append(out, Sample{
				T:        b.T + int64(k)*60,
				Window:   window,
				MinLeft:  left,
				Z:        z,
				Sigma:    sigma,
				Reversal: (price < open && final > open) || (price > open && final < open),
				Tie:      final == open,
			})
		}
	}
	return out
}
