package config

import (
	"testing"

	"github.com/shopspring/decimal"
)

// validConfig — минимальный набор значений, проходящий validate() целиком.
// Каждый тест-кейс ниже портит ровно одно поле и ждёт ошибку.
func validConfig() *AppConfig {
	return &AppConfig{
		Symbols:              []string{"BTCUSDT", "ETHUSDT"},
		Leverage:             3,
		MarginType:           "ISOLATED",
		LookbackBars:         30,
		CooldownBars:         3,
		TrendEMAPeriod:       100,
		ATRPeriod:            14,
		VolumeAvgPeriod:      20,
		BreakoutPct:          decimal.NewFromFloat(0.05),
		VolumeMultiplier:     decimal.NewFromFloat(2.0),
		ATRStopMultiplier:    decimal.NewFromFloat(3.0),
		RiskRewardRatio:      decimal.NewFromFloat(1.0),
		ADXPeriod:            14,
		TrendStrengthMinADX:  decimal.NewFromFloat(25),
		BreakevenTriggerR:    decimal.Zero,
		MeanRevATRMultiplier: decimal.Zero,
		FundingCarryMinRate:  decimal.NewFromFloat(0.001),
		VolTargetPeriod:      50,
		OrderFlowMinRatio:    decimal.NewFromFloat(0.6),
		RiskPerTradePct:      decimal.NewFromFloat(1.0),
		PortfolioRiskCapPct:  decimal.NewFromFloat(4.5),
		DailyLossLimitPct:    decimal.NewFromFloat(3.0),
		MaxDrawdownPct:       decimal.NewFromFloat(20),
	}
}

func TestValidConfigPasses(t *testing.T) {
	if err := validConfig().validate(); err != nil {
		t.Fatalf("базовый валидный конфиг не должен давать ошибку: %v", err)
	}
}

func TestValidateRejects(t *testing.T) {
	cases := []struct {
		name   string
		break_ func(*AppConfig)
	}{
		{"пустой Symbols", func(c *AppConfig) { c.Symbols = nil }},
		{"пустой символ в Symbols", func(c *AppConfig) { c.Symbols = []string{"BTCUSDT", ""} }},
		{"повтор в Symbols", func(c *AppConfig) { c.Symbols = []string{"BTCUSDT", "BTCUSDT"} }},
		{"Leverage ниже диапазона", func(c *AppConfig) { c.Leverage = 0 }},
		{"Leverage выше диапазона", func(c *AppConfig) { c.Leverage = 126 }},
		{"неверный MarginType", func(c *AppConfig) { c.MarginType = "FOO" }},
		{"LookbackBars < 2", func(c *AppConfig) { c.LookbackBars = 1 }},
		{"отрицательный CooldownBars", func(c *AppConfig) { c.CooldownBars = -1 }},
		{"TrendEMAPeriod < 1", func(c *AppConfig) { c.TrendEMAPeriod = 0 }},
		{"ATRPeriod < 1", func(c *AppConfig) { c.ATRPeriod = 0 }},
		{"VolumeAvgPeriod < 1", func(c *AppConfig) { c.VolumeAvgPeriod = 0 }},
		{"отрицательный BreakoutPct", func(c *AppConfig) { c.BreakoutPct = decimal.NewFromFloat(-0.01) }},
		{"VolumeMultiplier <= 0", func(c *AppConfig) { c.VolumeMultiplier = decimal.Zero }},
		{"ATRStopMultiplier <= 0", func(c *AppConfig) { c.ATRStopMultiplier = decimal.Zero }},
		{"RiskRewardRatio <= 0", func(c *AppConfig) { c.RiskRewardRatio = decimal.Zero }},
		{"ADXPeriod < 1", func(c *AppConfig) { c.ADXPeriod = 0 }},
		{"отрицательный TrendStrengthMinADX", func(c *AppConfig) { c.TrendStrengthMinADX = decimal.NewFromFloat(-1) }},
		{"отрицательный BreakevenTriggerR", func(c *AppConfig) { c.BreakevenTriggerR = decimal.NewFromFloat(-1) }},
		{"отрицательный MeanRevATRMultiplier", func(c *AppConfig) { c.MeanRevATRMultiplier = decimal.NewFromFloat(-1) }},
		{"отрицательный FundingCarryMinRate", func(c *AppConfig) { c.FundingCarryMinRate = decimal.NewFromFloat(-1) }},
		{"отрицательный VolTargetPeriod", func(c *AppConfig) { c.VolTargetPeriod = -1 }},
		{"отрицательный OrderFlowMinRatio", func(c *AppConfig) { c.OrderFlowMinRatio = decimal.NewFromFloat(-0.1) }},
		{"OrderFlowMinRatio > 1", func(c *AppConfig) { c.OrderFlowMinRatio = decimal.NewFromFloat(1.1) }},
		{"RiskPerTradePct <= 0", func(c *AppConfig) { c.RiskPerTradePct = decimal.Zero }},
		{"PortfolioRiskCapPct < RiskPerTradePct", func(c *AppConfig) {
			c.RiskPerTradePct = decimal.NewFromFloat(5)
			c.PortfolioRiskCapPct = decimal.NewFromFloat(1)
		}},
		{"DailyLossLimitPct <= 0", func(c *AppConfig) { c.DailyLossLimitPct = decimal.Zero }},
		{"отрицательный MaxDrawdownPct", func(c *AppConfig) { c.MaxDrawdownPct = decimal.NewFromFloat(-1) }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig()
			tc.break_(cfg)
			if err := cfg.validate(); err == nil {
				t.Fatalf("ожидалась ошибка для кейса %q, получено nil", tc.name)
			}
		})
	}
}

func TestEnvHelpers(t *testing.T) {
	t.Run("envStr использует переменную и умолчание", func(t *testing.T) {
		t.Setenv("CFG_TEST_STR", "")
		if got := envStr("CFG_TEST_STR", "def"); got != "def" {
			t.Fatalf("ожидалось def, получено %q", got)
		}
		t.Setenv("CFG_TEST_STR", "val")
		if got := envStr("CFG_TEST_STR", "def"); got != "val" {
			t.Fatalf("ожидалось val, получено %q", got)
		}
	})

	t.Run("envStrList парсит, обрезает и приводит к верхнему регистру", func(t *testing.T) {
		t.Setenv("CFG_TEST_LIST", " btcusdt, ethusdt ,,")
		got := envStrList("CFG_TEST_LIST", nil)
		want := []string{"BTCUSDT", "ETHUSDT"}
		if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
			t.Fatalf("получено %v, ожидалось %v", got, want)
		}
	})

	t.Run("envBool парсит валидное значение и падает на невалидном", func(t *testing.T) {
		t.Setenv("CFG_TEST_BOOL", "false")
		if got := envBool("CFG_TEST_BOOL", true); got != false {
			t.Fatalf("ожидалось false, получено %v", got)
		}
		t.Setenv("CFG_TEST_BOOL", "не-булево")
		if got := envBool("CFG_TEST_BOOL", true); got != true {
			t.Fatalf("невалидное значение должно откатываться на умолчание, получено %v", got)
		}
	})

	t.Run("envInt возвращает ошибку на невалидном значении", func(t *testing.T) {
		t.Setenv("CFG_TEST_INT", "не-число")
		if _, err := envInt("CFG_TEST_INT", 1); err == nil {
			t.Fatal("ожидалась ошибка")
		}
	})

	t.Run("envDecimal возвращает ошибку на невалидном значении", func(t *testing.T) {
		t.Setenv("CFG_TEST_DEC", "не-число")
		if _, err := envDecimal("CFG_TEST_DEC", "1.0"); err == nil {
			t.Fatal("ожидалась ошибка")
		}
	})
}
