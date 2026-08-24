package pairs

import (
	"math"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func mkBars(closes ...float64) []DailyBar {
	bars := make([]DailyBar, len(closes))
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i, c := range closes {
		bars[i] = DailyBar{T: base.AddDate(0, 0, i), Close: decimal.NewFromFloat(c)}
	}
	return bars
}

func TestSelectPairs_PicksMostCorrelated(t *testing.T) {
	// A и B двигаются идентично (корреляция ~1), C — независимо в разнобой.
	daily := map[string][]DailyBar{
		"A": mkBars(100, 105, 100, 108, 102, 110),
		"B": mkBars(50, 52.5, 50, 54, 51, 55),
		"C": mkBars(10, 9, 11, 8, 12, 7),
	}
	pairs := SelectPairs(daily, []string{"A", "B", "C"}, 0, 6, 1)
	if len(pairs) != 1 {
		t.Fatalf("ожидалась 1 пара, получено %d", len(pairs))
	}
	got := pairs[0]
	if !((got.A == "A" && got.B == "B") || (got.A == "B" && got.B == "A")) {
		t.Fatalf("ожидалась пара A/B (самая коррелированная), получено %s/%s", got.A, got.B)
	}
	if got.Correlation < 0.9 {
		t.Fatalf("A и B двигаются идентично, ожидалась корреляция >=0.9, получено %.3f", got.Correlation)
	}
}

func TestSimulate_ProfitsFromSpreadReversion(t *testing.T) {
	// A и B держатся в постоянном отношении (спред=0) первые 30 дней (для
	// прогрева окна), затем A резко подскакивает на день 30 (спред уходит в
	// экстремум) и возвращается обратно к прежнему отношению на день 32 —
	// ровно та форма, на которую рассчитан вход/выход по z-score.
	closesA := make([]float64, 0, 40)
	closesB := make([]float64, 0, 40)
	for i := 0; i < 30; i++ {
		closesA = append(closesA, 100)
		closesB = append(closesB, 50)
	}
	closesA = append(closesA, 130, 130, 100, 100, 100) // день 30: скачок, день 32: вернулся
	closesB = append(closesB, 50, 50, 50, 50, 50)

	daily := map[string][]DailyBar{"A": mkBars(closesA...), "B": mkBars(closesB...)}
	pairs := []Pair{{A: "A", B: "B", Correlation: 1}}
	p := Params{LookbackDays: 20, EntryZ: 1.5, ExitZ: 0.3, StopZ: 10, MaxHoldDays: 10, MaxConcurrent: 1}

	st := Simulate(daily, pairs, 20, len(closesA), p,
		decimal.NewFromInt(10000), decimal.NewFromFloat(0.0005), decimal.NewFromFloat(0.0002))

	if st.NumTrades == 0 {
		t.Fatal("ожидался хотя бы один вход на явном скачке спреда, сделок 0")
	}
	if !st.FinalEquity.GreaterThan(st.StartEquity) {
		t.Fatalf("схождение спреда обратно к среднему должно дать прибыль: start=%s final=%s",
			st.StartEquity, st.FinalEquity)
	}
}

func TestSimulate_NoSignalWhenSpreadFlat(t *testing.T) {
	closes := make([]float64, 40)
	for i := range closes {
		closes[i] = 100
	}
	daily := map[string][]DailyBar{"A": mkBars(closes...), "B": mkBars(closes...)}
	pairs := []Pair{{A: "A", B: "B", Correlation: 1}}
	p := Params{LookbackDays: 20, EntryZ: 1.5, ExitZ: 0.3, StopZ: 10, MaxHoldDays: 10, MaxConcurrent: 1}

	st := Simulate(daily, pairs, 20, 40, p,
		decimal.NewFromInt(10000), decimal.NewFromFloat(0.0005), decimal.NewFromFloat(0.0002))

	if st.NumTrades != 0 {
		t.Fatalf("плоский спред (стандартное отклонение 0) не должен давать сигналов, сделок %d", st.NumTrades)
	}
	if !st.FinalEquity.Equal(st.StartEquity) {
		t.Fatalf("без сделок эквити не должен меняться: start=%s final=%s", st.StartEquity, st.FinalEquity)
	}
}

func TestRollingZ_NaNBeforeWarmup(t *testing.T) {
	a := mkBars(100, 101, 102, 103, 104)
	b := mkBars(50, 50, 50, 50, 50)
	z := rollingZ(a, b, 3)
	for i := 0; i < 3; i++ {
		if !math.IsNaN(z[i]) {
			t.Fatalf("z[%d] должен быть NaN до прогрева окна, получено %v", i, z[i])
		}
	}
	if math.IsNaN(z[3]) || math.IsNaN(z[4]) {
		t.Fatalf("z после прогрева не должен быть NaN: z[3]=%v z[4]=%v", z[3], z[4])
	}
}
