package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSlugParsing(t *testing.T) {
	cases := []struct {
		slug string
		min  int
		sym  string
	}{
		{"btc-up-or-down-5m", 5, "BTCUSDT"},
		{"eth-up-or-down-15m", 15, "ETHUSDT"},
	}
	for _, c := range cases {
		m, err := windowMinutes(c.slug)
		if err != nil || m != c.min || spotSymbol(c.slug) != c.sym {
			t.Fatalf("%s: min=%d err=%v sym=%s", c.slug, m, err, spotSymbol(c.slug))
		}
	}
	if _, err := windowMinutes("btc-up-or-down-1h"); err == nil {
		t.Fatal("want error for unsupported window")
	}
}

func TestWriterRotatesHourly(t *testing.T) {
	dir := t.TempDir()
	w := &dayWriter{dir: dir}
	t0 := time.Date(2026, 11, 24, 10, 59, 0, 0, time.UTC)
	w.write(t0, []string{"a"})
	w.write(t0.Add(2*time.Minute), []string{"b"})
	w.close()
	for _, name := range []string{"predict_2026-11-24_10.csv", "predict_2026-11-24_11.csv"} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if lines := strings.Count(string(b), "\n"); lines != 2 { // заголовок + строка
			t.Fatalf("%s: %d lines", name, lines)
		}
	}
}
