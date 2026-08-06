// Subscription cursors: cursor/<cluster> -> Cursor{cluster, last_seq}.
//
// Each cursor records the highest Worker event/audit sequence the server has
// acknowledged for a cluster, so that after a reconnect or restart the gRPC
// Subscribe* stream resumes from where it left off (no duplicates, no gaps —
// the Worker persists events until acked).
package store

import (
	"encoding/binary"
	"encoding/json"
	"fmt"

	"github.com/syndtr/goleveldb/leveldb"
)

func cursorKey(cluster string) string { return BucketCursor + "/" + cluster }

// Cursor is the persisted subscription position for one cluster.
type Cursor struct {
	Cluster string `json:"cluster"`
	LastSeq int64  `json:"last_seq"`
}

// GetCursor returns the saved subscription cursor for a cluster, or (0, nil)
// when none exists yet (a fresh cluster starts from the beginning).
func (s *Store) GetCursor(cluster string) (int64, error) {
	data, err := s.db.Get([]byte(cursorKey(cluster)), nil)
	if err == leveldb.ErrNotFound {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("get cursor %s: %w", cluster, err)
	}
	if len(data) >= 8 {
		// binary fast path (PutCursorBinary writes 8 bytes)
		return int64(binary.BigEndian.Uint64(data)), nil
	}
	// fall back to JSON if a legacy/json cursor exists
	var c Cursor
	if err := json.Unmarshal(data, &c); err != nil {
		return 0, nil // corrupt → restart from 0
	}
	return c.LastSeq, nil
}

// PutCursor persists the subscription cursor for a cluster (8-byte big-endian,
// matching the store's sequence convention).
func (s *Store) PutCursor(cluster string, lastSeq int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(lastSeq))
	if err := s.db.Put([]byte(cursorKey(cluster)), buf[:], nil); err != nil {
		return fmt.Errorf("put cursor %s: %w", cluster, err)
	}
	return nil
}

// DeleteCursor removes a cluster's cursor (used when a cluster is removed).
func (s *Store) DeleteCursor(cluster string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Delete([]byte(cursorKey(cluster)), nil)
}
