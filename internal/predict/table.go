package predict

import (
	"math"
	"math/rand/v2"
	"sort"
)

// Fee — комиссия площадки (feeRateBps=200). База начисления не выяснена,
// берём худший случай: 2% с выплаты.
const Fee = 0.02

// ZEdges — границы корзин |z|.
var ZEdges = []float64{0, .5, 1, 1.5, 2, 2.5, 3, 3.5, 4, 5, math.Inf(1)}

// Bucket — номер корзины |z|.
func Bucket(z float64) int {
	a := math.Abs(z)
	for i := len(ZEdges) - 2; i > 0; i-- {
		if a >= ZEdges[i] {
			return i
		}
	}
	return 0
}

// Key — ячейка таблицы: окно × минут до конца × корзина |z|.
type Key struct{ Window, MinLeft, Bucket int }

// Cell — статистика ячейки. GaussP — средняя «честная» вероятность разворота
// по гауссовой модели Φ(−|z|), то, что площадка заложила бы без толстых хвостов.
type Cell struct {
	N, Rev, Ties int
	GaussP       float64
}

func (c *Cell) P() float64 {
	if c.N == 0 {
		return 0
	}
	return float64(c.Rev) / float64(c.N)
}

// Wilson — 95% доверительный интервал для P.
func (c *Cell) Wilson() (lo, hi float64) {
	if c.N == 0 {
		return 0, 1
	}
	const z2 = 1.96 * 1.96
	n, p := float64(c.N), c.P()
	den := 1 + z2/n
	mid := (p + z2/(2*n)) / den
	half := 1.96 * math.Sqrt(p*(1-p)/n+z2/(4*n*n)) / den
	return math.Max(0, mid-half), math.Min(1, mid+half)
}

// Breakeven — коэффициент, при котором ставка на разворот выходит в ноль.
func (c *Cell) Breakeven() float64 {
	if c.P() == 0 {
		return math.Inf(1)
	}
	return 1 / ((1 - Fee) * c.P())
}

func phiTail(z float64) float64 { return 0.5 * math.Erfc(math.Abs(z)/math.Sqrt2) }

// Tabulate сводит точки входа в ячейки.
func Tabulate(s []Sample) map[Key]*Cell {
	t := make(map[Key]*Cell)
	for _, x := range s {
		k := Key{x.Window, x.MinLeft, Bucket(x.Z)}
		c := t[k]
		if c == nil {
			c = &Cell{}
			t[k] = c
		}
		c.N++
		if x.Reversal {
			c.Rev++
		}
		if x.Tie {
			c.Ties++
		}
		c.GaussP += phiTail(x.Z) // пока сумма, делим ниже
	}
	for _, c := range t {
		c.GaussP /= float64(c.N)
	}
	return t
}

// BankrollStats — итог Монте-Карло ставок по $1.
type BankrollStats struct {
	RuinProb, MedianFinal                 float64
	MedianMaxLoseStreak, P95MaxLoseStreak int
}

// SimulateBankroll: runs прогонов по bets ставок в $1 с вероятностью выигрыша p;
// выигрыш возвращает odds·(1−Fee) (ставка внутри). Разорение — банк < $1.
func SimulateBankroll(p, odds float64, bank, bets, runs int, seed int64) BankrollStats {
	rng := rand.New(rand.NewPCG(uint64(seed), 0))
	finals := make([]float64, runs)
	streaks := make([]int, runs)
	ruined := 0
	for r := range runs {
		b := float64(bank)
		cur, worst := 0, 0
		for range bets {
			if b < 1 {
				break
			}
			b--
			if rng.Float64() < p {
				b += odds * (1 - Fee)
				cur = 0
			} else if cur++; cur > worst {
				worst = cur
			}
		}
		if b < 1 {
			ruined++
		}
		finals[r], streaks[r] = b, worst
	}
	sort.Float64s(finals)
	sort.Ints(streaks)
	return BankrollStats{
		RuinProb:            float64(ruined) / float64(runs),
		MedianFinal:         finals[runs/2],
		MedianMaxLoseStreak: streaks[runs/2],
		P95MaxLoseStreak:    streaks[runs*95/100],
	}
}
