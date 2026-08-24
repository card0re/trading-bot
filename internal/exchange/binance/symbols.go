package binance

import (
	"context"
	"fmt"

	"github.com/adshao/go-binance/v2/futures"
	"github.com/shopspring/decimal"
)

// SymbolInfo — торговые ограничения символа, без которых биржа отклоняет ордера
// по PRICE_FILTER / LOT_SIZE / MIN_NOTIONAL.
type SymbolInfo struct {
	Symbol      string
	TickSize    decimal.Decimal
	StepSize    decimal.Decimal
	MinQty      decimal.Decimal
	MaxQty      decimal.Decimal
	MinNotional decimal.Decimal
	PricePrec   int32
	QtyPrec     int32
}

// LoadSymbolInfo тянет фильтры одного символа из exchangeInfo. Под капотом
// exchangeInfo всегда отдаёт фильтры всех символов сразу — при работе с
// несколькими символами используйте LoadAllSymbolInfo, чтобы не делать
// по отдельному полному запросу exchangeInfo на каждый.
func LoadSymbolInfo(ctx context.Context, client *futures.Client, symbol string) (*SymbolInfo, error) {
	all, err := LoadAllSymbolInfo(ctx, client, []string{symbol})
	if err != nil {
		return nil, err
	}
	return all[symbol], nil
}

// LoadAllSymbolInfo запрашивает exchangeInfo один раз и возвращает фильтры
// для каждого из перечисленных символов.
func LoadAllSymbolInfo(ctx context.Context, client *futures.Client, symbols []string) (map[string]*SymbolInfo, error) {
	info, err := client.NewExchangeInfoService().Do(ctx)
	if err != nil {
		return nil, fmt.Errorf("запрос exchangeInfo: %w", err)
	}

	want := make(map[string]bool, len(symbols))
	for _, s := range symbols {
		want[s] = true
	}

	result := make(map[string]*SymbolInfo, len(symbols))
	for i := range info.Symbols {
		s := &info.Symbols[i]
		if !want[s.Symbol] {
			continue
		}
		si, err := buildSymbolInfo(s)
		if err != nil {
			return nil, err
		}
		result[s.Symbol] = si
	}

	for _, symbol := range symbols {
		if _, ok := result[symbol]; !ok {
			return nil, fmt.Errorf("символ %s не найден в exchangeInfo", symbol)
		}
	}
	return result, nil
}

func buildSymbolInfo(s *futures.Symbol) (*SymbolInfo, error) {
	symbol := s.Symbol
	if s.Status != "TRADING" {
		return nil, fmt.Errorf("символ %s недоступен для торговли (статус %s)", symbol, s.Status)
	}

	si := &SymbolInfo{
		Symbol:    symbol,
		PricePrec: int32(s.PricePrecision),
		QtyPrec:   int32(s.QuantityPrecision),
	}
	var err error
	if f := s.PriceFilter(); f != nil {
		if si.TickSize, err = decimal.NewFromString(f.TickSize); err != nil {
			return nil, fmt.Errorf("разбор tickSize %q: %w", f.TickSize, err)
		}
	}
	if f := s.LotSizeFilter(); f != nil {
		if si.StepSize, err = decimal.NewFromString(f.StepSize); err != nil {
			return nil, fmt.Errorf("разбор stepSize %q: %w", f.StepSize, err)
		}
		if si.MinQty, err = decimal.NewFromString(f.MinQuantity); err != nil {
			return nil, fmt.Errorf("разбор minQty %q: %w", f.MinQuantity, err)
		}
		if si.MaxQty, err = decimal.NewFromString(f.MaxQuantity); err != nil {
			return nil, fmt.Errorf("разбор maxQty %q: %w", f.MaxQuantity, err)
		}
	}
	if f := s.MinNotionalFilter(); f != nil {
		if si.MinNotional, err = decimal.NewFromString(f.Notional); err != nil {
			return nil, fmt.Errorf("разбор minNotional %q: %w", f.Notional, err)
		}
	}
	if si.TickSize.IsZero() || si.StepSize.IsZero() {
		return nil, fmt.Errorf("для %s не найдены фильтры tickSize/stepSize", symbol)
	}
	return si, nil
}

// RoundPrice приводит цену к сетке tickSize. down=true округляет вниз —
// это нужно, чтобы стоп/тейк не оказались ближе к цене, чем задумано.
func (s *SymbolInfo) RoundPrice(price decimal.Decimal, down bool) decimal.Decimal {
	q := price.Div(s.TickSize)
	if down {
		q = q.Floor()
	} else {
		q = q.Ceil()
	}
	return q.Mul(s.TickSize)
}

// RoundQuantity округляет объём вниз к stepSize (вверх нельзя — не хватит маржи)
// и проверяет minQty/maxQty/minNotional по опорной цене.
func (s *SymbolInfo) RoundQuantity(qty, refPrice decimal.Decimal) (decimal.Decimal, error) {
	rounded := qty.Div(s.StepSize).Floor().Mul(s.StepSize)

	if rounded.LessThanOrEqual(decimal.Zero) {
		return decimal.Zero, fmt.Errorf("объём %s после округления к шагу %s стал нулевым", qty, s.StepSize)
	}
	if !s.MinQty.IsZero() && rounded.LessThan(s.MinQty) {
		return decimal.Zero, fmt.Errorf("объём %s меньше минимального %s", rounded, s.MinQty)
	}
	if !s.MaxQty.IsZero() && rounded.GreaterThan(s.MaxQty) {
		return decimal.Zero, fmt.Errorf("объём %s больше максимального %s", rounded, s.MaxQty)
	}
	if !s.MinNotional.IsZero() && !refPrice.IsZero() {
		if notional := rounded.Mul(refPrice); notional.LessThan(s.MinNotional) {
			return decimal.Zero, fmt.Errorf(
				"номинал %s USDT меньше минимального %s USDT (увеличьте QUANTITY)",
				notional.StringFixed(2), s.MinNotional)
		}
	}
	return rounded, nil
}

// FormatPrice / FormatQuantity готовят строки в точности, которую ждёт биржа.
func (s *SymbolInfo) FormatPrice(price decimal.Decimal) string {
	return price.StringFixed(s.PricePrec)
}

func (s *SymbolInfo) FormatQuantity(qty decimal.Decimal) string {
	return qty.StringFixed(s.QtyPrec)
}
