package binance

import (
	"context"
	"fmt"
	"time"

	"github.com/adshao/go-binance/v2/futures"
	"github.com/shopspring/decimal"
)

// FundingPoint — одна историческая выплата финансирования (обычно раз в
// 8 часов). Rate — в долях (0.0001 = 0.01%), как отдаёт биржа.
type FundingPoint struct {
	Time time.Time
	Rate decimal.Decimal
}

// LoadFundingRateHistory тянет историю ставок финансирования за период,
// постранично — на всякий случай, хотя на 8-часовом шаге даже год истории
// укладывается в один запрос (лимит биржи — 1000 записей за раз).
func LoadFundingRateHistory(ctx context.Context, client *futures.Client, symbol string, start, end time.Time) ([]FundingPoint, error) {
	const pageLimit = 1000

	var all []FundingPoint
	cursor := start.UnixMilli()
	endMs := end.UnixMilli()

	for cursor < endMs {
		rates, err := client.NewFundingRateService().
			Symbol(symbol).
			StartTime(cursor).
			EndTime(endMs).
			Limit(pageLimit).
			Do(ctx)
		if err != nil {
			return nil, fmt.Errorf("запрос истории funding rate %s: %w", symbol, err)
		}
		if len(rates) == 0 {
			break
		}

		for _, r := range rates {
			rate, err := decimal.NewFromString(r.FundingRate)
			if err != nil {
				return nil, fmt.Errorf("разбор funding rate %q: %w", r.FundingRate, err)
			}
			all = append(all, FundingPoint{Time: time.UnixMilli(r.FundingTime), Rate: rate})
		}

		last := rates[len(rates)-1]
		if last.FundingTime <= cursor {
			break
		}
		cursor = last.FundingTime + 1
	}

	return all, nil
}

// LiveFundingRateSource реализует domain.FundingRateSource поверх
// premiumIndex — публичного REST-эндпоинта с текущей ставкой финансирования
// (без нужды копить историю, ключ не требуется).
type LiveFundingRateSource struct {
	client *futures.Client
	symbol string
}

// NewLiveFundingRateSource создаёт источник для одного символа.
func NewLiveFundingRateSource(client *futures.Client, symbol string) *LiveFundingRateSource {
	return &LiveFundingRateSource{client: client, symbol: symbol}
}

func (f *LiveFundingRateSource) FundingRate(ctx context.Context) (decimal.Decimal, error) {
	rows, err := f.client.NewPremiumIndexService().Symbol(f.symbol).Do(ctx)
	if err != nil {
		return decimal.Zero, fmt.Errorf("запрос funding rate %s: %w", f.symbol, err)
	}
	if len(rows) == 0 {
		return decimal.Zero, fmt.Errorf("пустой ответ premiumIndex для %s", f.symbol)
	}
	rate, err := decimal.NewFromString(rows[0].LastFundingRate)
	if err != nil {
		return decimal.Zero, fmt.Errorf("разбор funding rate %q: %w", rows[0].LastFundingRate, err)
	}
	return rate, nil
}
