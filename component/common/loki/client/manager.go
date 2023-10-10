package client

import (
	"fmt"
	"strings"
	"sync"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/grafana/agent/component/common/loki"
	"github.com/grafana/agent/component/common/loki/limit"
	"github.com/grafana/agent/component/common/loki/wal"
)

// WriterEventsNotifier implements a notifier that's received by the Manager, to which wal.Watcher can subscribe for
// writer events.
type WriterEventsNotifier interface {
	SubscribeCleanup(subscriber wal.CleanupEventSubscriber)
	SubscribeWrite(subscriber wal.WriteEventSubscriber)
}

var (
	// NilNotifier is a no-op WriterEventsNotifier.
	NilNotifier = nilNotifier{}

	NilMarker = nilMarker{}
)

// nilNotifier implements WriterEventsNotifier with no-ops callbacks.
type nilNotifier struct{}

func (n nilNotifier) SubscribeCleanup(_ wal.CleanupEventSubscriber) {}

func (n nilNotifier) SubscribeWrite(_ wal.WriteEventSubscriber) {}

// nilMarker implements Marker returning always a non-existing segment number.
type nilMarker struct{}

func (n nilMarker) LastMarkedSegment() int {
	return -1
}

type Stoppable interface {
	Stop()
}

// Manager manages remote write client instantiation, and connects the related components to orchestrate the flow of loki.Entry
// from the scrape targets, to the remote write clients themselves.
//
// Right now it just supports instantiating the WAL writer side of the future-to-be WAL enabled client. In follow-up
// work, tracked in https://github.com/grafana/loki/issues/8197, this Manager will be responsible for instantiating all client
// types: Logger, Multi and WAL.
type Manager struct {
	name        string
	clients     []Client
	walWatchers []Stoppable

	entries chan loki.Entry
	once    sync.Once

	wg sync.WaitGroup
}

// NewManager creates a new Manager
func NewManager(metrics *Metrics, logger log.Logger, limits limit.Config, reg prometheus.Registerer, walCfg wal.Config, notifier WriterEventsNotifier, clientCfgs ...Config) (*Manager, error) {
	var fake struct{}

	watcherMetrics := wal.NewWatcherMetrics(reg)

	if len(clientCfgs) == 0 {
		return nil, fmt.Errorf("at least one client config must be provided")
	}

	clientsCheck := make(map[string]struct{})
	clients := make([]Client, 0, len(clientCfgs))
	watchers := make([]Stoppable, 0, len(clientCfgs))
	for _, cfg := range clientCfgs {
		if walCfg.Enabled {
			// TODO: Should I care here about duplicate clients and else, or should we be thinking in a WAL only future?
			configName := asSha256(cfg)

			// add some context information for the logger the watcher uses
			wlog := log.With(logger, "client", configName)

			queue, err := NewQueue(metrics, cfg, limits.MaxStreams, limits.MaxLineSize.Val(), limits.MaxLineSizeTruncate, logger)
			if err != nil {
				return nil, fmt.Errorf("error starting queue client: %w", err)
			}
			// subscribe watcher's wal.WriteTo to writer events. This will make the writer trigger the cleanup of the wal.WriteTo
			// series cache whenever a segment is deleted.
			notifier.SubscribeCleanup(queue)

			watcher := wal.NewWatcher(walCfg.Dir, configName, watcherMetrics, queue, wlog, walCfg.WatchConfig, NilMarker)
			// subscribe watcher to wal write events
			notifier.SubscribeWrite(watcher)

			level.Debug(logger).Log("msg", "starting WAL watcher for client", "client", configName)
			watcher.Start()

			watchers = append(watchers, watcher)
		} else {
			client, err := New(metrics, cfg, limits.MaxStreams, limits.MaxLineSize.Val(), limits.MaxLineSizeTruncate, logger)
			if err != nil {
				return nil, fmt.Errorf("error starting client: %w", err)
			}

			// Don't allow duplicate clients, we have client specific metrics that need at least one unique label value (name).
			if _, ok := clientsCheck[client.Name()]; ok {
				return nil, fmt.Errorf("duplicate client configs are not allowed, found duplicate for name: %s", cfg.Name)
			}

			clientsCheck[client.Name()] = fake
			clients = append(clients, client)
		}
	}
	manager := &Manager{
		clients:     clients,
		walWatchers: watchers,
		entries:     make(chan loki.Entry),
	}
	if walCfg.Enabled {
		manager.name = "wal"
		manager.startWithConsume()
	} else {
		manager.name = "multi"
		manager.startWithForward()
	}
	return manager, nil
}

// startWithConsume starts the main manager routine, which reads and discards entries from the exposed channel.
// This is necessary since to treat the WAL-enabled manager the same way as the WAL-disabled one, the processing pipeline
// send entries both to the WAL writer, and the channel exposed by the manager. In the case the WAL is enabled, these entries
// are not used since they are read from the WAL, so we need a routine to just read the entries received through the channel
// and discarding them, to not block the sending side.
func (m *Manager) startWithConsume() {
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		// discard read entries
		//nolint:revive
		for range m.entries {
		}
	}()
}

// startWithForward starts the main manager routine, which reads entries from the exposed channel, and forwards them
// doing a fan-out across all inner clients.
func (m *Manager) startWithForward() {
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		for e := range m.entries {
			for _, c := range m.clients {
				c.Chan() <- e
			}
		}
	}()
}

func (m *Manager) StopNow() {
	for _, c := range m.clients {
		c.StopNow()
	}
}

func (m *Manager) Name() string {
	var sb strings.Builder
	sb.WriteString(m.name)
	sb.WriteString(":")
	for i, c := range m.clients {
		sb.WriteString(c.Name())
		if i != len(m.clients)-1 {
			sb.WriteString(",")
		}
	}
	return sb.String()
}

func (m *Manager) Chan() chan<- loki.Entry {
	return m.entries
}

func (m *Manager) Stop() {
	// first stop the receiving channel
	m.once.Do(func() { close(m.entries) })
	m.wg.Wait()
	// close wal watchers
	for _, walWatcher := range m.walWatchers {
		walWatcher.Stop()
	}
	// close clients
	for _, c := range m.clients {
		c.Stop()
	}
}
