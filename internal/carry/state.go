package carry

import (
	"encoding/json"
	"os"
	"time"

	"github.com/shopspring/decimal"
)

// Position — открытая виртуальная нога carry-сделки: спот куплен, перп
// шортится на тот же нотионал (обе ноги виртуальные, реальных ордеров нет).
type Position struct {
	EntrySpotPrice decimal.Decimal `json:"entry_spot_price"`
	EntryPerpPrice decimal.Decimal `json:"entry_perp_price"`
	OpenedAt       time.Time       `json:"opened_at"`
	HeldPeriods    int             `json:"held_periods"`
}

// SymbolState — виртуальный carry-счёт ОДНОЙ монеты. Символы не делят один
// пул капитала (как ротация) — каждый ведёт свою независимую долю, ровно
// как их бэктестил cmd/carry (7 независимых $X-симуляций, не одна общая) —
// живой бот должен повторять ту же методологию, которую проверяли.
type SymbolState struct {
	Equity          decimal.Decimal   `json:"equity"`
	PeakEquity      decimal.Decimal   `json:"peak_equity"`
	Position        *Position         `json:"position,omitempty"`
	RecentRates     []decimal.Decimal `json:"recent_rates"` // последние ставки funding, для скользящего среднего (Params.Lookback)
	LastFundingTime time.Time         `json:"last_funding_time"`
}

// State — весь виртуальный carry-портфель на диске.
type State struct {
	Symbols map[string]SymbolState `json:"symbols"`
}

// LoadState — как rotation.LoadState: отсутствие файла — первый запуск, не
// ошибка. equityPerSymbol — стартовый виртуальный эквити для символа,
// которого ещё нет в файле (новый символ добавлен в конфиг задним числом).
func LoadState(path string, symbols []string, equityPerSymbol decimal.Decimal) (State, error) {
	st := State{Symbols: make(map[string]SymbolState, len(symbols))}
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return State{}, err
		}
	} else if err := json.Unmarshal(data, &st); err != nil {
		return State{}, err
	}
	if st.Symbols == nil {
		st.Symbols = make(map[string]SymbolState, len(symbols))
	}
	for _, s := range symbols {
		if _, ok := st.Symbols[s]; !ok {
			st.Symbols[s] = SymbolState{Equity: equityPerSymbol, PeakEquity: equityPerSymbol}
		}
	}
	return st, nil
}

// SaveState — тот же write-temp-then-rename паттерн, что и rotation.SaveState.
func SaveState(path string, st State) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
