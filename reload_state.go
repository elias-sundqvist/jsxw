package main

import (
	"sync"

	webview_selector "github.com/jchv/go-webview-selector"
)

type reloadStateStore struct {
	mu   sync.RWMutex
	data string
}

func newReloadStateStore() *reloadStateStore {
	return &reloadStateStore{data: "{}"}
}

func (s *reloadStateStore) get() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data
}

func (s *reloadStateStore) set(data string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if data == "" {
		s.data = "{}"
		return
	}
	s.data = data
}

func initReloadStateBridge(w webview_selector.WebView, store *reloadStateStore) error {
	if err := w.Bind("__codexGetReloadState", func() string {
		return store.get()
	}); err != nil {
		return err
	}

	if err := w.Bind("__codexSetReloadState", func(data string) error {
		store.set(data)
		return nil
	}); err != nil {
		return err
	}

	return nil
}
