package metrics

import (
	"fmt"
	"io"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

type metricFamily struct {
	name    string
	help    string
	typ     string
	samples []string
}

func escapeLabelValue(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// WriteMetrics 把当前收集的所有指标以 Prometheus 0.0.4 纯文本格式写入 w。
func (c *Collector) WriteMetrics(w io.Writer, g Gauges) error {
	var families []*metricFamily

	// 1. uaig_audit_dropped_total (counter)
	if g.AuditDropped != nil {
		dropped := g.AuditDropped()
		families = append(families, &metricFamily{
			name:    "uaig_audit_dropped_total",
			help:    "Total number of audit records dropped due to full queue.",
			typ:     "counter",
			samples: []string{fmt.Sprintf("uaig_audit_dropped_total %d", dropped)},
		})
	}

	// 2. uaig_build_info (gauge)
	if g.Version != "" {
		families = append(families, &metricFamily{
			name: "uaig_build_info",
			help: "Gateway build and version information.",
			typ:  "gauge",
			samples: []string{
				fmt.Sprintf(`uaig_build_info{version="%s"} 1`, escapeLabelValue(g.Version)),
			},
		})
	}

	// 3. uaig_goroutines (gauge)
	numGoroutines := runtime.NumGoroutine()
	if g.Goroutines != nil {
		numGoroutines = g.Goroutines()
	}
	families = append(families, &metricFamily{
		name:    "uaig_goroutines",
		help:    "Number of goroutines that currently exist.",
		typ:     "gauge",
		samples: []string{fmt.Sprintf("uaig_goroutines %d", numGoroutines)},
	})

	// 4. uaig_key_inflight (gauge) & uaig_keys_status (gauge)
	if g.Keys != nil {
		keys := g.Keys()
		var inflightSamples []string
		var statusSamples []string

		candidateStates := []string{"ready", "quarantined", "disabled", "cooldown", "breaker_open"}

		for _, k := range keys {
			idStr := strconv.FormatInt(k.ID, 10)
			// uaig_key_inflight{pool="...",id="...",name="..."}
			inflightSamples = append(inflightSamples, fmt.Sprintf(
				`uaig_key_inflight{id="%s",name="%s",pool="%s"} %d`,
				escapeLabelValue(idStr),
				escapeLabelValue(k.Name),
				escapeLabelValue(k.Pool),
				k.InFlight,
			))

			// uaig_keys_status{pool="...",id="...",name="...",state="..."}
			for _, st := range candidateStates {
				val := 0
				switch st {
				case "ready":
					if k.Persisted == "ready" {
						val = 1
					}
				case "quarantined":
					if k.Persisted == "quarantined" {
						val = 1
					}
				case "disabled":
					if k.Persisted == "disabled" {
						val = 1
					}
				case "cooldown":
					if k.CooldownRemaining > 0 {
						val = 1
					}
				case "breaker_open":
					if k.BreakerOpen {
						val = 1
					}
				}
				statusSamples = append(statusSamples, fmt.Sprintf(
					`uaig_keys_status{id="%s",name="%s",pool="%s",state="%s"} %d`,
					escapeLabelValue(idStr),
					escapeLabelValue(k.Name),
					escapeLabelValue(k.Pool),
					st,
					val,
				))
			}
		}

		if len(inflightSamples) > 0 {
			sort.Strings(inflightSamples)
			families = append(families, &metricFamily{
				name:    "uaig_key_inflight",
				help:    "Current in-flight requests for an upstream key.",
				typ:     "gauge",
				samples: inflightSamples,
			})
		}

		if len(statusSamples) > 0 {
			sort.Strings(statusSamples)
			families = append(families, &metricFamily{
				name:    "uaig_keys_status",
				help:    "Status of upstream keys across candidate states.",
				typ:     "gauge",
				samples: statusSamples,
			})
		}
	}

	// 5. uaig_ledger_events_queue_capacity & uaig_ledger_events_queue_length (gauge)
	if g.LedgerQueue != nil {
		qLen, qCap := g.LedgerQueue()
		families = append(families, &metricFamily{
			name:    "uaig_ledger_events_queue_capacity",
			help:    "Capacity of the ledger billing events queue.",
			typ:     "gauge",
			samples: []string{fmt.Sprintf("uaig_ledger_events_queue_capacity %d", qCap)},
		})
		families = append(families, &metricFamily{
			name:    "uaig_ledger_events_queue_length",
			help:    "Current length of the ledger billing events queue.",
			typ:     "gauge",
			samples: []string{fmt.Sprintf("uaig_ledger_events_queue_length %d", qLen)},
		})
	}

	// 6. uaig_overload_inflight_body_bytes & uaig_overload_inflight_requests (gauge)
	if g.InFlight != nil {
		reqs, bytes := g.InFlight()
		families = append(families, &metricFamily{
			name:    "uaig_overload_inflight_body_bytes",
			help:    "Current in-flight buffered request body bytes in overload guard.",
			typ:     "gauge",
			samples: []string{fmt.Sprintf("uaig_overload_inflight_body_bytes %d", bytes)},
		})
		families = append(families, &metricFamily{
			name:    "uaig_overload_inflight_requests",
			help:    "Current in-flight requests in overload guard.",
			typ:     "gauge",
			samples: []string{fmt.Sprintf("uaig_overload_inflight_requests %d", reqs)},
		})
	}

	// 7. uaig_start_time_seconds (gauge)
	if !g.StartedAt.IsZero() {
		families = append(families, &metricFamily{
			name:    "uaig_start_time_seconds",
			help:    "Process start time in seconds since Unix epoch.",
			typ:     "gauge",
			samples: []string{fmt.Sprintf("uaig_start_time_seconds %d", g.StartedAt.Unix())},
		})
	}

	// 8. uaig_requests_total (counter)
	c.mu.RLock()
	var reqSamples []string
	for k, cnt := range c.requests {
		v := cnt.Load()
		reqSamples = append(reqSamples, fmt.Sprintf(
			`uaig_requests_total{code="%s",ingress="%s",model="%s",outcome="%s",upstream="%s"} %d`,
			escapeLabelValue(k.code),
			escapeLabelValue(k.ingress),
			escapeLabelValue(k.model),
			escapeLabelValue(k.outcome),
			escapeLabelValue(k.upstream),
			v,
		))
	}
	if len(reqSamples) > 0 {
		sort.Strings(reqSamples)
		families = append(families, &metricFamily{
			name:    "uaig_requests_total",
			help:    "Total number of completed requests.",
			typ:     "counter",
			samples: reqSamples,
		})
	}

	// 9. uaig_request_errors_total (counter)
	var errSamples []string
	for k, cnt := range c.errors {
		v := cnt.Load()
		errSamples = append(errSamples, fmt.Sprintf(
			`uaig_request_errors_total{error_code="%s",model="%s"} %d`,
			escapeLabelValue(k.errorCode),
			escapeLabelValue(k.model),
			v,
		))
	}
	if len(errSamples) > 0 {
		sort.Strings(errSamples)
		families = append(families, &metricFamily{
			name:    "uaig_request_errors_total",
			help:    "Total number of request errors by model and error code.",
			typ:     "counter",
			samples: errSamples,
		})
	}

	// 10. uaig_upstream_attempts_total (counter)
	var attSamples []string
	for k, cnt := range c.attempts {
		v := cnt.Load()
		attSamples = append(attSamples, fmt.Sprintf(
			`uaig_upstream_attempts_total{model="%s",upstream="%s"} %d`,
			escapeLabelValue(k.model),
			escapeLabelValue(k.upstream),
			v,
		))
	}
	if len(attSamples) > 0 {
		sort.Strings(attSamples)
		families = append(families, &metricFamily{
			name:    "uaig_upstream_attempts_total",
			help:    "Total number of upstream attempts.",
			typ:     "counter",
			samples: attSamples,
		})
	}

	// 11. uaig_request_duration_seconds (histogram)
	type histEntry struct {
		key  histKey
		snap histSnapshot
	}
	var durEntries []histEntry
	for k, h := range c.duration {
		durEntries = append(durEntries, histEntry{key: k, snap: h.snapshot()})
	}
	sort.Slice(durEntries, func(i, j int) bool {
		if durEntries[i].key.model != durEntries[j].key.model {
			return durEntries[i].key.model < durEntries[j].key.model
		}
		return durEntries[i].key.stream < durEntries[j].key.stream
	})

	var durSamples []string
	for _, entry := range durEntries {
		m := escapeLabelValue(entry.key.model)
		s := escapeLabelValue(entry.key.stream)
		snap := entry.snap

		for i, b := range snap.bounds {
			bStr := strconv.FormatFloat(b, 'g', -1, 64)
			durSamples = append(durSamples, fmt.Sprintf(
				`uaig_request_duration_seconds_bucket{model="%s",stream="%s",le="%s"} %d`,
				m, s, bStr, snap.buckets[i],
			))
		}
		durSamples = append(durSamples, fmt.Sprintf(
			`uaig_request_duration_seconds_bucket{model="%s",stream="%s",le="+Inf"} %d`,
			m, s, snap.count,
		))
		durSamples = append(durSamples, fmt.Sprintf(
			`uaig_request_duration_seconds_sum{model="%s",stream="%s"} %s`,
			m, s, strconv.FormatFloat(snap.sum, 'g', -1, 64),
		))
		durSamples = append(durSamples, fmt.Sprintf(
			`uaig_request_duration_seconds_count{model="%s",stream="%s"} %d`,
			m, s, snap.count,
		))
	}
	if len(durSamples) > 0 {
		families = append(families, &metricFamily{
			name:    "uaig_request_duration_seconds",
			help:    "End-to-end request latency in seconds.",
			typ:     "histogram",
			samples: durSamples,
		})
	}

	// 12. uaig_ttfb_seconds (histogram)
	var ttfbEntries []histEntry
	for k, h := range c.ttfb {
		ttfbEntries = append(ttfbEntries, histEntry{key: k, snap: h.snapshot()})
	}
	sort.Slice(ttfbEntries, func(i, j int) bool {
		if ttfbEntries[i].key.model != ttfbEntries[j].key.model {
			return ttfbEntries[i].key.model < ttfbEntries[j].key.model
		}
		return ttfbEntries[i].key.stream < ttfbEntries[j].key.stream
	})

	var ttfbSamples []string
	for _, entry := range ttfbEntries {
		m := escapeLabelValue(entry.key.model)
		s := escapeLabelValue(entry.key.stream)
		snap := entry.snap

		for i, b := range snap.bounds {
			bStr := strconv.FormatFloat(b, 'g', -1, 64)
			ttfbSamples = append(ttfbSamples, fmt.Sprintf(
				`uaig_ttfb_seconds_bucket{model="%s",stream="%s",le="%s"} %d`,
				m, s, bStr, snap.buckets[i],
			))
		}
		ttfbSamples = append(ttfbSamples, fmt.Sprintf(
			`uaig_ttfb_seconds_bucket{model="%s",stream="%s",le="+Inf"} %d`,
			m, s, snap.count,
		))
		ttfbSamples = append(ttfbSamples, fmt.Sprintf(
			`uaig_ttfb_seconds_sum{model="%s",stream="%s"} %s`,
			m, s, strconv.FormatFloat(snap.sum, 'g', -1, 64),
		))
		ttfbSamples = append(ttfbSamples, fmt.Sprintf(
			`uaig_ttfb_seconds_count{model="%s",stream="%s"} %d`,
			m, s, snap.count,
		))
	}
	if len(ttfbSamples) > 0 {
		families = append(families, &metricFamily{
			name:    "uaig_ttfb_seconds",
			help:    "Time to first byte in seconds.",
			typ:     "histogram",
			samples: ttfbSamples,
		})
	}
	c.mu.RUnlock()

	// 稳定排序：按指标家族名称升序
	sort.Slice(families, func(i, j int) bool {
		return families[i].name < families[j].name
	})

	// 依次写出指标家族
	for _, f := range families {
		if len(f.samples) == 0 {
			continue
		}
		if _, err := fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", f.name, f.help, f.name, f.typ); err != nil {
			return err
		}
		for _, s := range f.samples {
			if _, err := fmt.Fprintf(w, "%s\n", s); err != nil {
				return err
			}
		}
	}

	return nil
}
