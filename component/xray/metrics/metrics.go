package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	"go.uber.org/atomic"

	"github.com/prometheus/prometheus/storage"

	"github.com/grafana/agent/component"
	"github.com/grafana/agent/component/prometheus"
	"github.com/grafana/agent/pkg/flow/logging/level"
	"github.com/grafana/agent/service/labelstore"
	"github.com/prometheus/prometheus/model/exemplar"
	"github.com/prometheus/prometheus/model/histogram"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/metadata"
)

func init() {
	component.Register(component.Registration{
		Name:    "xray.metrics",
		Args:    Arguments{},
		Exports: Exports{},

		Build: func(o component.Options, c component.Arguments) (component.Component, error) {
			return New(o, c.(Arguments))
		},
	})
}

type Arguments struct {
}

type Exports struct {
	Receiver storage.Appendable `river:"receiver,attr"`
}

// Component implements the prometheus.relabel component.
type Component struct {
	mut      sync.RWMutex
	opts     component.Options
	receiver *prometheus.Interceptor

	fanout *prometheus.Fanout
	exited atomic.Bool
	ls     labelstore.LabelStore

	allSeries map[string]*SeriesSummary
	cacheMut  sync.RWMutex
}

type SeriesSummary struct {
	Labels     labels.Labels
	LabelsStr  string  `river:"labels,attr"`
	DataPoints uint64  `river:"dataPoints,attr"`
	LastValue  float64 `river:"last,attr"`
}

// {a=1,b=2} 234 last=42.65

// Queries:

// /reset
// /summary?label=__name__&label=job
// /details?job=integration/agent&__name__=cpu_requests

// Summarize by __name__
// cpu_whatever - 234 series (2k dp)
// mem_foo - 15 series (15k dp)

// Series Details {__name__ = cpu_whatever}
// {a=b} 1k dp last=23

var (
	_ component.Component = (*Component)(nil)
)

func New(o component.Options, args Arguments) (*Component, error) {
	data, err := o.GetServiceData(labelstore.ServiceName)
	if err != nil {
		return nil, err
	}
	c := &Component{
		opts:      o,
		ls:        data.(labelstore.LabelStore),
		allSeries: map[string]*SeriesSummary{},
	}

	c.fanout = prometheus.NewFanout(nil, o.ID, o.Registerer, c.ls)
	c.receiver = prometheus.NewInterceptor(
		c.fanout,
		c.ls,
		prometheus.WithAppendHook(func(_ storage.SeriesRef, l labels.Labels, t int64, v float64, next storage.Appender) (storage.SeriesRef, error) {
			if c.exited.Load() {
				return 0, fmt.Errorf("%s has exited", o.ID)
			}
			c.cacheMut.Lock()
			labelStr := l.String()
			ss := c.allSeries[labelStr]
			if ss == nil {
				ss = &SeriesSummary{
					Labels:    l,
					LabelsStr: labelStr,
				}
				c.allSeries[labelStr] = ss
			}
			ss.DataPoints++
			ss.LastValue = v
			c.cacheMut.Unlock()
			return 0, nil
		}),
		// TODO: these do nothing
		prometheus.WithExemplarHook(func(_ storage.SeriesRef, l labels.Labels, e exemplar.Exemplar, next storage.Appender) (storage.SeriesRef, error) {
			if c.exited.Load() {
				return 0, fmt.Errorf("%s has exited", o.ID)
			}
			return 0, nil
		}),
		prometheus.WithMetadataHook(func(_ storage.SeriesRef, l labels.Labels, m metadata.Metadata, next storage.Appender) (storage.SeriesRef, error) {
			if c.exited.Load() {
				return 0, fmt.Errorf("%s has exited", o.ID)
			}
			return 0, nil
		}),
		prometheus.WithHistogramHook(func(_ storage.SeriesRef, l labels.Labels, t int64, h *histogram.Histogram, fh *histogram.FloatHistogram, next storage.Appender) (storage.SeriesRef, error) {
			if c.exited.Load() {
				return 0, fmt.Errorf("%s has exited", o.ID)
			}
			return 0, nil
		}),
	)

	// Immediately export the receiver which remains the same for the component
	// lifetime.
	o.OnStateChange(Exports{Receiver: c.receiver})

	// Call to Update() to set the relabelling rules once at the start.
	if err = c.Update(args); err != nil {
		return nil, err
	}

	return c, nil
}

// Run implements component.Component.
func (c *Component) Run(ctx context.Context) error {
	defer c.exited.Store(true)

	<-ctx.Done()
	return nil
}

// Update implements component.Component.
func (c *Component) Update(args component.Arguments) error {
	c.mut.Lock()
	defer c.mut.Unlock()

	//c.clearCache(100_000)

	return nil
}

// ScraperStatus reports the status of the scraper's jobs.
type debugInfo struct {
	Metrics map[string]*summary `river:"metrics,attr"`
	Jobs    map[string]*summary `river:"jobs,attr"`
	Details []*SeriesSummary    `river:"details,attr"`
}

// DebugInfo implements component.DebugComponent
func (c *Component) DebugInfo() interface{} {

	c.cacheMut.RLock()
	di := &debugInfo{
		Metrics: c.Summarize("__name__"),
		Jobs:    c.Summarize("job"),
		Details: c.Details(map[string]string{"__name__": "prometheus_sd_kubernetes_events_total"}),
	}
	c.cacheMut.RUnlock()
	return di
}

type summary struct {
	Labels              labels.Labels
	SeriesCount         int `river:"series_count,attr" json:"series_count"`
	DataPointCountTotal int `river:"data_point_count_total,attr" json:"data_point_count_total"`
}

// summarize by __name__
// summarize by job
// summarize by __name__,job
func (c *Component) Summarize(ls ...string) map[string]*summary {
	summaries := map[string]*summary{}
	for _, v := range c.allSeries {
		thisLabels := map[string]string{}
		for _, l := range ls {
			thisLabels[l] = v.Labels.Get(l)
		}
		labs := labels.FromMap(thisLabels)
		summ := summaries[labs.String()]
		if summ == nil {
			summ = &summary{
				Labels: labs,
			}
			summaries[labs.String()] = summ
		}
		summ.DataPointCountTotal += int(v.DataPoints)
		summ.SeriesCount++
	}
	return summaries
}

// details for __name__ = prometheus_sd_kubernetes_events_total
func (c *Component) Details(matchers map[string]string) []*SeriesSummary {
	matches := []*SeriesSummary{}
	for _, s := range c.allSeries {
		match := true
		for k, v := range matchers {
			if s.Labels.Get(k) != v {
				match = false
				break
			}
		}
		if match {
			matches = append(matches, s)
		}
	}
	return matches
}

func (c *Component) Handler() http.Handler {
	router := http.NewServeMux()

	router.HandleFunc("/summary", func(w http.ResponseWriter, r *http.Request) {
		params := r.URL.Query()
		ls := append([]string{}, params["label"]...)

		summaries := c.Summarize(ls...)

		w.Header().Set("Content-Type", "application/json")
		err := json.NewEncoder(w).Encode(summaries)
		if err != nil {
			level.Error(c.opts.Logger).Log("msg", "failed to encode json", "err", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})

	router.HandleFunc("/details", func(w http.ResponseWriter, r *http.Request) {
		params := r.URL.Query()
		ls := map[string]string{}
		for k, v := range params {
			ls[k] = v[0]
		}

		details := c.Details(ls)

		w.Header().Set("Content-Type", "application/json")
		err := json.NewEncoder(w).Encode(details)
		if err != nil {
			level.Error(c.opts.Logger).Log("msg", "failed to encode json", "err", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})

	router.HandleFunc("/clear", func(w http.ResponseWriter, r *http.Request) {
		c.cacheMut.Lock()
		defer c.cacheMut.Unlock()
		c.allSeries = map[string]*SeriesSummary{}
	})

	return router
}
