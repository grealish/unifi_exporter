// Package unifiexporter provides the Exporter type used in the unifi_exporter
// Prometheus exporter.
package unifiexporter

import (
	"strings"
	"sync"

	"github.com/mdlayher/unifi"
	"github.com/prometheus/client_golang/prometheus"
)

const (
	// namespace is the top-level namespace for this UniFi exporter.
	namespace = "unifi"
)

// An Exporter is a Prometheus exporter for Ubiquiti UniFi Controller API
// metrics.  It wraps all UniFi metrics collectors and provides a single global
// exporter which can serve metrics. It also ensures that the collection
// is done in a thread-safe manner, the necessary requirement stated by
// Prometheus. It implements the prometheus.Collector interface in order to
// register with Prometheus.
type Exporter struct {
	mu         sync.Mutex
	collectors []prometheus.Collector

	errC chan error

	haltMu sync.RWMutex
	halt   bool

	done func()
}

// Verify that the Exporter implements the prometheus.Collector interface.
var _ prometheus.Collector = &Exporter{}

// New creates a new Exporter which collects metrics from one or more sites.
//
// Once the Exporter is created, call its ErrC method to retrieve a channel
// of incoming errors encountered during metrics collection.  This channel
// must be drained for metrics collection to proceed.
//
// When the Exporter is no longer needed, call its Close method to clean
// up its resources.
func New(c *unifi.Client, sites []*unifi.Site) *Exporter {
	dc := NewDeviceCollector(c, sites)
	sc := NewStationCollector(c, sites)

	errC := make(chan error)

	e := &Exporter{
		errC: errC,

		collectors: []prometheus.Collector{
			dc,
			sc,
		},
	}

	var wg sync.WaitGroup
	doneC := make(chan struct{})

	// Fan in errors from external error channels to Exporter's error channel
	wg.Add(1)
	go e.fanInErrors(
		&wg,
		doneC,
		dc.ErrC(),
		sc.ErrC(),
	)

	// Clean up collectors and error fan-in goroutine when Close is called later
	e.done = func() {
		dc.Close()
		sc.Close()

		close(doneC)
		wg.Wait()

		close(errC)
	}

	return e
}

// ErrC returns a channel of incoming errors encountered during metrics
// collection.  This channel must be drained for metrics collection
// to proceed.
func (e *Exporter) ErrC() <-chan error {
	return e.errC
}

// Close halts all metric collection activity and cleans up the
// Exporter's resources when it is no longer needed.
func (e *Exporter) Close() {
	e.haltMu.Lock()
	defer e.haltMu.Unlock()

	e.halt = true

	e.done()
}

// Describe sends all the descriptors of the collectors included to
// the provided channel.
func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {
	e.haltMu.Lock()
	defer e.haltMu.Unlock()

	if e.halt {
		return
	}

	for _, cc := range e.collectors {
		cc.Describe(ch)
	}
}

// Collect sends the collected metrics from each of the collectors to
// prometheus. Collect could be called several times concurrently
// and thus its run is protected by a single mutex.
func (e *Exporter) Collect(ch chan<- prometheus.Metric) {
	e.haltMu.Lock()
	defer e.haltMu.Unlock()

	if e.halt {
		return
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	for _, cc := range e.collectors {
		cc.Collect(ch)
	}
}

// fanInErrors collectors errors from incoming channels and fans them into
// the Exporter's error channel.
// Reference: https://blog.golang.org/pipelines.
func (e *Exporter) fanInErrors(
	wg *sync.WaitGroup,
	doneC chan struct{},
	errCs ...<-chan error,
) {
	var fanWG sync.WaitGroup

	out := func(errC <-chan error) {
		for err := range errC {
			e.errC <- err
		}
		fanWG.Done()
	}

	fanWG.Add(len(errCs))
	for _, errC := range errCs {
		go out(errC)
	}

	select {
	case <-doneC:
		fanWG.Wait()
		wg.Done()
		return
	}
}

// siteDescription a metric label value for Prometheus metrics by
// normalizing the a site description field.
func siteDescription(desc string) string {
	desc = strings.ToLower(desc)
	return strings.Map(func(r rune) rune {
		switch r {
		// TODO(mdlayher): figure out valid set of characters for description
		case ' ', '-', '_', '.':
			return -1
		}

		return r
	}, desc)
}
