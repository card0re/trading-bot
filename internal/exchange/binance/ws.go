package binance

import (
	"context"
	"fmt"
	"log"
	"time"

	"trading-bot/internal/domain"

	"github.com/adshao/go-binance/v2/futures"
	"github.com/jpillora/backoff"
	"github.com/shopspring/decimal"
)

// StreamKlines держит поток свечей живым до отмены ctx: при обрыве соединения
// переподключается с экспоненциальной задержкой. Без этого обрыв означал бы
// «бот жив, но слеп» — с открытой позицией это самый опасный сценарий.
//
// Возвращает канал, который закрывается, когда поток окончательно остановлен.
func StreamKlines(ctx context.Context, symbol, interval string, out chan<- domain.Candle) <-chan struct{} {
	finished := make(chan struct{})

	go func() {
		defer close(finished)

		b := &backoff.Backoff{Min: time.Second, Max: 2 * time.Minute, Factor: 2, Jitter: true}

		for ctx.Err() == nil {
			handler := func(event *futures.WsKlineEvent) {
				candle, err := toCandle(event)
				if err != nil {
					log.Printf("⚠️  Свеча %s пропущена: %v", symbol, err)
					return
				}
				// Неблокирующая отправка: обработчик выполняется в горутине
				// чтения сокета, и залипание здесь остановило бы весь поток.
				// Свечу лучше потерять, чем подвесить соединение.
				select {
				case out <- candle:
				default:
					log.Printf("⚠️  Канал свечей переполнен, тик %s отброшен", symbol)
				}
			}

			errHandler := func(err error) {
				log.Printf("❌ Ошибка потока свечей %s: %v", symbol, err)
			}

			doneC, stopC, err := futures.WsKlineServe(symbol, interval, handler, errHandler)
			if err != nil {
				d := b.Duration()
				log.Printf("❌ Не удалось подключиться к потоку свечей: %v (повтор через %s)", err, d.Round(time.Second))
				if !sleepCtx(ctx, d) {
					return
				}
				continue
			}

			log.Printf("📡 Поток свечей %s (%s) подключен", symbol, interval)
			b.Reset()

			if waitStream(ctx, doneC, stopC) {
				return // остановка по ctx
			}
			log.Printf("🔌 Поток свечей %s разорван, переподключаюсь...", symbol)
		}
	}()

	return finished
}

// waitStream ждёт либо отмены контекста, либо обрыва соединения.
// Возвращает true, если остановка инициирована нами.
//
// stopC закрывается клиентом, doneC закрывает библиотека — попытка записи
// в doneC заблокировала бы вызывающую горутину навсегда.
func waitStream(ctx context.Context, doneC, stopC chan struct{}) bool {
	select {
	case <-ctx.Done():
		close(stopC)
		<-doneC // дожидаемся фактического закрытия сокета
		return true
	case <-doneC:
		return false
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// ohlcv — сырые строковые поля свечи, общие для REST и WS.
type ohlcv struct {
	open, high, low, closePrice, volume string
}

// parseOHLCV разбирает цены в decimal. Ошибка парсинга — это повод пропустить
// свечу, а не торговать по нулевой цене: прежний fmt.Sscanf молча оставлял 0.
func parseOHLCV(raw ohlcv) (o, h, l, c, v decimal.Decimal, err error) {
	fields := []struct {
		name string
		src  string
		dst  *decimal.Decimal
	}{
		{"open", raw.open, &o},
		{"high", raw.high, &h},
		{"low", raw.low, &l},
		{"close", raw.closePrice, &c},
		{"volume", raw.volume, &v},
	}
	for _, f := range fields {
		parsed, perr := decimal.NewFromString(f.src)
		if perr != nil {
			return o, h, l, c, v, fmt.Errorf("разбор %s %q: %w", f.name, f.src, perr)
		}
		*f.dst = parsed
	}
	return o, h, l, c, v, nil
}

func toCandle(event *futures.WsKlineEvent) (domain.Candle, error) {
	k := event.Kline

	open, high, low, closePrice, volume, err := parseOHLCV(ohlcv{k.Open, k.High, k.Low, k.Close, k.Volume})
	if err != nil {
		return domain.Candle{}, err
	}

	return domain.Candle{
		Symbol:    event.Symbol,
		OpenTime:  time.UnixMilli(k.StartTime),
		CloseTime: time.UnixMilli(k.EndTime),
		Open:      open,
		High:      high,
		Low:       low,
		Close:     closePrice,
		Volume:    volume,
		IsClosed:  k.IsFinal,
	}, nil
}

// LoadRecentCandles подтягивает историю через REST, чтобы стратегия начала
// работать сразу, а не через N свечей после запуска.
func LoadRecentCandles(ctx context.Context, client *futures.Client, symbol, interval string, limit int) ([]domain.Candle, error) {
	// Последняя свеча в ответе ещё формируется — берём на одну больше и режем.
	klines, err := client.NewKlinesService().
		Symbol(symbol).
		Interval(interval).
		Limit(limit + 1).
		Do(ctx)
	if err != nil {
		return nil, fmt.Errorf("запрос истории свечей: %w", err)
	}
	if len(klines) > 0 {
		klines = klines[:len(klines)-1]
	}

	candles := make([]domain.Candle, 0, len(klines))
	for _, k := range klines {
		c, err := klineToCandle(symbol, k)
		if err != nil {
			return nil, err
		}
		candles = append(candles, c)
	}
	return candles, nil
}

// klineToCandle разбирает один REST-kline в domain.Candle. Общий код между
// LoadRecentCandles и LoadHistoricalCandles.
func klineToCandle(symbol string, k *futures.Kline) (domain.Candle, error) {
	open, high, low, closePrice, volume, err := parseOHLCV(ohlcv{k.Open, k.High, k.Low, k.Close, k.Volume})
	if err != nil {
		return domain.Candle{}, err
	}
	return domain.Candle{
		Symbol:    symbol,
		OpenTime:  time.UnixMilli(k.OpenTime),
		CloseTime: time.UnixMilli(k.CloseTime),
		Open:      open,
		High:      high,
		Low:       low,
		Close:     closePrice,
		Volume:    volume,
		IsClosed:  true,
	}, nil
}

// LoadHistoricalCandles тянет историю за произвольный период через REST,
// постранично — Binance отдаёт не больше ~1500 свечей за один запрос.
// Используется бэктестом; для «последние N свечей прямо сейчас» проще и
// дешевле LoadRecentCandles.
func LoadHistoricalCandles(ctx context.Context, client *futures.Client, symbol, interval string, start, end time.Time) ([]domain.Candle, error) {
	const pageLimit = 1500

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
			return nil, fmt.Errorf("запрос истории %s (страница с %s): %w", symbol, time.UnixMilli(cursor), err)
		}
		if len(klines) == 0 {
			break
		}

		for _, k := range klines {
			c, err := klineToCandle(symbol, k)
			if err != nil {
				return nil, err
			}
			all = append(all, c)
		}

		last := klines[len(klines)-1]
		if last.CloseTime <= cursor {
			// Страховка от зацикливания, если биржа вдруг не продвинула курсор.
			break
		}
		cursor = last.CloseTime + 1
	}

	return all, nil
}
