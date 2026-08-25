// Package sentiment грузит Fear & Greed Index (alternative.me) — бесплатный,
// без ключа, дневная история с 2018-02-01. Единственный источник данных в
// проекте, независимый от самой Binance (цена/объём/funding — всё оттуда).
package sentiment

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// DailyIndex — значение индекса (0=Extreme Fear .. 100=Extreme Greed) на
// календарный день (UTC, 00:00).
type DailyIndex struct {
	Date  time.Time
	Value int
}

type apiResponse struct {
	Data []struct {
		Value     string `json:"value"`
		Timestamp string `json:"timestamp"`
	} `json:"data"`
}

// FetchHistory тянет всю доступную историю (limit=0 у API значит "всё").
func FetchHistory(ctx context.Context) ([]DailyIndex, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.alternative.me/fng/?limit=0&format=json", nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("запрос Fear&Greed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Fear&Greed API вернул статус %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var parsed apiResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("разбор ответа Fear&Greed: %w", err)
	}

	out := make([]DailyIndex, 0, len(parsed.Data))
	for _, d := range parsed.Data {
		v, err := strconv.Atoi(d.Value)
		if err != nil {
			continue // пропускаем неразобранную точку, не валим всю историю
		}
		ts, err := strconv.ParseInt(d.Timestamp, 10, 64)
		if err != nil {
			continue
		}
		out = append(out, DailyIndex{
			Date:  time.Unix(ts, 0).UTC().Truncate(24 * time.Hour),
			Value: v,
		})
	}
	return out, nil
}

// ByDate индексирует историю по календарному дню (UTC) для O(1) поиска.
func ByDate(history []DailyIndex) map[time.Time]int {
	m := make(map[time.Time]int, len(history))
	for _, d := range history {
		m[d.Date] = d.Value
	}
	return m
}
