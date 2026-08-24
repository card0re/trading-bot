// Package rotation содержит только состояние виртуального портфеля
// ротационного shadow-бота (cmd/rotationbot) — вынесено в отдельный пакет,
// чтобы internal/app мог читать тот же файл состояния для команды /status,
// не завися от cmd/rotationbot (у команд зависимости быть не должно).
package rotation

import (
	"encoding/json"
	"os"
	"time"

	"github.com/shopspring/decimal"
)

// Position — одна нога виртуального портфеля. Qty знаковый: >0 лонг, <0 шорт.
type Position struct {
	Qty        decimal.Decimal `json:"qty"`
	EntryPrice decimal.Decimal `json:"entry_price"`
}

// State — весь виртуальный портфель ротации на диске.
type State struct {
	Equity        decimal.Decimal     `json:"equity"`
	PeakEquity    decimal.Decimal     `json:"peak_equity"`
	Positions     map[string]Position `json:"positions"`
	LastRebalance time.Time           `json:"last_rebalance"`
}

// LoadState читает состояние из файла. Отсутствие файла — не ошибка, это
// нормальный первый запуск: возвращается State{} с эквити startEquity.
func LoadState(path string, startEquity decimal.Decimal) (State, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return State{Equity: startEquity, PeakEquity: startEquity, Positions: make(map[string]Position)}, nil
		}
		return State{}, err
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return State{}, err
	}
	if st.Positions == nil {
		st.Positions = make(map[string]Position)
	}
	return st, nil
}

// SaveState сохраняет состояние в файл (перезаписывает). Пишет во временный
// файл рядом и переименовывает его поверх целевого — os.Rename на одной
// файловой системе атомарен, поэтому крэш/OOM/нехватка места посреди записи
// не может оставить на диске битый обрубленный JSON: либо остаётся старый
// файл целиком, либо новый целиком, третьего не бывает. Прямой
// os.WriteFile этого не гарантирует.
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
