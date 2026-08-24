package backtest

import (
	"context"
	"time"

	exchange "trading-bot/internal/exchange/binance"

	"github.com/shopspring/decimal"
)

// FundingReplay реализует domain.FundingRateSource поверх исторических
// точек финансирования, проигрываемых синхронно с симулируемым временем —
// как SimExecutor для позиций и risk.Manager.SetClock для дневного лимита,
// только для ставки финансирования.
type FundingReplay struct {
	points  []exchange.FundingPoint // отсортированы по времени по возрастанию
	idx     int                     // индекс последней точки, известной на текущий момент
	current decimal.Decimal
}

// NewFundingReplay создаёт реплей поверх отсортированной по времени истории.
func NewFundingReplay(points []exchange.FundingPoint) *FundingReplay {
	return &FundingReplay{points: points, idx: -1}
}

// Advance продвигает "текущее время" реплея — вызывается драйвером
// бэктеста перед каждой свечой, синхронно с проигрыванием истории.
func (f *FundingReplay) Advance(t time.Time) {
	for f.idx+1 < len(f.points) && !f.points[f.idx+1].Time.After(t) {
		f.idx++
	}
	if f.idx >= 0 {
		f.current = f.points[f.idx].Rate
	}
}

func (f *FundingReplay) FundingRate(context.Context) (decimal.Decimal, error) {
	return f.current, nil
}
