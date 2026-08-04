// Package store implements the management-plane persistence on LevelDB
// (goleveldb). Data is organised as JSON values under key prefixes
// ("buckets"); keys sort byte-wise so time-ordered buckets page naturally.
// Writes are serialised by a single in-process mutex; multi-key updates go
// through WriteBatch so they commit atomically. Schema versioning is handled
// by a meta/version counter plus ordered migration functions — no external
// migration tooling, matching the single-instance embedded deployment.
package store

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/util"
)

// Key prefixes (buckets). Each maps to a Go type in this package.
const (
	BucketMeta    = "meta"
	BucketCluster = "cluster"
	BucketSeq     = "seq"
)

// schemaVersion 当前数据版本；每次不兼容变更 +1 并追加 migrate 函数。
const schemaVersion = 1

// Store 是 LevelDB 数据存储的门面。
type Store struct {
	db *leveldb.DB
	// mu 串行化进程内的读-改-写序列（单实例，无需分布式锁）
	mu sync.Mutex
}

// Open 打开（或创建）数据目录并执行到最新版本的迁移。
func Open(path string) (*Store, error) {
	db, err := leveldb.OpenFile(path, nil)
	if err != nil {
		return nil, fmt.Errorf("open leveldb %q: %w", path, err)
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate %q: %w", path, err)
	}
	return s, nil
}

// Close 关闭数据库。
func (s *Store) Close() error { return s.db.Close() }

// --- 版本迁移 ---

// migrate 将数据版本逐步提升到 schemaVersion。
func (s *Store) migrate() error {
	cur, err := s.getVersion()
	if err != nil {
		return err
	}
	if cur >= schemaVersion {
		return nil
	}
	for v := cur + 1; v <= schemaVersion; v++ {
		mig, ok := migrations[v]
		if !ok {
			return fmt.Errorf("no migration for version %d", v)
		}
		if err := mig(s); err != nil {
			return fmt.Errorf("migration %d: %w", v, err)
		}
	}
	return nil
}

// migrations 各版本迁移函数（v1 为空基座）。
var migrations = map[int]func(*Store) error{
	1: func(s *Store) error {
		// v1：初始 schema，无历史数据需要转换
		return s.putVersion(1)
	},
}

func (s *Store) getVersion() (int, error) {
	raw, err := s.db.Get([]byte(BucketMeta+"/version"), nil)
	if err == leveldb.ErrNotFound {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return int(binary.BigEndian.Uint64(raw)), nil
}

func (s *Store) putVersion(v int) error {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(v))
	return s.db.Put([]byte(BucketMeta+"/version"), buf[:], nil)
}

// --- 通用 KV 助手 ---

// get 读取一个 key，返回 ErrNotFound 语义（leveldb.ErrNotFound）。
func (s *Store) get(key string) ([]byte, error) {
	return s.db.Get([]byte(key), nil)
}

// put 写入一个 key。
func (s *Store) put(key string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal %q: %w", key, err)
	}
	return s.db.Put([]byte(key), data, nil)
}

// NextSeq 原子递增并返回 seq/<kind> 序列号。
func (s *Store) NextSeq(kind string) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := BucketSeq + "/" + kind
	raw, err := s.db.Get([]byte(key), nil)
	var cur uint64
	if err == nil {
		cur = binary.BigEndian.Uint64(raw)
	} else if err != leveldb.ErrNotFound {
		return 0, err
	}
	cur++
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], cur)
	if err := s.db.Put([]byte(key), buf[:], nil); err != nil {
		return 0, err
	}
	return cur, nil
}

// iterate 遍历 prefix 下的所有 key（不含 dir 风格子前缀过滤，调用方自理）。
func (s *Store) iterate(prefix string, fn func(key string, value []byte) error) error {
	iter := s.db.NewIterator(util.BytesPrefix([]byte(prefix)), nil)
	defer iter.Release()
	for iter.Next() {
		if err := fn(string(iter.Key()), iter.Value()); err != nil {
			return err
		}
	}
	return iter.Error()
}
