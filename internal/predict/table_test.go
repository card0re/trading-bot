package predict

import (
	"math"
	"testing"
)

func TestBucket(t *testing.T) {
	if Bucket(0.2) != 0 || Bucket(-2.2) != 4 || Bucket(7) != 9 {
		t.Fatalf("Bucket: %d %d %d", Bucket(0.2), Bucket(-2.2), Bucket(7))
	}
}

func TestWilson(t *testing.T) {
	c := Cell{N: 100, Rev: 2}
	lo, hi := c.Wilson()
	if c.P() != 0.02 || math.Abs(lo-0.0055) > 0.001 || math.Abs(hi-0.0700) > 0.001 {
		t.Fatalf("P=%v lo=%v hi=%v", c.P(), lo, hi)
	}
}

func TestBreakeven(t *testing.T) {
	if k := (&Cell{N: 100, Rev: 2}).Breakeven(); math.Abs(k-51.02) > 0.01 {
		t.Fatalf("breakeven %v", k)
	}
	if k := (&Cell{N: 100}).Breakeven(); !math.IsInf(k, 1) {
		t.Fatalf("breakeven at p=0 must be +Inf, got %v", k)
	}
}

func TestTabulate(t *testing.T) {
	s := []Sample{
		{Window: 5, MinLeft: 2, Z: -2.2, Reversal: true},
		{Window: 5, MinLeft: 2, Z: 2.3, Tie: true},
		{Window: 5, MinLeft: 2, Z: -2.4},
	}
	c := Tabulate(s)[Key{Window: 5, MinLeft: 2, Bucket: 4}]
	if c == nil || c.N != 3 || c.Rev != 1 || c.Ties != 1 || c.GaussP <= 0 {
		t.Fatalf("cell %+v", c)
	}
}

func TestSimulateBankrollEdges(t *testing.T) {
	if s := SimulateBankroll(1, 50, 10, 100, 50, 1); s.RuinProb != 0 || s.MedianMaxLoseStreak != 0 {
		t.Fatalf("p=1: %+v", s)
	}
	if s := SimulateBankroll(0, 50, 10, 100, 50, 1); s.RuinProb != 1 {
		t.Fatalf("p=0: %+v", s)
	}
}
