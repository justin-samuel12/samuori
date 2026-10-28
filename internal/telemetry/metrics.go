package telemetry

import (
	"context"
	"crypto/subtle"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Sample is one gauge value computed when metrics are scraped.
type Sample struct {
	Name, Help string
	Labels     map[string]string
	Value      float64
}

// Gauges returns the current gauge samples, usually from the database.
type Gauges func(ctx context.Context) ([]Sample, error)

// Metrics holds Samuori's Prometheus registry. It has no tenant labels, so one scrape never reveals another
// organization's names (docs/architecture/observability.md#metrics).
type Metrics struct {
	reg      *prometheus.Registry
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
}

func NewMetrics(gauges Gauges) *Metrics {
	m := &Metrics{
		reg: prometheus.NewRegistry(),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "samuori_http_requests_total", Help: "API requests by operation, method and status code.",
		}, []string{"operation", "method", "code"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "samuori_http_request_duration_seconds", Help: "API request latency by operation.", Buckets: prometheus.DefBuckets,
		}, []string{"operation"}),
	}
	m.reg.MustRegister(m.requests, m.duration, collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	if gauges != nil {
		m.reg.MustRegister(newGaugeCollector(gauges))
	}
	return m
}

// ObserveRequest records one API request; operation is the OpenAPI operation ID, or "unmatched".
func (m *Metrics) ObserveRequest(operation, method string, code int, d time.Duration) {
	if m == nil {
		return
	}
	if operation == "" {
		operation = "unmatched"
	}
	m.requests.WithLabelValues(operation, method, strconv.Itoa(code)).Inc()
	m.duration.WithLabelValues(operation).Observe(d.Seconds())
}

// Handler serves the Prometheus text format. With a token, scrapes must send it as a bearer token.
func (m *Metrics) Handler(token string) http.Handler {
	h := promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{ErrorHandling: promhttp.ContinueOnError})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token != "" && subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="samuori-metrics"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// gaugeCollector turns samples into constant metrics at scrape time, caching them briefly so frequent scrapes do
// not load the database.
type gaugeCollector struct {
	gauges Gauges
	errors prometheus.Counter
	mu     sync.Mutex
	cached []prometheus.Metric
	at     time.Time
}

func newGaugeCollector(g Gauges) *gaugeCollector {
	return &gaugeCollector{gauges: g, errors: prometheus.NewCounter(prometheus.CounterOpts{
		Name: "samuori_metrics_snapshot_errors_total", Help: "Scrapes whose state snapshot failed.",
	})}
}

func (c *gaugeCollector) Describe(ch chan<- *prometheus.Desc) { c.errors.Describe(ch) }

func (c *gaugeCollector) Collect(ch chan<- prometheus.Metric) {
	c.errors.Collect(ch)
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.at) > 10*time.Second {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		samples, err := c.gauges(ctx)
		cancel()
		if err != nil {
			c.errors.Inc()
		} else {
			c.cached, c.at = toMetrics(samples), time.Now()
		}
	}
	for _, m := range c.cached {
		ch <- m
	}
}

func toMetrics(samples []Sample) []prometheus.Metric {
	descs := map[string]*prometheus.Desc{}
	var out []prometheus.Metric
	for _, s := range samples {
		names := make([]string, 0, len(s.Labels))
		for k := range s.Labels {
			names = append(names, k)
		}
		slices.Sort(names)
		key := s.Name
		for _, n := range names {
			key += "\x00" + n
		}
		d, ok := descs[key]
		if !ok {
			d = prometheus.NewDesc(s.Name, s.Help, names, nil)
			descs[key] = d
		}
		values := make([]string, len(names))
		for i, n := range names {
			values[i] = s.Labels[n]
		}
		if m, err := prometheus.NewConstMetric(d, prometheus.GaugeValue, s.Value, values...); err == nil {
			out = append(out, m)
		}
	}
	return out
}
