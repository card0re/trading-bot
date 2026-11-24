package predict

import (
	"math"
	"testing"
)

func TestOpportunities(t *testing.T) {
	bars := fixture(99, 98, 98, 98, 101)
	table := map[Key]*Cell{}
	for b := range len(ZEdges) - 1 {
		table[Key{Window: 5, MinLeft: 2, Bucket: b}] = &Cell{N: 100, Rev: 3}
	}
	ws := windowStart * 1000
	at := ws + 150_000 // 2.5 мин до конца → MinLeft 2
	rows := []RecRow{
		{T: at, Slug: "btc-up-or-down-5m", WindowStart: ws, Side: "UP", Ask: 0.02, HasAsk: true, Depth: 5, Spot: 98},
		{T: at, Slug: "btc-up-or-down-5m", WindowStart: ws, Side: "DOWN", Ask: 0.97, HasAsk: true, Depth: 0, Spot: 98},
		{T: ws + 330_000, Slug: "btc-up-or-down-5m", WindowStart: ws, Side: "RESULT", Winner: "Up"},
	}
	opps := Opportunities(rows, bars, table, nil)
	if l := (map[int]map[int]*Lift{5: {HourGroup(Sample{T: at / 1000}): {N: 10, Rev: 2, Exp: 1}}}); len(Opportunities(rows, bars, table, l)) != 1 ||
		math.Abs(Opportunities(rows, bars, table, l)[0].ModelP-0.06) > 1e-12 {
		t.Fatal("hour lift ×2 must double ModelP 0.03 → 0.06")
	}
	if len(opps) != 1 {
		t.Fatalf("want 1 opportunity, got %d: %+v", len(opps), opps)
	}
	o := opps[0]
	if o.Window != 5 || o.MinLeft != 2 || o.Z >= 0 || o.ImpliedP != 0.02 || math.Abs(o.EV-0.47) > 1e-9 || !o.Won || !o.Resolved {
		t.Fatalf("opp %+v", o)
	}
}

func TestOpportunitiesSkipsThinBook(t *testing.T) {
	bars := fixture(99, 98, 98, 98, 101)
	table := map[Key]*Cell{}
	for b := range len(ZEdges) - 1 {
		table[Key{Window: 5, MinLeft: 2, Bucket: b}] = &Cell{N: 100, Rev: 3}
	}
	ws := windowStart * 1000
	rows := []RecRow{{T: ws + 150_000, Slug: "btc-up-or-down-5m", WindowStart: ws, Side: "UP", Ask: 0.02, HasAsk: true, Depth: 0.5, Spot: 98}}
	if opps := Opportunities(rows, bars, table, nil); len(opps) != 0 {
		t.Fatalf("book with < $1 depth is not an opportunity: %+v", opps)
	}
}
