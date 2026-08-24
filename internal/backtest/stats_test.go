package backtest

import (
	"testing"

	"github.com/shopspring/decimal"
)

func d(s string) decimal.Decimal {
	v, err := decimal.NewFromString(s)
	if err != nil {
		panic(err)
	}
	return v
}

func TestComputeStats_KnownTrades(t *testing.T) {
	// Старт 1000. Сделки по очереди: +200, -100, +150, -50.
	// Эквити: 1000 -> 1200 (пик) -> 1100 -> 1250 (новый пик) -> 1200.
	// Просадки: (1200-1100)/1200=8.33%, (1250-1200)/1250=4%. Макс — 8.33%.
	trades := []Trade{
		{PnL: d("200"), RiskDollars: d("100")},  // R=2
		{PnL: d("-100"), RiskDollars: d("100")}, // R=-1
		{PnL: d("150"), RiskDollars: d("100")},  // R=1.5
		{PnL: d("-50"), RiskDollars: d("100")},  // R=-0.5
	}

	stats := ComputeStats(trades, d("1000"))

	if stats.TradeCount != 4 {
		t.Fatalf("ожидалось 4 сделки, получено %d", stats.TradeCount)
	}
	if stats.Wins != 2 || stats.Losses != 2 {
		t.Fatalf("ожидалось 2 победы/2 убытка, получено %d/%d", stats.Wins, stats.Losses)
	}
	if !stats.WinRatePct.Equal(d("50")) {
		t.Fatalf("ожидался winrate 50%%, получено %s", stats.WinRatePct)
	}
	// profit factor = (200+150)/(100+50) = 350/150 = 2.3333...
	wantPF := d("350").Div(d("150"))
	if !stats.ProfitFactor.Equal(wantPF) {
		t.Fatalf("ожидался profit factor %s, получено %s", wantPF, stats.ProfitFactor)
	}
	// средний R = (2 -1 +1.5 -0.5)/4 = 2/4 = 0.5
	if !stats.AvgRMultiple.Equal(d("0.5")) {
		t.Fatalf("ожидался средний R 0.5, получено %s", stats.AvgRMultiple)
	}
	if !stats.FinalEquity.Equal(d("1200")) {
		t.Fatalf("ожидалось финальное эквити 1200, получено %s", stats.FinalEquity)
	}
	wantReturn := d("20") // (1200-1000)/1000*100
	if !stats.TotalReturnPct.Equal(wantReturn) {
		t.Fatalf("ожидалась доходность %s%%, получено %s%%", wantReturn, stats.TotalReturnPct)
	}
	wantDD := d("100").Div(d("1200")).Mul(d("100")) // 8.3333...%
	assertCloseStats(t, "MaxDrawdownPct", stats.MaxDrawdownPct, wantDD)
}

func assertCloseStats(t *testing.T, name string, got, want decimal.Decimal) {
	t.Helper()
	diff := got.Sub(want).Abs()
	if diff.GreaterThan(d("0.0001")) {
		t.Errorf("%s: got %s, want %s", name, got, want)
	}
}

func TestComputeStats_NoTrades(t *testing.T) {
	stats := ComputeStats(nil, d("1000"))
	if stats.TradeCount != 0 {
		t.Fatalf("ожидалось 0 сделок, получено %d", stats.TradeCount)
	}
	if !stats.FinalEquity.Equal(d("1000")) {
		t.Fatalf("без сделок эквити не должно меняться, получено %s", stats.FinalEquity)
	}
}

func TestComputeStats_AllWinsNoProfitFactorDivideByZero(t *testing.T) {
	trades := []Trade{{PnL: d("100"), RiskDollars: d("50")}}
	stats := ComputeStats(trades, d("1000"))
	// Без убыточных сделок ProfitFactor остаётся нулевым (не определено),
	// а не паникует на делении на ноль.
	if !stats.ProfitFactor.IsZero() {
		t.Fatalf("без убытков ProfitFactor должен быть 0, получено %s", stats.ProfitFactor)
	}
}
