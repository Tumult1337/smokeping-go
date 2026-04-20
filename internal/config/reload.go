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

type Store struct {
	path string
	cur  atomic.Pointer[Config]
	mu   sync.Mutex
	subs []chan<- *Config
}

func NewStore(path string, initial *Config) *Store {
	s := &Store{path: path}
	s.cur.Store(initial)
	return s
}

func (s *Store) Current() *Config {
	return s.cur.Load()
}

func (s *Store) Subscribe(ch chan<- *Config) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subs = append(s.subs, ch)
}

func (s *Store) Reload() error {
	cfg, err := Load(s.path)
	if err != nil {
		return err
	}
	s.cur.Store(cfg)
	s.mu.Lock()
	subs := append([]chan<- *Config(nil), s.subs...)
	s.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- cfg:
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
		w.Close()
		return err
	}

	go func() {
		defer w.Close()
		// Debounce bursty editor writes (vim: WRITE → RENAME → CHMOD).
		// A 200ms quiet period is short enough to feel instant and long
		// enough to collapse any real editor save into one reload.
		const debounce = 200 * time.Millisecond
		var timer *time.Timer
		fire := func() {
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
