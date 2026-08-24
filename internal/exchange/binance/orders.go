package binance

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"

	"trading-bot/internal/domain"

	"github.com/adshao/go-binance/v2/futures"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// OrderExecutor исполняет ордера по одному конкретному символу.
type OrderExecutor struct {
	client *futures.Client
	info   *SymbolInfo
}

func NewOrderExecutor(client *futures.Client, info *SymbolInfo) *OrderExecutor {
	return &OrderExecutor{client: client, info: info}
}

func (e *OrderExecutor) Symbol() string { return e.info.Symbol }

func (e *OrderExecutor) NormalizeQuantity(qty, refPrice decimal.Decimal) (decimal.Decimal, error) {
	return e.info.RoundQuantity(qty, refPrice)
}

func (e *OrderExecutor) NormalizePrice(price decimal.Decimal, down bool) decimal.Decimal {
	return e.info.RoundPrice(price, down)
}

// OpenLong покупает по рынку и возвращает фактическую среднюю цену входа.
// Стоп и тейк считаются именно от неё, а не от цены закрытия свечи:
// проскальзывание маркет-ордера иначе смещает весь риск-профиль сделки.
func (e *OrderExecutor) OpenLong(ctx context.Context, qty decimal.Decimal) (decimal.Decimal, error) {
	order, err := e.client.NewCreateOrderService().
		Symbol(e.info.Symbol).
		Side(futures.SideTypeBuy).
		Type(futures.OrderTypeMarket).
		Quantity(e.info.FormatQuantity(qty)).
		NewClientOrderID(clientOrderID("entry")).
		NewOrderResponseType(futures.NewOrderRespTypeRESULT).
		Do(ctx)
	if err != nil {
		return decimal.Zero, fmt.Errorf("вход в лонг: %w", err)
	}

	avg, err := decimal.NewFromString(order.AvgPrice)
	if err != nil || avg.IsZero() {
		// RESULT изредка приходит до расчёта средней цены — доспрашиваем позицию.
		pos, perr := e.Position(ctx)
		if perr != nil || pos.EntryPrice.IsZero() {
			return decimal.Zero, fmt.Errorf("не удалось определить цену входа: %w", errors.Join(err, perr))
		}
		avg = pos.EntryPrice
	}

	log.Printf("✅ ВХОД В ЛОНГ | %s | %s шт | средняя цена %s | ID %d",
		e.info.Symbol, e.info.FormatQuantity(qty), e.info.FormatPrice(avg), order.OrderID)
	return avg, nil
}

// OpenShort продаёт по рынку и возвращает фактическую среднюю цену входа.
// Симметрично OpenLong: стоп и тейк считаются от неё же.
func (e *OrderExecutor) OpenShort(ctx context.Context, qty decimal.Decimal) (decimal.Decimal, error) {
	order, err := e.client.NewCreateOrderService().
		Symbol(e.info.Symbol).
		Side(futures.SideTypeSell).
		Type(futures.OrderTypeMarket).
		Quantity(e.info.FormatQuantity(qty)).
		NewClientOrderID(clientOrderID("entry")).
		NewOrderResponseType(futures.NewOrderRespTypeRESULT).
		Do(ctx)
	if err != nil {
		return decimal.Zero, fmt.Errorf("вход в шорт: %w", err)
	}

	avg, err := decimal.NewFromString(order.AvgPrice)
	if err != nil || avg.IsZero() {
		pos, perr := e.Position(ctx)
		if perr != nil || pos.EntryPrice.IsZero() {
			return decimal.Zero, fmt.Errorf("не удалось определить цену входа: %w", errors.Join(err, perr))
		}
		avg = pos.EntryPrice
	}

	log.Printf("✅ ВХОД В ШОРТ | %s | %s шт | средняя цена %s | ID %d",
		e.info.Symbol, e.info.FormatQuantity(qty), e.info.FormatPrice(avg), order.OrderID)
	return avg, nil
}

// PlaceStopLoss и PlaceTakeProfit ставят условные ордера с closePosition=true:
// биржа сама закроет весь остаток позиции, поэтому количество не передаётся и
// не может разойтись с реальным размером после частичных исполнений. Это же
// гарантирует, что срабатывание не откроет противоположную позицию.
func (e *OrderExecutor) PlaceStopLoss(ctx context.Context, triggerPrice decimal.Decimal) error {
	return e.placeConditional(ctx, "STOP_MARKET", "sl", triggerPrice)
}

func (e *OrderExecutor) PlaceTakeProfit(ctx context.Context, triggerPrice decimal.Decimal) error {
	return e.placeConditional(ctx, "TAKE_PROFIT_MARKET", "tp", triggerPrice)
}

// placeConditional закрывающая сторона определяется знаком текущей живой
// позиции, а не запоминается локально — источник истины остаётся один
// (позиция на бирже), и это переживает рестарт процесса без потери контекста.
func (e *OrderExecutor) placeConditional(ctx context.Context, orderType, tag string, trigger decimal.Decimal) error {
	pos, err := e.Position(ctx)
	if err != nil {
		return fmt.Errorf("постановка %s: чтение позиции для определения стороны: %w", orderType, err)
	}
	if pos.IsFlat() {
		return fmt.Errorf("постановка %s: позиция уже нулевая", orderType)
	}
	side := futures.SideTypeSell // закрытие лонга
	if pos.Amount.IsNegative() {
		side = futures.SideTypeBuy // закрытие шорта
	}

	_, err = e.client.NewCreateAlgoOrderService().
		Symbol(e.info.Symbol).
		Side(side).
		AlgoType(futures.OrderAlgoType("CONDITIONAL")).
		Type(futures.AlgoOrderType(orderType)).
		TriggerPrice(e.info.FormatPrice(trigger)).
		ClosePosition(true).
		WorkingType(futures.WorkingTypeMarkPrice).
		PriceProtect(true).
		ClientAlgoId(clientOrderID(tag)).
		Do(ctx)
	if err != nil {
		return fmt.Errorf("постановка %s на %s: %w", orderType, e.info.FormatPrice(trigger), err)
	}

	log.Printf("🛡️  %s ВЫСТАВЛЕН | %s | триггер %s", orderType, e.info.Symbol, e.info.FormatPrice(trigger))
	return nil
}

// ClosePositionMarket закрывает позицию по рынку. Используется как аварийный
// выход, если защитные ордера поставить не удалось.
func (e *OrderExecutor) ClosePositionMarket(ctx context.Context) error {
	pos, err := e.Position(ctx)
	if err != nil {
		return fmt.Errorf("чтение позиции перед закрытием: %w", err)
	}
	if pos.IsFlat() {
		return nil
	}
	amount := pos.Amount

	side := futures.SideTypeSell
	if amount.IsNegative() {
		side = futures.SideTypeBuy
	}

	_, err = e.client.NewCreateOrderService().
		Symbol(e.info.Symbol).
		Side(side).
		Type(futures.OrderTypeMarket).
		Quantity(e.info.FormatQuantity(amount.Abs())).
		ReduceOnly(true).
		NewClientOrderID(clientOrderID("close")).
		Do(ctx)
	if err != nil {
		return fmt.Errorf("аварийное закрытие позиции: %w", err)
	}

	log.Printf("🚪 ПОЗИЦИЯ ЗАКРЫТА ПО РЫНКУ | %s | %s шт", e.info.Symbol, e.info.FormatQuantity(amount.Abs()))
	return nil
}

// Position возвращает текущую позицию по символу — источник истины о том,
// открыт ли бот. Локальный флаг ей всегда уступает.
func (e *OrderExecutor) Position(ctx context.Context) (domain.Position, error) {
	risks, err := e.client.NewGetPositionRiskService().Symbol(e.info.Symbol).Do(ctx)
	if err != nil {
		return domain.Position{}, fmt.Errorf("запрос позиции: %w", err)
	}

	pos := domain.Position{Amount: decimal.Zero, EntryPrice: decimal.Zero}
	for _, r := range risks {
		if r.Symbol != e.info.Symbol {
			continue
		}
		amt, err := decimal.NewFromString(r.PositionAmt)
		if err != nil {
			return domain.Position{}, fmt.Errorf("разбор positionAmt %q: %w", r.PositionAmt, err)
		}
		pos.Amount = pos.Amount.Add(amt)

		if !amt.IsZero() {
			entry, err := decimal.NewFromString(r.EntryPrice)
			if err != nil {
				return domain.Position{}, fmt.Errorf("разбор entryPrice %q: %w", r.EntryPrice, err)
			}
			pos.EntryPrice = entry
		}
	}
	return pos, nil
}

// CancelAll чистит обе книги ордеров. Обычные и алго-ордера живут на разных
// эндпоинтах (/fapi/v1/openOrders и /fapi/v1/algoOpenOrders), и отмена одной
// книги не трогает другую — стоп и тейк лежат именно в алго-книге.
func (e *OrderExecutor) CancelAll(ctx context.Context) error {
	var errs []error

	if err := e.client.NewCancelAllOpenOrdersService().Symbol(e.info.Symbol).Do(ctx); err != nil && !isEmptyBookErr(err) {
		errs = append(errs, fmt.Errorf("отмена обычных ордеров: %w", err))
	}
	if err := e.client.NewCancelAllAlgoOpenOrdersService().Symbol(e.info.Symbol).Do(ctx); err != nil && !isEmptyBookErr(err) {
		errs = append(errs, fmt.Errorf("отмена алго-ордеров: %w", err))
	}

	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	log.Printf("🧹 Ордера по %s сняты (обычные + алго)", e.info.Symbol)
	return nil
}

// isEmptyBookErr отсекает ответы вида «отменять нечего» — это не сбой.
func isEmptyBookErr(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "-2011") || // Unknown order sent
		strings.Contains(msg, "-4046") || // No need to change
		strings.Contains(msg, "No orders")
}

// clientOrderID даёт ордеру уникальный идентификатор: при ретрае после сетевой
// ошибки биржа отклонит дубликат вместо создания второй позиции.
func clientOrderID(tag string) string {
	return fmt.Sprintf("bot-%s-%s", tag, uuid.NewString()[:8])
}
