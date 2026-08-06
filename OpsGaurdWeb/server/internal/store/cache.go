// Cluster data cache: cache/<cluster>/<resource> -> JSON payload + timestamp.
//
// The management plane caches per-cluster read endpoints (nodes, workloads,
// service detail) so page loads render instantly from the last-known state
// while a background refresh updates the cache. Cache entries expire after
// CacheTTL and are refreshed on demand by the workerproxy client.
package store

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/syndtr/goleveldb/leveldb"
)

const (
	// CacheBucket is the LevelDB key prefix for cluster data cache.
	CacheBucket = "cache"
	// CacheTTL is how long a cached entry is considered fresh.
	CacheTTL = 30 * time.Second
)

// ClusterCache is a cached read response for one cluster resource.
type ClusterCache struct {
	Cluster   string    `json:"cluster"`
	Resource  string    `json:"resource"` // e.g. "nodes", "workloads", "workload/<name>"
	Payload   []byte    `json:"payload"`  // raw JSON of the response
	FetchedAt time.Time `json:"fetched_at"`
}

func clusterCacheKey(cluster, resource string) string {
	return fmt.Sprintf("%s/%s/%s", CacheBucket, cluster, resource)
}

// GetClusterCache returns the cached payload if present and fresh, or nil.
func (s *Store) GetClusterCache(cluster, resource string) *ClusterCache {
	data, err := s.db.Get([]byte(clusterCacheKey(cluster, resource)), nil)
	if err == leveldb.ErrNotFound {
		return nil
	}
	if err != nil {
		return nil
	}
	var c ClusterCache
	if err := json.Unmarshal(data, &c); err != nil {
		return nil
	}
	if time.Since(c.FetchedAt) > CacheTTL {
		return nil // stale — treat as miss
	}
	return &c
}

// PutClusterCache writes or overwrites a cache entry.
func (s *Store) PutClusterCache(cluster, resource string, payload []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := ClusterCache{
		Cluster:   cluster,
		Resource:  resource,
		Payload:   payload,
		FetchedAt: time.Now().UTC(),
	}
	data, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return s.db.Put([]byte(clusterCacheKey(cluster, resource)), data, nil)
}

// DeleteClusterCache removes a cluster's cached entries (used on cluster removal).
func (s *Store) DeleteClusterCache(cluster string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	prefix := []byte(CacheBucket + "/" + cluster + "/")
	iter := s.db.NewIterator(nil, nil)
	defer iter.Release()
	var keys [][]byte
	for iter.Next() {
		k := iter.Key()
		if len(k) > len(prefix) && string(k[:len(prefix)]) == string(prefix) {
			keys = append(keys, append([]byte(nil), k...))
		}
	}
	for _, k := range keys {
		if err := s.db.Delete(k, nil); err != nil {
			return err
		}
	}
	return nil
}
