// Package metrics — минимальный сериализатор в формат Prometheus text
// exposition (https://prometheus.io/docs/instrumenting/exposition_formats/).
// Без внешней зависимости: формат — это несколько строк текста, тянуть ради
// него клиентскую библиотеку незачем ни у одного из двух ботов в этом репо.
package metrics

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

// Metric — одно значение метрики с опциональными метками.
type Metric struct {
	Name   string
	Help   string
	Type   string // "gauge" | "counter"
	Value  float64
	Labels map[string]string // может быть nil
}

// WriteText сериализует метрики в Prometheus text format. HELP/TYPE строки
// пишутся один раз на имя метрики — вызывающий сам группирует метрики с
// одинаковым Name подряд (все Collect-функции в этом репо так и делают).
func WriteText(w io.Writer, metrics []Metric) error {
	seenHeader := make(map[string]bool, len(metrics))
	for _, m := range metrics {
		if !seenHeader[m.Name] {
			if _, err := fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", m.Name, m.Help, m.Name, m.Type); err != nil {
				return err
			}
			seenHeader[m.Name] = true
		}
		if len(m.Labels) == 0 {
			if _, err := fmt.Fprintf(w, "%s %g\n", m.Name, m.Value); err != nil {
				return err
			}
			continue
		}
		keys := make([]string, 0, len(m.Labels))
		for k := range m.Labels {
			keys = append(keys, k)
		}
		sort.Strings(keys) // детерминированный порядок — иначе диффы между скрейпами шумят почём зря
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = fmt.Sprintf("%s=%q", k, m.Labels[k])
		}
		if _, err := fmt.Fprintf(w, "%s{%s} %g\n", m.Name, strings.Join(parts, ","), m.Value); err != nil {
			return err
		}
	}
	return nil
}

// Handler отдаёт /metrics, вызывая collect заново на каждый скрейп — свежие
// данные вместо кэша, скрейпы Prometheus редкие (обычно раз в 15-60с), а сам
// сбор здесь дешёвый (локальные значения + максимум один REST-вызов эквити).
func Handler(collect func(ctx context.Context) []Metric) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_ = WriteText(w, collect(r.Context()))
	})
}
