package rotation

import (
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

func TestSimulate_ProfitsFromPersistentMomentum(t *testing.T) {
	// A steadily rises 10%/day, B steadily falls 10%/day — ранжирование по
	// вчерашней доходности всегда угадывает направление сегодняшнего дня.
	closes := map[string][]DailyBar{
		"A": mkBars(100, 110, 121, 133.1),
		"B": mkBars(100, 90, 81, 72.9),
	}
	p := Params{LookbackDays: 1, TopK: 1, RebalanceEvery: 1}
	st := Simulate(closes, []string{"A", "B"}, 1, 4, p,
		decimal.NewFromInt(10000), decimal.NewFromFloat(0.0005), decimal.NewFromFloat(0.0002))

	if st.NumRebalances != 3 {
		t.Fatalf("ожидалось 3 ребалансировки, получено %d", st.NumRebalances)
	}
	if !st.FinalEquity.GreaterThan(st.StartEquity) {
		t.Fatalf("устойчивый моментум по обеим ногам должен давать прибыль: start=%s final=%s",
			st.StartEquity, st.FinalEquity)
	}
}

func TestSimulate_FlatMarketBleedsOnFeesAlone(t *testing.T) {
	// Цены не двигаются — вся просадка эквити должна объясняться комиссией
	// и проскальзыванием на каждой ребалансировке, а не ошибкой в цене входа.
	closes := map[string][]DailyBar{
		"A": mkBars(100, 100, 100, 100),
		"B": mkBars(100, 100, 100, 100),
	}
	p := Params{LookbackDays: 1, TopK: 1, RebalanceEvery: 1}
	st := Simulate(closes, []string{"A", "B"}, 1, 4, p,
		decimal.NewFromInt(10000), decimal.NewFromFloat(0.0005), decimal.NewFromFloat(0.0002))

	if !st.FinalEquity.LessThan(st.StartEquity) {
		t.Fatalf("плоский рынок должен терять на комиссиях, получено start=%s final=%s",
			st.StartEquity, st.FinalEquity)
	}
}

func TestSimulate_SingleSymbolNeverTrades(t *testing.T) {
	// topK требует минимум 2*topK кандидатов (лонг+шорт) — с одним символом
	// k схлопывается в 0, сделок быть не должно (и не должно быть паники на
	// делении при notionalPerLeg).
	closes := map[string][]DailyBar{"A": mkBars(100, 110, 121)}
	p := Params{LookbackDays: 1, TopK: 1, RebalanceEvery: 1}
	st := Simulate(closes, []string{"A"}, 1, 3, p,
		decimal.NewFromInt(10000), decimal.NewFromFloat(0.0005), decimal.NewFromFloat(0.0002))

	if st.NumRebalances != 0 {
		t.Fatalf("одного символа недостаточно для лонг+шорт, ожидалось 0 ребалансировок, получено %d", st.NumRebalances)
	}
	if !st.FinalEquity.Equal(st.StartEquity) {
		t.Fatalf("без сделок эквити не должен меняться: start=%s final=%s", st.StartEquity, st.FinalEquity)
	}
}
