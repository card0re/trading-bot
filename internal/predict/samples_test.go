package predict

import (
	"math"
	"testing"
)

// windowStart выровнен по 5m и 15m: 1700000100 = 5666667*300, но не кратно 900 —
// для 5m-тестов этого достаточно.
const windowStart int64 = 1700000100

// fixture: 60 баров разогрева (чередование ±0.1% вокруг 100), затем 5m-окно
// open=100 с заданными close.
func fixture(closes ...float64) []Bar {
	var bars []Bar
	t := windowStart - 60*60
	for i := 0; i < 60; i++ {
		c := 100.1
		if i%2 == 1 {
			c = 99.9
		}
		bars = append(bars, Bar{T: t, O: 100, C: c})
		t += 60
	}
	open := 100.0
	for _, c := range closes {
		bars = append(bars, Bar{T: t, O: open, C: c})
		open = c
		t += 60
	}
	bars[60].O = 100
	return bars
}

func TestSamplesReversal(t *testing.T) {
	s := Samples(fixture(99, 98, 98, 98, 101), 5)
	if len(s) != 4 {
		t.Fatalf("want 4 samples, got %d", len(s))
	}
	for i, x := range s {
		if x.Z >= 0 || !x.Reversal || x.Tie || x.MinLeft != 4-i || x.Window != 5 {
			t.Fatalf("sample %d wrong: %+v", i, x)
		}
		if x.T != windowStart+int64(i+1)*60 {
			t.Fatalf("sample %d T=%d", i, x.T)
		}
	}
}

func TestSamplesNoReversal(t *testing.T) {
	for _, x := range Samples(fixture(99, 98, 98, 98, 97), 5) {
		if x.Reversal || x.Tie {
			t.Fatalf("unexpected reversal: %+v", x)
		}
	}
}

func TestSamplesTie(t *testing.T) {
	s := Samples(fixture(99, 98, 98, 98, 100), 5)
	if len(s) == 0 {
		t.Fatal("no samples")
	}
	for _, x := range s {
		if x.Reversal || !x.Tie {
			t.Fatalf("tie must be a loss with Tie=true: %+v", x)
		}
	}
}

func TestSamplesGapSkipsWindow(t *testing.T) {
	bars := fixture(99, 98, 98, 98, 101)
	bars = append(bars[:62], bars[63:]...) // убрали 3-й бар окна
	if s := Samples(bars, 5); len(s) != 0 {
		t.Fatalf("window with a gap must be skipped, got %d samples", len(s))
	}
}

func TestSamplesFlatSkipped(t *testing.T) {
	var bars []Bar
	for i := 0; i < 65; i++ {
		bars = append(bars, Bar{T: windowStart - 3600 + int64(i)*60, O: 100, C: 100})
	}
	for _, x := range Samples(bars, 5) {
		if math.IsNaN(x.Z) || math.IsInf(x.Z, 0) {
			t.Fatalf("NaN/Inf z: %+v", x)
		}
	}
	if s := Samples(bars, 5); len(s) != 0 {
		t.Fatalf("flat market must give 0 samples, got %d", len(s))
	}
}

func TestSamplesAligned(t *testing.T) {
	bars := fixture(99, 98, 98, 98, 101)
	for i := range bars {
		bars[i].T += 60 // сдвигаем всё на минуту
	}
	for _, x := range Samples(bars, 5) {
		start := x.T - int64(x.Window-x.MinLeft)*60
		if start%300 != 0 {
			t.Fatalf("unaligned window start %d", start)
		}
	}
}
