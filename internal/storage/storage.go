package storage

import (
	"fmt"
	"os"
	"time"

	badger "github.com/dgraph-io/badger/v4"
)

type Storage struct {
	db     *badger.DB
	gcStop chan struct{}
	gcDone chan struct{}
}

func Open(path string) (*Storage, error) {
	if err := os.MkdirAll(path, 0o755); err != nil {
		return nil, fmt.Errorf("create storage dir: %w", err)
	}
	opts := badger.DefaultOptions(path).WithLoggingLevel(badger.ERROR)
	db, err := badger.Open(opts)
	if err != nil {
		return nil, fmt.Errorf("open badger: %w", err)
	}
	s := &Storage{
		db:     db,
		gcStop: make(chan struct{}),
		gcDone: make(chan struct{}),
	}
	go s.gcLoop()
	return s, nil
}

func (s *Storage) Close() error {
	close(s.gcStop)
	<-s.gcDone
	return s.db.Close()
}

func (s *Storage) gcLoop() {
	defer close(s.gcDone)
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-s.gcStop:
			return
		case <-ticker.C:
			for {
				if err := s.db.RunValueLogGC(0.5); err != nil {
					break
				}
			}
		}
	}
}
