package metrics

import (
	"bytes"
	"strings"
	"testing"
)

func TestWriteText(t *testing.T) {
	var buf bytes.Buffer
	err := WriteText(&buf, []Metric{
		{Name: "equity_usdt", Help: "Equity", Type: "gauge", Value: 10000.5},
		{Name: "position_state", Help: "State", Type: "gauge", Value: 2, Labels: map[string]string{"symbol": "BTCUSDT"}},
	})
	if err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	got := buf.String()

	want := []string{
		"# HELP equity_usdt Equity",
		"# TYPE equity_usdt gauge",
		"equity_usdt 10000.5",
		`position_state{symbol="BTCUSDT"} 2`,
	}
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("вывод не содержит %q, получено:\n%s", w, got)
		}
	}
}

func TestWriteTextLabelOrderDeterministic(t *testing.T) {
	var buf bytes.Buffer
	m := Metric{Name: "x", Help: "x", Type: "gauge", Value: 1, Labels: map[string]string{"z": "1", "a": "2"}}
	if err := WriteText(&buf, []Metric{m}); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	if got := buf.String(); !strings.Contains(got, `x{a="2",z="1"}`) {
		t.Fatalf("метки должны идти в отсортированном порядке (a перед z), получено: %s", got)
	}
}
