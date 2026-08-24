package binance

import (
	"context"
	"fmt"

	"github.com/adshao/go-binance/v2/futures"
	"github.com/shopspring/decimal"
)

// AccountEquitySource реализует domain.EquitySource поверх аккаунт-вайд
// REST-вызова к Binance. Отдельный тип, а не метод OrderExecutor: эквити
// общий на весь аккаунт, один экземпляр используется всеми символами сразу.
type AccountEquitySource struct {
	client *futures.Client
}

// NewAccountEquitySource создаёт источник эквити поверх общего клиента.
func NewAccountEquitySource(client *futures.Client) *AccountEquitySource {
	return &AccountEquitySource{client: client}
}

// Equity возвращает totalMarginBalance (баланс кошелька + нереализованный
// PnL по всем позициям) — это база для риск-сайзинга: в отличие от
// availableBalance она не уменьшается, когда маржа уже занята под открытые
// позиции, и в отличие от totalWalletBalance учитывает текущую плавающую
// просадку/прибыль по другим открытым позициям.
func (a *AccountEquitySource) Equity(ctx context.Context) (decimal.Decimal, error) {
	acc, err := a.client.NewGetAccountService().Do(ctx)
	if err != nil {
		return decimal.Zero, fmt.Errorf("запрос состояния счёта: %w", err)
	}
	equity, err := decimal.NewFromString(acc.TotalMarginBalance)
	if err != nil {
		return decimal.Zero, fmt.Errorf("разбор totalMarginBalance %q: %w", acc.TotalMarginBalance, err)
	}
	return equity, nil
}
