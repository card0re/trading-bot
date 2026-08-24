package binance

import (
	"testing"

	"github.com/shopspring/decimal"
)

func btcInfo() *SymbolInfo {
	return &SymbolInfo{
		Symbol:      "BTCUSDT",
		TickSize:    decimal.RequireFromString("0.10"),
		StepSize:    decimal.RequireFromString("0.001"),
		MinQty:      decimal.RequireFromString("0.001"),
		MaxQty:      decimal.RequireFromString("1000"),
		MinNotional: decimal.RequireFromString("100"),
		PricePrec:   1,
		QtyPrec:     3,
	}
}

func TestRoundPriceSnapsToTick(t *testing.T) {
	info := btcInfo()

	cases := []struct {
		name  string
		price string
		down  bool
		want  string
	}{
		{"вниз до тика", "60123.47", true, "60123.4"},
		{"вверх до тика", "60123.41", false, "60123.5"},
		{"точное значение вниз", "60123.40", true, "60123.4"},
		{"точное значение вверх", "60123.40", false, "60123.4"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := info.RoundPrice(decimal.RequireFromString(tc.price), tc.down)
			if !got.Equal(decimal.RequireFromString(tc.want)) {
				t.Fatalf("RoundPrice(%s, down=%v) = %s, ожидалось %s", tc.price, tc.down, got, tc.want)
			}
		})
	}
}

func TestRoundQuantityFloorsToStep(t *testing.T) {
	info := btcInfo()
	price := decimal.RequireFromString("60000")

	got, err := info.RoundQuantity(decimal.RequireFromString("0.0037"), price)
	if err != nil {
		t.Fatalf("неожиданная ошибка: %v", err)
	}
	// Округление только вниз: вверх могло бы не хватить маржи.
	if !got.Equal(decimal.RequireFromString("0.003")) {
		t.Fatalf("получено %s, ожидалось 0.003", got)
	}
}

func TestRoundQuantityRejectsBelowMinNotional(t *testing.T) {
	info := btcInfo()
	price := decimal.RequireFromString("60000")

	// 0.001 * 60000 = 60 USDT при минимуме 100 — биржа отклонила бы ордер.
	if _, err := info.RoundQuantity(decimal.RequireFromString("0.001"), price); err == nil {
		t.Fatal("ожидалась ошибка minNotional, получено nil")
	}
}

func TestRoundQuantityRejectsBelowMinQty(t *testing.T) {
	info := btcInfo()

	if _, err := info.RoundQuantity(decimal.RequireFromString("0.0005"), decimal.RequireFromString("60000")); err == nil {
		t.Fatal("ожидалась ошибка minQty, получено nil")
	}
}

func TestRoundQuantityRejectsAboveMaxQty(t *testing.T) {
	info := btcInfo()

	if _, err := info.RoundQuantity(decimal.RequireFromString("2000"), decimal.RequireFromString("60000")); err == nil {
		t.Fatal("ожидалась ошибка maxQty, получено nil")
	}
}

func TestFormatUsesExchangePrecision(t *testing.T) {
	info := btcInfo()

	if got := info.FormatPrice(decimal.RequireFromString("60123.4")); got != "60123.4" {
		t.Fatalf("FormatPrice = %q, ожидалось \"60123.4\"", got)
	}
	if got := info.FormatQuantity(decimal.RequireFromString("0.003")); got != "0.003" {
		t.Fatalf("FormatQuantity = %q, ожидалось \"0.003\"", got)
	}
}

// Символ с целым шагом объёма (например, 1 контракт) — округление не должно
// давать дробей, иначе биржа вернёт ошибку LOT_SIZE.
func TestRoundQuantityWithIntegerStep(t *testing.T) {
	info := &SymbolInfo{
		Symbol:      "DOGEUSDT",
		TickSize:    decimal.RequireFromString("0.00001"),
		StepSize:    decimal.RequireFromString("1"),
		MinQty:      decimal.RequireFromString("1"),
		MinNotional: decimal.RequireFromString("5"),
		PricePrec:   5,
		QtyPrec:     0,
	}

	got, err := info.RoundQuantity(decimal.RequireFromString("123.9"), decimal.RequireFromString("0.4"))
	if err != nil {
		t.Fatalf("неожиданная ошибка: %v", err)
	}
	if !got.Equal(decimal.RequireFromString("123")) {
		t.Fatalf("получено %s, ожидалось 123", got)
	}
	if s := info.FormatQuantity(got); s != "123" {
		t.Fatalf("FormatQuantity = %q, ожидалось \"123\"", s)
	}
}
