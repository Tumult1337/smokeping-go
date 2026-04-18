package config

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
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
					log.Info("config reloaded")
				}
			}
		}
	}()
}
