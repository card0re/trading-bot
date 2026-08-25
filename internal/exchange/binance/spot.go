package binance

import (
	"context"
	"fmt"
	"time"

	"trading-bot/internal/domain"

	spotapi "github.com/adshao/go-binance/v2"
)

// LoadHistoricalSpotCandles — то же самое, что LoadHistoricalCandles, но со
// спот-рынка, не фьючерсов. Нужен только для delta-neutral funding-carry
// (internal/carry): чтобы держать реальную спот+шорт-перп позицию, а не
// направленную ставку на перпе в одиночку, бэктест должен знать спот-цену,
// а не только цену перпа — иначе базис (расхождение спот/перп) на входе и
// выходе тихо остаётся неучтённым.
func LoadHistoricalSpotCandles(ctx context.Context, client *spotapi.Client, symbol, interval string, start, end time.Time) ([]domain.Candle, error) {
	const pageLimit = 1000

	var all []domain.Candle
	cursor := start.UnixMilli()
	endMs := end.UnixMilli()

	for cursor < endMs {
		klines, err := client.NewKlinesService().
			Symbol(symbol).
			Interval(interval).
			StartTime(cursor).
			EndTime(endMs).
			Limit(pageLimit).
			Do(ctx)
		if err != nil {
			return nil, fmt.Errorf("запрос спот-истории %s (страница с %s): %w", symbol, time.UnixMilli(cursor), err)
		}
		if len(klines) == 0 {
			break
		}

		for _, k := range klines {
			open, high, low, closePrice, volume, _, err := parseOHLCV(
				ohlcv{k.Open, k.High, k.Low, k.Close, k.Volume, k.TakerBuyBaseAssetVolume})
			if err != nil {
				return nil, err
			}
			all = append(all, domain.Candle{
				Symbol:    symbol,
				OpenTime:  time.UnixMilli(k.OpenTime),
				CloseTime: time.UnixMilli(k.CloseTime),
				Open:      open,
				High:      high,
				Low:       low,
				Close:     closePrice,
				Volume:    volume,
				IsClosed:  true,
			})
		}

		last := klines[len(klines)-1]
		if last.CloseTime <= cursor {
			break
		}
		cursor = last.CloseTime + 1
	}

	return all, nil
}
