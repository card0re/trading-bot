package binance

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// Binance Prediction (рынки predict.fun внутри Binance) — публичные эндпоинты
// веб-сайта, без ключа. shortcut: API недокументирован (подсмотрен у
// binance.com/prediction 2026-11-24) — при изменении формата парсеры ниже
// вернут ошибку; тогда обновить testdata/prediction_*.json и поля.
const predBase = "https://www.binance.com/bapi/defi/v1/public/wallet-direct/prediction/"

// PredWindow — одно окно up/down-рынка.
type PredWindow struct {
	Slug               string
	Start, End         int64 // unix ms
	MarketID           int64
	UpToken, DownToken string
	Status             string // REGISTERED / ... / RESOLVED
	Winner             string // "Up" / "Down" / "" пока не решено
}

// PredBook — верх стакана одного исхода. Цена доли 0.01–0.99, выплата $1.
type PredBook struct {
	BestBid, BestAsk float64
	HasBid, HasAsk   bool
	AskBestUSDT      float64 // сколько USDT можно купить по лучшему ask (цена·доли)
}

type predEnvelope struct {
	Code    string          `json:"code"`
	Message *string         `json:"message"`
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data"`
}

func unwrapPred(body []byte, v any) error {
	var env predEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return fmt.Errorf("prediction: не JSON: %w", err)
	}
	if env.Code != "000000" || !env.Success {
		msg := ""
		if env.Message != nil {
			msg = *env.Message
		}
		return fmt.Errorf("prediction: code=%s %s", env.Code, msg)
	}
	return json.Unmarshal(env.Data, v)
}

func parsePredWindow(body []byte) (PredWindow, error) {
	var d struct {
		EventSlug        string `json:"eventSlug"`
		StartDate        int64  `json:"startDate"`
		EndDate          int64  `json:"endDate"`
		UngroupedMarkets []struct {
			MarketID int64  `json:"marketId"`
			Status   string `json:"status"`
			Outcomes []struct {
				Name    string `json:"name"`
				TokenID string `json:"tokenId"`
				Winner  *bool  `json:"winner"`
			} `json:"outcomes"`
		} `json:"ungroupedMarkets"`
	}
	if err := unwrapPred(body, &d); err != nil {
		return PredWindow{}, err
	}
	if len(d.UngroupedMarkets) != 1 {
		return PredWindow{}, fmt.Errorf("prediction: ожидался 1 рынок в окне, получено %d", len(d.UngroupedMarkets))
	}
	m := d.UngroupedMarkets[0]
	w := PredWindow{Slug: d.EventSlug, Start: d.StartDate, End: d.EndDate, MarketID: m.MarketID, Status: m.Status}
	for _, o := range m.Outcomes {
		switch o.Name {
		case "Up":
			w.UpToken = o.TokenID
		case "Down":
			w.DownToken = o.TokenID
		}
		if o.Winner != nil && *o.Winner {
			w.Winner = o.Name
		}
	}
	if w.MarketID == 0 || w.UpToken == "" || w.DownToken == "" {
		return PredWindow{}, fmt.Errorf("prediction: в окне нет marketId/токенов Up/Down")
	}
	return w, nil
}

func parsePredBook(body []byte) (PredBook, error) {
	type level struct{ Price, Size float64 }
	var d struct{ Bids, Asks []level }
	if err := unwrapPred(body, &d); err != nil {
		return PredBook{}, err
	}
	var b PredBook
	for _, l := range d.Bids {
		if !b.HasBid || l.Price > b.BestBid {
			b.BestBid, b.HasBid = l.Price, true
		}
	}
	for _, l := range d.Asks {
		if !b.HasAsk || l.Price < b.BestAsk {
			b.BestAsk, b.HasAsk, b.AskBestUSDT = l.Price, true, 0
		}
		if l.Price == b.BestAsk {
			b.AskBestUSDT += l.Price * l.Size
		}
	}
	return b, nil
}

func predPost(ctx context.Context, hc *http.Client, path string, body any) ([]byte, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, predBase+path, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("prediction %s: HTTP %d", path, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// FetchPredWindow — окно рынка slug (например "btc-up-or-down-5m"), начинающееся в startMs.
func FetchPredWindow(ctx context.Context, hc *http.Client, slug string, startMs int64) (PredWindow, error) {
	body, err := predPost(ctx, hc, "web/market/event/detail", map[string]any{"eventSlug": slug, "startDate": startMs})
	if err != nil {
		return PredWindow{}, err
	}
	return parsePredWindow(body)
}

// FetchPredBook — стакан одного исхода (tokenId) рынка marketID.
func FetchPredBook(ctx context.Context, hc *http.Client, marketID int64, tokenID string) (PredBook, error) {
	body, err := predPost(ctx, hc, "msite-web/order-book", map[string]any{"marketId": marketID, "tokenId": tokenID, "vendor": "PREDICT_FUN"})
	if err != nil {
		return PredBook{}, err
	}
	return parsePredBook(body)
}
