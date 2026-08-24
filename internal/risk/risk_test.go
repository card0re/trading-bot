package risk

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func d(s string) decimal.Decimal {
	v, err := decimal.NewFromString(s)
	if err != nil {
		panic(err)
	}
	return v
}

func newTestManager() *Manager {
	// portfolioCap=5%, dailyLossLimit=3%, maxDrawdown=20%.
	return NewManager(d("5"), d("3"), d("20"))
}

func TestCanOpen_AllowsWithinCap(t *testing.T) {
	m := newTestManager()

	allowed, reason := m.CanOpen(d("10000"), d("400")) // 400 < 5% of 10000 = 500
	if !allowed {
		t.Fatalf("ожидалось разрешение, получен отказ: %s", reason)
	}
}

func TestCanOpen_BlocksOverCap(t *testing.T) {
	m := newTestManager()
	m.Reserve("BTCUSDT", d("300"))

	allowed, reason := m.CanOpen(d("10000"), d("250")) // 300+250=550 > 500
	if allowed {
		t.Fatal("ожидался отказ: суммарный риск превышает лимит портфеля")
	}
	if reason == "" {
		t.Fatal("причина отказа не должна быть пустой")
	}
}

func TestCanOpen_AggregatesAcrossSymbols(t *testing.T) {
	m := newTestManager()
	m.Reserve("BTCUSDT", d("200"))
	m.Reserve("ETHUSDT", d("200"))

	// 200+200+150 = 550 > 500.
	allowed, _ := m.CanOpen(d("10000"), d("150"))
	if allowed {
		t.Fatal("риск должен считаться суммарно по всем символам, а не по одному")
	}
}

func TestReserveThenRelease_FreesCapacity(t *testing.T) {
	m := newTestManager()
	m.Reserve("BTCUSDT", d("300"))
	m.Release("BTCUSDT")

	allowed, reason := m.CanOpen(d("10000"), d("450"))
	if !allowed {
		t.Fatalf("после Release риск должен быть освобождён, получен отказ: %s", reason)
	}
}

func TestReserveOverwritesPreviousValueForSameSymbol(t *testing.T) {
	m := newTestManager()
	m.Reserve("BTCUSDT", d("300"))
	m.Reserve("BTCUSDT", d("100")) // повторный Reserve по тому же символу — не складывается

	allowed, reason := m.CanOpen(d("10000"), d("350")) // 100+350=450 < 500
	if !allowed {
		t.Fatalf("повторный Reserve должен перезаписывать, а не складывать риск: %s", reason)
	}
}

func TestDailyLossLimitHit_TriggersAfterLoss(t *testing.T) {
	m := newTestManager()

	if m.DailyLossLimitHit(d("10000")) {
		t.Fatal("без убытков лимит не должен быть достигнут")
	}

	m.RecordRealizedPnL(d("-350")) // лимит = 3% от 10000 = 300, убыток 350 > 300

	if !m.DailyLossLimitHit(d("10000")) {
		t.Fatal("после убытка 350 при лимите 300 ожидалось достижение лимита")
	}
}

func TestDailyLossLimitHit_PositivePnLDoesNotTrigger(t *testing.T) {
	m := newTestManager()
	m.RecordRealizedPnL(d("500"))

	if m.DailyLossLimitHit(d("10000")) {
		t.Fatal("положительный PnL не должен включать дневной лимит убытка")
	}
}

func TestSetClock_OverridesTimeSource(t *testing.T) {
	m := newTestManager()

	fixed := time.Date(2020, 5, 1, 12, 0, 0, 0, time.UTC)
	m.SetClock(func() time.Time { return fixed })
	m.RecordRealizedPnL(d("-400")) // лимит 300 при эквити 10000

	if !m.DailyLossLimitHit(d("10000")) {
		t.Fatal("ожидалось достижение лимита с подменённым временем")
	}

	// Сутки по подменённому времени сдвинулись — лимит должен сброситься,
	// даже если реальное время (time.Now) не менялось вовсе.
	m.SetClock(func() time.Time { return fixed.AddDate(0, 0, 1) })
	if m.DailyLossLimitHit(d("10000")) {
		t.Fatal("после смены подменённых суток лимит должен был сброситься")
	}
}

func TestMaxDrawdownHit_TracksPeakAndTriggers(t *testing.T) {
	m := newTestManager() // maxDrawdown=20%

	if m.MaxDrawdownHit(d("10000")) {
		t.Fatal("на первом наблюдении (это и есть пик) просадки быть не может")
	}
	// Пик поднялся до 12000.
	if m.MaxDrawdownHit(d("12000")) {
		t.Fatal("рост эквити не должен считаться просадкой")
	}
	// Просадка от пика 12000: (12000-9700)/12000 = 19.17% < 20% — ещё не лимит.
	if m.MaxDrawdownHit(d("9700")) {
		t.Fatal("просадка 19.17% не должна достигать лимита 20%")
	}
	// Просадка от пика 12000: (12000-9500)/12000 = 20.83% >= 20% — лимит достигнут.
	if !m.MaxDrawdownHit(d("9500")) {
		t.Fatal("просадка 20.83% должна была достичь лимита 20%")
	}
}

func TestMaxDrawdownHit_RecoversAutomatically(t *testing.T) {
	m := newTestManager()
	m.MaxDrawdownHit(d("10000")) // пик
	if !m.MaxDrawdownHit(d("7000")) {
		t.Fatal("просадка 30% должна была достичь лимита 20%")
	}
	// Эквити отыграла обратно выше порога (пик всё ещё 10000, просадка 15%).
	if m.MaxDrawdownHit(d("8500")) {
		t.Fatal("после восстановления эквити выше порога лимит должен сняться сам, без ручного сброса")
	}
}

func TestMaxDrawdownHit_DisabledWhenZero(t *testing.T) {
	m := NewManager(d("5"), d("3"), decimal.Zero)
	m.MaxDrawdownHit(d("10000"))
	if m.MaxDrawdownHit(d("1")) {
		t.Fatal("MaxDrawdownPct=0 должен полностью отключать проверку")
	}
}

func TestDailyLossResetsOnNewUTCDay(t *testing.T) {
	m := newTestManager()

	day1 := time.Date(2026, 1, 1, 23, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return day1 }
	m.RecordRealizedPnL(d("-400"))

	if !m.DailyLossLimitHit(d("10000")) {
		t.Fatal("ожидалось достижение дневного лимита убытка 1 января")
	}

	day2 := time.Date(2026, 1, 2, 1, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return day2 }

	if m.DailyLossLimitHit(d("10000")) {
		t.Fatal("после смены суток (UTC) дневной убыток должен обнулиться")
	}
}
