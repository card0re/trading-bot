package carry

import (
	"path/filepath"
	"testing"

	"github.com/shopspring/decimal"
)

func TestLoadState_MissingFileGivesStartEquityPerSymbol(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.json")
	st, err := LoadState(path, []string{"BTCUSDT", "ETHUSDT"}, decimal.NewFromInt(5000))
	if err != nil {
		t.Fatalf("неожиданная ошибка: %v", err)
	}
	if len(st.Symbols) != 2 {
		t.Fatalf("ожидалось 2 символа, получено %d", len(st.Symbols))
	}
	if !st.Symbols["BTCUSDT"].Equity.Equal(decimal.NewFromInt(5000)) {
		t.Fatalf("ожидался стартовый эквити 5000, получено %s", st.Symbols["BTCUSDT"].Equity)
	}
}

func TestSaveLoadState_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	st, err := LoadState(path, []string{"BTCUSDT"}, decimal.NewFromInt(1000))
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	sym := st.Symbols["BTCUSDT"]
	sym.Equity = decimal.NewFromInt(1234)
	sym.Position = &Position{EntrySpotPrice: decimal.NewFromInt(100), EntryPerpPrice: decimal.NewFromInt(99)}
	sym.RecentRates = []decimal.Decimal{decimal.NewFromFloat(0.001)}
	st.Symbols["BTCUSDT"] = sym

	if err := SaveState(path, st); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	reloaded, err := LoadState(path, []string{"BTCUSDT"}, decimal.NewFromInt(1000))
	if err != nil {
		t.Fatalf("повторный LoadState: %v", err)
	}
	got := reloaded.Symbols["BTCUSDT"]
	if !got.Equity.Equal(decimal.NewFromInt(1234)) {
		t.Fatalf("эквити не пережил round-trip: %s", got.Equity)
	}
	if got.Position == nil || !got.Position.EntrySpotPrice.Equal(decimal.NewFromInt(100)) {
		t.Fatalf("позиция не пережила round-trip: %+v", got.Position)
	}
	if len(got.RecentRates) != 1 {
		t.Fatalf("recent rates не пережили round-trip: %v", got.RecentRates)
	}
}

func TestLoadState_KeepsExistingSymbolUntouched(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	st, _ := LoadState(path, []string{"BTCUSDT"}, decimal.NewFromInt(1000))
	sym := st.Symbols["BTCUSDT"]
	sym.Equity = decimal.NewFromInt(2000) // "торговали", эквити выросло
	st.Symbols["BTCUSDT"] = sym
	if err := SaveState(path, st); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	// Повторная загрузка с тем же стартовым эквити не должна затирать выросший баланс.
	reloaded, err := LoadState(path, []string{"BTCUSDT"}, decimal.NewFromInt(1000))
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if !reloaded.Symbols["BTCUSDT"].Equity.Equal(decimal.NewFromInt(2000)) {
		t.Fatalf("существующий символ не должен сбрасываться на стартовый эквити, получено %s", reloaded.Symbols["BTCUSDT"].Equity)
	}
}
