package predict

import (
	"math"
	"testing"
)

func TestLiftBy(t *testing.T) {
	k := Key{Window: 5, MinLeft: 2, Bucket: 4}
	table := map[Key]*Cell{k: {N: 100, Rev: 10}} // p = 0.1
	var s []Sample
	for i := 0; i < 10; i++ {
		s = append(s, Sample{T: 0, Window: 5, MinLeft: 2, Z: 2.2, Reversal: i < 2}) // группа 0: 2 из 10
		s = append(s, Sample{T: 3600, Window: 5, MinLeft: 2, Z: -2.2})              // группа 1: 0 из 10
	}
	s = append(s, Sample{T: 0, Window: 5, MinLeft: 1, Z: 2.2, Reversal: true}) // вне зоны: 1 мин
	s = append(s, Sample{T: 0, Window: 5, MinLeft: 2, Z: 1.2, Reversal: true}) // вне зоны: |z| < 2
	l := LiftBy(s, table, func(x Sample) int { return int(x.T / 3600) }, InZone)
	g0, g1 := l[0], l[1]
	if g0 == nil || g0.N != 10 || g0.Rev != 2 || math.Abs(g0.Exp-1) > 1e-9 || math.Abs(g0.Ratio()-2) > 1e-9 {
		t.Fatalf("group 0: %+v", g0)
	}
	if g1 == nil || g1.Rev != 0 || g1.Ratio() != 0 {
		t.Fatalf("group 1: %+v", g1)
	}
}

func TestHourWeekdayUTC(t *testing.T) {
	// 1700000100 = 2023-11-14 22:15 UTC, вторник
	x := Sample{T: 1700000100}
	if HourUTC(x) != 22 || WeekdayUTC(x) != 2 {
		t.Fatalf("hour %d weekday %d", HourUTC(x), WeekdayUTC(x))
	}
}

func TestAdjustP(t *testing.T) {
	l := &Lift{N: 100, Rev: 3, Exp: 2}
	if p := AdjustP(0.02, l); math.Abs(p-0.03) > 1e-12 {
		t.Fatalf("adjusted %v", p)
	}
	if p := AdjustP(0.02, nil); p != 0.02 {
		t.Fatalf("nil lift must keep p, got %v", p)
	}
	if p := AdjustP(0.8, &Lift{N: 10, Rev: 5, Exp: 1}); p != 1 {
		t.Fatalf("p must be capped at 1, got %v", p)
	}
}

func TestLogLoss(t *testing.T) {
	s := []Sample{{Reversal: true}, {}, {Reversal: true}, {}}
	ll, n := LogLoss(s, func(Sample) (float64, bool) { return 0.5, true })
	if n != 4 || math.Abs(ll-math.Ln2) > 1e-12 {
		t.Fatalf("logloss %v n %d", ll, n)
	}
	if _, n := LogLoss(s, func(Sample) (float64, bool) { return 0, false }); n != 0 {
		t.Fatalf("skipped samples must not count, n=%d", n)
	}
}

func TestHourGroup(t *testing.T) {
	at := func(h int64) Sample { return Sample{T: 1700000100 - 22*3600 + h*3600} } // 1700000100 = 22:15 UTC
	if HourGroup(at(13)) != 1 || HourGroup(at(17)) != 2 || HourGroup(at(3)) != 0 {
		t.Fatalf("groups %d %d %d", HourGroup(at(13)), HourGroup(at(17)), HourGroup(at(3)))
	}
}
