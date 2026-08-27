package config

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Store holds the live Config and notifies a single subscriber on reload.
// The "one subscriber" shape matches actual use — only the scheduler
// Supervisor listens — and keeps the Reload → Subscriber handoff ordered
// on a single channel instead of fanning across a slice.
//
// The channel carries struct{} (signal-only) so the subscriber always calls
// Current() for the latest config. Sending the config object on the channel
// was unsafe: a rapid double-SIGHUP could drop the second send (channel full),
// leaving the scheduler on a stale config while the store held the new one.
type Store struct {
	path string
	cur  atomic.Pointer[Config]
	mu   sync.Mutex
	sub  chan<- struct{}
	// validate is an extra admission check a reload must pass, for invariants
	// this package cannot express: alert conditions are parsed in internal/alert,
	// which imports this one, so Validate cannot reach them.
	validate func(*Config) error
}

func NewStore(path string, initial *Config) *Store {
	s := &Store{path: path}
	s.cur.Store(initial)
	return s
}

func (s *Store) Current() *Config {
	return s.cur.Load()
}

// Subscribe registers the single reload listener. Panics if called twice;
// the store is intentionally 1:1 with the lifecycle helper.
func (s *Store) Subscribe(ch chan<- struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sub != nil {
		panic("config: Store.Subscribe called more than once")
	}
	s.sub = ch
}

// SetValidator installs a check every later Reload must pass before the new
// config becomes current. Without it a config that loads but that a consumer
// cannot apply is published anyway, and the consumer's own refresh failure is
// only a log line: the evaluator kept its previous condition map while
// Store.Current() served the new alerts, so every alert added in that edit
// silently never fired for the life of the process.
func (s *Store) SetValidator(fn func(*Config) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.validate = fn
}

func (s *Store) Reload() error {
	cfg, err := Load(s.path)
	if err != nil {
		return err
	}
	s.mu.Lock()
	validate := s.validate
	s.mu.Unlock()
	if validate != nil {
		if err := validate(cfg); err != nil {
			return err
		}
	}
	s.cur.Store(cfg)
	s.mu.Lock()
	sub := s.sub
	s.mu.Unlock()
	if sub != nil {
		select {
		case sub <- struct{}{}:
		default:
		}
	}
	return nil
}

func (s *Store) WatchSIGHUP(ctx context.Context, log *slog.Logger) {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGHUP)
	go func() {
		defer signal.Stop(sig)
		for {
			select {
			case <-ctx.Done():
				return
			case <-sig:
				if err := s.Reload(); err != nil {
					log.Error("config reload failed", "err", err)
				} else {
					log.Info("config reloaded", "trigger", "sighup")
				}
			}
		}
	}()
}

// WatchFile watches the config path for in-place edits and atomic writes
// (editors that rename a tempfile over the target, k8s configmap symlink
// swaps) and calls Reload after a short quiet period. Reload failures are
// logged and the previous config is kept — a malformed edit doesn't take
// the process down.
//
// The watcher is attached to the parent directory so rename-over-target
// doesn't leave a dangling inode; every dir event is filtered to the
// config file's basename.
func (s *Store) WatchFile(ctx context.Context, log *slog.Logger) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	base := filepath.Base(s.path)
	if err := w.Add(dir); err != nil {
		_ = w.Close()
		return err
	}

	go func() {
		defer func() {
			if err := w.Close(); err != nil {
				log.Warn("config watcher close", "err", err)
			}
		}()
		// Debounce bursty editor writes (vim: WRITE → RENAME → CHMOD).
		// A 200ms quiet period is short enough to feel instant and long
		// enough to collapse any real editor save into one reload.
		const debounce = 200 * time.Millisecond
		var timer *time.Timer
		fire := func() {
			if ctx.Err() != nil {
				return
			}
			if err := s.Reload(); err != nil {
				log.Warn("config file reload failed, keeping previous config", "err", err)
				return
			}
			log.Info("config reloaded", "trigger", "file")
		}
		for {
			select {
			case <-ctx.Done():
				if timer != nil {
					timer.Stop()
				}
				return
			case ev, ok := <-w.Events:
				if !ok {
					return
				}
				if filepath.Base(ev.Name) != base {
					continue
				}
				// Rename/Remove events detach the inode we were
				// watching; re-add the dir is a no-op (already added)
				// but ensures we keep seeing events on k8s configmap
				// symlink swaps.
				if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename|fsnotify.Remove) == 0 {
					continue
				}
				if timer == nil {
					timer = time.AfterFunc(debounce, fire)
				} else {
					timer.Reset(debounce)
				}
			case err, ok := <-w.Errors:
				if !ok {
					return
				}
				if errors.Is(err, context.Canceled) {
					return
				}
				log.Warn("config file watcher error", "err", err)
			}
		}
	}()
	return nil
}
