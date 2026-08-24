package binance

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"trading-bot/internal/domain"

	"github.com/adshao/go-binance/v2/futures"
	"github.com/jpillora/backoff"
	"github.com/shopspring/decimal"
)

// StreamUserData держит приватный канал живым: продлевает listenKey и
// переподключается при обрыве.
//
// listenKey протухает через 60 минут без продления. Раньше keepalive не было
// вовсе — через час бот переставал получать события об исполнении и навсегда
// застревал в состоянии «позиция открыта».
func StreamUserData(
	ctx context.Context,
	client *futures.Client,
	keepaliveEvery time.Duration,
	out chan<- domain.OrderEvent,
) <-chan struct{} {
	finished := make(chan struct{})

	go func() {
		defer close(finished)

		b := &backoff.Backoff{Min: time.Second, Max: 2 * time.Minute, Factor: 2, Jitter: true}

		for ctx.Err() == nil {
			listenKey, err := client.NewStartUserStreamService().Do(ctx)
			if err != nil {
				d := b.Duration()
				slog.Error(fmt.Sprintf("❌ Получение listenKey: %v (повтор через %s)", err, d.Round(time.Second)))
				if !sleepCtx(ctx, d) {
					return
				}
				continue
			}

			handler := func(event *futures.WsUserDataEvent) {
				if event.Event != futures.UserDataEventTypeOrderTradeUpdate {
					return
				}
				u := event.OrderTradeUpdate

				// Ошибки парсинга не фатальны: тип и статус ордера важнее цены,
				// а состояние позиции всё равно сверяется с биржей.
				avg, _ := decimal.NewFromString(u.AveragePrice)
				filled, _ := decimal.NewFromString(u.AccumulatedFilledQty)
				realizedPnL, _ := decimal.NewFromString(u.RealizedPnL)

				select {
				case out <- domain.OrderEvent{
					Symbol:        u.Symbol,
					ClientOrderID: u.ClientOrderID,
					Type:          string(u.Type),
					Status:        string(u.Status),
					Side:          string(u.Side),
					AvgPrice:      avg,
					FilledQty:     filled,
					ReduceOnly:    u.IsReduceOnly,
					RealizedPnL:   realizedPnL,
				}:
				case <-ctx.Done():
				}
			}

			errHandler := func(err error) {
				slog.Error(fmt.Sprintf("❌ Ошибка приватного потока: %v", err))
			}

			doneC, stopC, err := futures.WsUserDataServe(listenKey, handler, errHandler)
			if err != nil {
				d := b.Duration()
				slog.Error(fmt.Sprintf("❌ Подключение приватного потока: %v (повтор через %s)", err, d.Round(time.Second)))
				if !sleepCtx(ctx, d) {
					return
				}
				continue
			}

			slog.Info("🔐 Приватный поток (User Data) подключен")
			b.Reset()

			// Keepalive живёт ровно столько, сколько текущее соединение.
			streamCtx, cancelKeepalive := context.WithCancel(ctx)
			go keepAlive(streamCtx, client, listenKey, keepaliveEvery)

			stopped := waitStream(ctx, doneC, stopC)
			cancelKeepalive()

			// listenKey привязан к соединению — закрываем его, чтобы не копить
			// висящие ключи на аккаунте (их лимит ограничен).
			closeListenKey(client, listenKey)

			if stopped {
				return
			}
			slog.Info("🔌 Приватный поток разорван, переподключаюсь...")
		}
	}()

	return finished
}

func keepAlive(ctx context.Context, client *futures.Client, listenKey string, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			err := client.NewKeepaliveUserStreamService().ListenKey(listenKey).Do(ctx)
			if err != nil {
				// Поток переподключится и возьмёт новый ключ.
				slog.Warn(fmt.Sprintf("⚠️  Продление listenKey не удалось: %v", err))
				continue
			}
			slog.Info("🔑 listenKey продлён")
		}
	}
}

func closeListenKey(client *futures.Client, listenKey string) {
	// Отдельный контекст: основной к этому моменту уже может быть отменён.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.NewCloseUserStreamService().ListenKey(listenKey).Do(ctx); err != nil {
		slog.Warn(fmt.Sprintf("⚠️  Закрытие listenKey: %v", err))
	}
}
