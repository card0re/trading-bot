package binance

import (
	"os"
	"testing"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestPredParseWindow(t *testing.T) {
	w, err := parsePredWindow(fixture(t, "prediction_event_detail.json"))
	if err != nil {
		t.Fatal(err)
	}
	if w.MarketID != 25621367 || w.UpToken == "" || w.DownToken == "" || w.UpToken == w.DownToken {
		t.Fatalf("window %+v", w)
	}
	if w.Start != 1795542900000 || w.End != 1795543200000 || w.Status != "REGISTERED" || w.Winner != "" {
		t.Fatalf("window %+v", w)
	}
}

func TestPredParseBook(t *testing.T) {
	b, err := parsePredBook(fixture(t, "prediction_order_book.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !b.HasAsk || b.BestAsk != 0.01 || b.HasBid {
		t.Fatalf("book %+v", b)
	}
	// на лучшем ask 0.01 лежит 9003.54 доли = $90.04
	if b.AskBestUSDT < 90.03 || b.AskBestUSDT > 90.04 {
		t.Fatalf("best ask usdt %v", b.AskBestUSDT)
	}
}

func TestPredParseBookNoAsks(t *testing.T) {
	b, err := parsePredBook(fixture(t, "prediction_order_book_noasks.json"))
	if err != nil {
		t.Fatal(err)
	}
	if b.HasAsk || !b.HasBid || b.BestBid != 0.97 {
		t.Fatalf("book %+v", b)
	}
}

func TestPredParseError(t *testing.T) {
	if _, err := parsePredWindow(fixture(t, "prediction_error.json")); err == nil {
		t.Fatal("want error for success=false")
	}
	if _, err := parsePredBook(fixture(t, "prediction_error.json")); err == nil {
		t.Fatal("want error for success=false")
	}
}
