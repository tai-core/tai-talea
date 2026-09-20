// Package telemetry samples SGLang and Router without blocking lifecycle work.
package telemetry

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"regexp"
	"strconv"
	"strings"
)

var sampleLine = regexp.MustCompile(`^([a-zA-Z_:][a-zA-Z0-9_:]*)(\{.*\})?\s+([^\s]+)(?:\s+.*)?$`)
var labelPair = regexp.MustCompile(`([a-zA-Z_][a-zA-Z0-9_]*)="((?:[^"\\]|\\.)*)"`)

// parseMetrics selects scheduler totals (priority="") to avoid counting both
// the total and its priority breakdown. SGLang exports one scheduler per DP
// rank, not one copy per TP rank. Counts sum across DP; cache uses the maximum.
func parseMetrics(r io.Reader) (map[string]float64, error) {
	values := map[string]float64{}
	scan := bufio.NewScanner(r)
	scan.Buffer(make([]byte, 4096), 1<<20)
	for scan.Scan() {
		line := strings.TrimSpace(scan.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		m := sampleLine.FindStringSubmatch(line)
		if m == nil {
			return nil, fmt.Errorf("invalid metric sample")
		}
		if !strings.HasPrefix(m[1], "sglang:") {
			continue
		}
		v, err := strconv.ParseFloat(m[3], 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			continue
		}
		labels := map[string]string{}
		for _, pair := range labelPair.FindAllStringSubmatch(m[2], -1) {
			labels[pair[1]] = pair[2]
		}
		if labels["priority"] != "" {
			continue
		}
		key := strings.TrimPrefix(m[1], "sglang:")
		if key == "realtime_tokens_total" {
			key += ":" + labels["mode"]
		}
		if key == "token_usage" {
			values[key] = math.Max(values[key], v)
		} else {
			values[key] += v
		}
	}
	return values, scan.Err()
}

func number(v float64) *float64 { return &v }
func value(m map[string]float64, key string) *float64 {
	v, ok := m[key]
	if !ok {
		return nil
	}
	return number(v)
}
func delta(now, before map[string]float64, key string, seconds float64) *float64 {
	a, aok := now[key]
	b, bok := before[key]
	if !aok || !bok || seconds <= 0 || seconds > 20 || a < b {
		return nil
	}
	return number((a - b) / seconds)
}
