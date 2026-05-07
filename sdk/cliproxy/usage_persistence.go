package cliproxy

import (
	"path/filepath"
	"strings"
	"sync"
	"time"

	internalusage "github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	log "github.com/sirupsen/logrus"
)

const (
	usagePersistenceFilename = ".usage-statistics.snapshot"
	usagePersistenceInterval = 30 * time.Second
)

type usagePersistenceRunner struct {
	stats    *internalusage.RequestStatistics
	path     string
	interval time.Duration

	mu      sync.Mutex
	started bool
	stopped bool
	stopCh  chan struct{}
	doneCh  chan struct{}
}

func newUsagePersistenceRunner(stats *internalusage.RequestStatistics, path string, interval time.Duration) *usagePersistenceRunner {
	return &usagePersistenceRunner{
		stats:    stats,
		path:     strings.TrimSpace(path),
		interval: interval,
		stopCh:   make(chan struct{}),
		doneCh:   make(chan struct{}),
	}
}

func (r *usagePersistenceRunner) start() {
	if r == nil || r.stats == nil || r.path == "" {
		return
	}
	r.mu.Lock()
	if r.started {
		r.mu.Unlock()
		return
	}
	r.started = true
	interval := r.interval
	stopCh := r.stopCh
	doneCh := r.doneCh
	path := r.path
	stats := r.stats
	r.mu.Unlock()

	if interval <= 0 {
		interval = usagePersistenceInterval
	}

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		defer close(doneCh)
		for {
			select {
			case <-ticker.C:
				if err := internalusage.SavePersistedState(path, stats); err != nil {
					log.Warnf("usage persistence save failed: %v", err)
				}
			case <-stopCh:
				return
			}
		}
	}()
}

func (r *usagePersistenceRunner) stop() error {
	if r == nil || r.stats == nil || r.path == "" {
		return nil
	}

	r.mu.Lock()
	started := r.started
	if !r.stopped {
		r.stopped = true
		if started {
			close(r.stopCh)
		}
	}
	doneCh := r.doneCh
	r.mu.Unlock()

	if started {
		<-doneCh
	}
	return internalusage.SavePersistedState(r.path, r.stats)
}

func (s *Service) loadUsagePersistence() error {
	runner := s.ensureUsagePersistenceRunner()
	if runner == nil {
		return nil
	}
	loaded, err := internalusage.LoadPersistedState(runner.path, runner.stats)
	if err != nil {
		return err
	}
	if loaded {
		log.Infof("restored usage statistics from %s", runner.path)
	}
	return nil
}

func (s *Service) startUsagePersistence() {
	if runner := s.ensureUsagePersistenceRunner(); runner != nil {
		runner.start()
	}
}

func (s *Service) stopUsagePersistence() error {
	if s == nil || s.usagePersistence == nil {
		return nil
	}
	return s.usagePersistence.stop()
}

func (s *Service) ensureUsagePersistenceRunner() *usagePersistenceRunner {
	if s == nil {
		return nil
	}
	if s.usagePersistence != nil {
		return s.usagePersistence
	}
	path := s.resolveUsagePersistencePath()
	if path == "" {
		return nil
	}
	s.usagePersistence = newUsagePersistenceRunner(internalusage.GetRequestStatistics(), path, usagePersistenceInterval)
	return s.usagePersistence
}

func (s *Service) resolveUsagePersistencePath() string {
	if s == nil || s.cfg == nil {
		return ""
	}

	authDir, err := util.ResolveAuthDir(strings.TrimSpace(s.cfg.AuthDir))
	if err == nil && authDir != "" {
		return filepath.Join(authDir, usagePersistenceFilename)
	}

	configPath := strings.TrimSpace(s.configPath)
	if configPath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(configPath), usagePersistenceFilename)
}
