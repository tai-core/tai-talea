// Package obs provides the observability primitives for the capacity plane:
// a dependency-free Prometheus text registry, structured alerting and helpers
// for building the structured logger.
package obs

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Metric kinds exposed through the text exposition format.
const (
	kindCounter = "counter"
	kindGauge   = "gauge"
	kindSummary = "summary"
)

type seriesKey struct {
	name   string
	labels string
}

type family struct {
	name      string
	help      string
	kind      string
	labelKeys []string
	series    map[string]*series
}

type series struct {
	labelValues []string
	value       float64
	count       uint64
	updatedAt   time.Time
}

// Registry is a minimal, concurrency-safe metric registry that renders the
// Prometheus text exposition format. It intentionally avoids a third-party
// dependency so the control plane stays a single static binary.
type Registry struct {
	mu       sync.RWMutex
	families map[string]*family
	order    []string
}

// NewRegistry creates an empty registry.
func NewRegistry() *Registry {
	return &Registry{families: map[string]*family{}}
}

func (r *Registry) family(name, help, kind string, labelKeys []string) *family {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.families[name]; ok {
		return existing
	}
	created := &family{name: name, help: help, kind: kind, labelKeys: labelKeys, series: map[string]*series{}}
	r.families[name] = created
	r.order = append(r.order, name)
	return created
}

func (f *family) resolve(labelValues []string) *series {
	key := strings.Join(labelValues, "\x00")
	if existing, ok := f.series[key]; ok {
		return existing
	}
	created := &series{labelValues: append([]string(nil), labelValues...)}
	f.series[key] = created
	return created
}

func pairsToValues(labelKeys []string, pairs []string) ([]string, error) {
	if len(pairs)%2 != 0 {
		return nil, fmt.Errorf("label pairs must be key/value couples, got %d elements", len(pairs))
	}
	if len(pairs)/2 != len(labelKeys) {
		return nil, fmt.Errorf("metric expects %d labels, got %d", len(labelKeys), len(pairs)/2)
	}
	values := make([]string, len(labelKeys))
	for index, key := range labelKeys {
		got := pairs[index*2]
		if got != key {
			return nil, fmt.Errorf("metric label %d must be %q, got %q", index, key, got)
		}
		values[index] = pairs[index*2+1]
	}
	return values, nil
}

// IncCounter increments a counter by one.
func (r *Registry) IncCounter(name, help string, labelKeys []string, pairs ...string) {
	r.AddCounter(name, help, labelKeys, 1, pairs...)
}

// AddCounter adds delta to a counter.
func (r *Registry) AddCounter(name, help string, labelKeys []string, delta float64, pairs ...string) {
	if r == nil {
		return
	}
	f := r.family(name, help, kindCounter, labelKeys)
	values, err := pairsToValues(labelKeys, pairs)
	if err != nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	s := f.resolve(values)
	s.value += delta
	s.updatedAt = time.Now()
}

// SetGauge sets a gauge to value.
func (r *Registry) SetGauge(name, help string, labelKeys []string, value float64, pairs ...string) {
	if r == nil {
		return
	}
	f := r.family(name, help, kindGauge, labelKeys)
	values, err := pairsToValues(labelKeys, pairs)
	if err != nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	s := f.resolve(values)
	s.value = value
	s.updatedAt = time.Now()
}

// Observe records one observation of a summary-style duration metric.
func (r *Registry) Observe(name, help string, labelKeys []string, value float64, pairs ...string) {
	if r == nil {
		return
	}
	f := r.family(name, help, kindSummary, labelKeys)
	values, err := pairsToValues(labelKeys, pairs)
	if err != nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	s := f.resolve(values)
	s.value += value
	s.count++
	s.updatedAt = time.Now()
}

// PointInTime returns the last gauge value, or zero when the series is absent.
func (r *Registry) PointInTime(name string, pairs ...string) float64 {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	f, ok := r.families[name]
	if !ok {
		return 0
	}
	key := strings.Join(seriesValues(f.labelKeys, pairs), "\x00")
	if s, ok := f.series[key]; ok {
		return s.value
	}
	return 0
}

func seriesValues(labelKeys []string, pairs []string) []string {
	values := make([]string, 0, len(labelKeys))
	for index, key := range labelKeys {
		position := index * 2
		if position+1 < len(pairs) && pairs[position] == key {
			values = append(values, pairs[position+1])
		} else {
			values = append(values, "")
		}
	}
	return values
}

// SeriesCount returns how many observations a summary metric recorded for one
// label set.
func (r *Registry) SeriesCount(name string, pairs ...string) uint64 {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	f, ok := r.families[name]
	if !ok {
		return 0
	}
	key := strings.Join(seriesValues(f.labelKeys, pairs), "\x00")
	if s, ok := f.series[key]; ok {
		return s.count
	}
	return 0
}

// Render produces the Prometheus text exposition payload.
func (r *Registry) Render() string {
	if r == nil {
		return ""
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	var builder strings.Builder
	names := append([]string(nil), r.order...)
	sort.Strings(names)
	for _, name := range names {
		f := r.families[name]
		if len(f.series) == 0 {
			continue
		}
		builder.WriteString("# HELP ")
		builder.WriteString(f.name)
		builder.WriteString(" ")
		builder.WriteString(f.help)
		builder.WriteString("\n# TYPE ")
		builder.WriteString(f.name)
		builder.WriteString(" ")
		builder.WriteString(f.kind)
		builder.WriteString("\n")

		keys := make([]string, 0, len(f.series))
		for key := range f.series {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			s := f.series[key]
			labels := renderLabels(f.labelKeys, s.labelValues)
			switch f.kind {
			case kindSummary:
				builder.WriteString(f.name)
				builder.WriteString("_sum")
				builder.WriteString(labels)
				builder.WriteString(" ")
				builder.WriteString(formatFloat(s.value))
				builder.WriteString("\n")
				builder.WriteString(f.name)
				builder.WriteString("_count")
				builder.WriteString(labels)
				builder.WriteString(" ")
				builder.WriteString(strconv.FormatUint(s.count, 10))
				builder.WriteString("\n")
			default:
				builder.WriteString(f.name)
				builder.WriteString(labels)
				builder.WriteString(" ")
				builder.WriteString(formatFloat(s.value))
				builder.WriteString("\n")
			}
		}
	}
	return builder.String()
}

func renderLabels(keys []string, values []string) string {
	if len(keys) == 0 {
		return ""
	}
	parts := make([]string, 0, len(keys))
	for index, key := range keys {
		value := ""
		if index < len(values) {
			value = values[index]
		}
		// The value is escaped here, so it must not pass through %q again.
		parts = append(parts, key+"=\""+escapeLabel(value)+"\"")
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func escapeLabel(value string) string {
	replacer := strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "\n", "\\n")
	return replacer.Replace(value)
}

func formatFloat(value float64) string {
	if value == float64(int64(value)) {
		return strconv.FormatInt(int64(value), 10)
	}
	return strconv.FormatFloat(value, 'g', -1, 64)
}
