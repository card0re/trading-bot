package rotation

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func TestLoadState_MissingFileReturnsStartEquity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.json")
	st, err := LoadState(path, decimal.NewFromInt(5000))
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if !st.Equity.Equal(decimal.NewFromInt(5000)) || !st.PeakEquity.Equal(decimal.NewFromInt(5000)) {
		t.Fatalf("ожидался стартовый эквити 5000, получено equity=%s peak=%s", st.Equity, st.PeakEquity)
	}
	if st.Positions == nil {
		t.Fatal("Positions должен быть инициализирован пустой мапой, не nil")
	}
}

func TestSaveThenLoadState_RoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	want := State{
		Equity:     decimal.NewFromFloat(9876.54),
		PeakEquity: decimal.NewFromFloat(10000),
		Positions: map[string]Position{
			"BTCUSDT": {Qty: decimal.NewFromFloat(0.015), EntryPrice: decimal.NewFromFloat(65000)},
			"ETHUSDT": {Qty: decimal.NewFromFloat(-1.2), EntryPrice: decimal.NewFromFloat(2500)},
		},
		LastRebalance: time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC),
	}

	if err := SaveState(path, want); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	got, err := LoadState(path, decimal.Zero)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if !got.Equity.Equal(want.Equity) {
		t.Errorf("Equity: получено %s, ожидалось %s", got.Equity, want.Equity)
	}
	if !got.PeakEquity.Equal(want.PeakEquity) {
		t.Errorf("PeakEquity: получено %s, ожидалось %s", got.PeakEquity, want.PeakEquity)
	}
	if !got.LastRebalance.Equal(want.LastRebalance) {
		t.Errorf("LastRebalance: получено %s, ожидалось %s", got.LastRebalance, want.LastRebalance)
	}
	if len(got.Positions) != len(want.Positions) {
		t.Fatalf("Positions: получено %d, ожидалось %d", len(got.Positions), len(want.Positions))
	}
	for symbol, wantPos := range want.Positions {
		gotPos, ok := got.Positions[symbol]
		if !ok {
			t.Fatalf("позиция %s пропала после сохранения/загрузки", symbol)
		}
		if !gotPos.Qty.Equal(wantPos.Qty) || !gotPos.EntryPrice.Equal(wantPos.EntryPrice) {
			t.Errorf("%s: получено %+v, ожидалось %+v", symbol, gotPos, wantPos)
		}
	}
}
