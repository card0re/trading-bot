package carry

import (
	"testing"
	"time"

	"trading-bot/internal/domain"

	"github.com/shopspring/decimal"
)

func mkEvents(rates []float64, spot, perp float64) []FundingEvent {
	out := make([]FundingEvent, len(rates))
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	for i, r := range rates {
		out[i] = FundingEvent{
			Time:      base.Add(time.Duration(i) * 8 * time.Hour),
			Rate:      decimal.NewFromFloat(r),
			SpotPrice: decimal.NewFromFloat(spot),
			PerpPrice: decimal.NewFromFloat(perp),
		}
	}
	return out
}

func TestSimulate_CollectsFundingWhenRateFavorable(t *testing.T) {
	// Ставка 0.05%/8ч устойчиво — без учёта комиссий эквити должно расти.
	events := mkEvents([]float64{0.0005, 0.0005, 0.0005, 0.0005, 0.0005, 0.0005, 0.0005, 0.0005, 0.0005, 0.0005}, 100, 100)
	p := Params{Lookback: 1, EntryAnnualRate: decimal.NewFromInt(10), ExitAnnualRate: decimal.Zero}
	stats := Simulate(events, p, decimal.NewFromInt(10000))

	if !stats.FundingCollected.IsPositive() {
		t.Fatalf("ожидался положительный собранный funding, получено %s", stats.FundingCollected)
	}
	if stats.FinalEquity.LessThanOrEqual(stats.StartEquity) {
		t.Fatalf("эквити должно было вырасти: %s -> %s", stats.StartEquity, stats.FinalEquity)
	}
}

func TestSimulate_ExitsWhenRateDrops(t *testing.T) {
	// Высокая ставка первые 5 периодов, затем ноль — должны войти и выйти ровно раз.
	rates := []float64{0.001, 0.001, 0.001, 0.001, 0.001, 0, 0, 0, 0, 0}
	events := mkEvents(rates, 100, 100)
	p := Params{Lookback: 1, EntryAnnualRate: decimal.NewFromInt(10), ExitAnnualRate: decimal.NewFromInt(5)}
	stats := Simulate(events, p, decimal.NewFromInt(10000))

	if stats.NumTrades != 1 {
		t.Fatalf("ожидалась ровно 1 сделка (вход+выход), получено %d", stats.NumTrades)
	}
}

func TestSimulate_FeesEatSmallFunding(t *testing.T) {
	// Ставка чуть выше порога входа, но комиссии на вход+выход должны съесть
	// весь собранный funding при коротком удержании — итог не должен быть
	// прибыльнее сырого funding без комиссий.
	events := mkEvents([]float64{0.0002, 0.0002, 0}, 100, 100)
	withFees := Params{Lookback: 1, EntryAnnualRate: decimal.NewFromInt(10), ExitAnnualRate: decimal.NewFromInt(5),
		SpotFeeRate: decimal.NewFromFloat(0.001), PerpFeeRate: decimal.NewFromFloat(0.0005)}
	noFees := Params{Lookback: 1, EntryAnnualRate: decimal.NewFromInt(10), ExitAnnualRate: decimal.NewFromInt(5)}

	start := decimal.NewFromInt(10000)
	withFeesStats := Simulate(events, withFees, start)
	noFeesStats := Simulate(events, noFees, start)

	if !withFeesStats.FinalEquity.LessThan(noFeesStats.FinalEquity) {
		t.Fatalf("комиссии должны были снизить итог: с комиссией %s, без %s",
			withFeesStats.FinalEquity, noFeesStats.FinalEquity)
	}
}

func TestBuildEvents_SkipsPointsBeforePriceHistory(t *testing.T) {
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	fundingTimes := []time.Time{base.Add(-1 * time.Hour), base.Add(1 * time.Hour)}
	fundingRates := []decimal.Decimal{decimal.NewFromFloat(0.001), decimal.NewFromFloat(0.001)}
	priceHistory := []domain.Candle{{CloseTime: base, Close: decimal.NewFromInt(100)}}

	events := BuildEvents(fundingTimes, fundingRates, priceHistory, priceHistory)

	if len(events) != 1 {
		t.Fatalf("ожидалось 1 событие (первая funding-точка раньше начала истории цены), получено %d", len(events))
	}
}
