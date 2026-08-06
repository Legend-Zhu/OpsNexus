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
	BucketMeta          = "meta"
	BucketProject       = "project"
	BucketCluster       = "cluster"
	BucketEvent         = "event"
	BucketAlert         = "alert"
	BucketPatrol        = "patrol"
	BucketPatrolRun     = "patrolrun"
	BucketReport        = "report"
	BucketAINexus       = "ainexus" // AiNexus 网关运行时配置（页面保存的热重载配置）
	BucketSeq           = "seq"
	BucketSettings      = "settings"      // 全局设置（如巡检报告投递策略）
	BucketInvestigation = "investigation" // 排查会话落库（对话式 troubleshoot）
	BucketSecret        = "secret"        // 密钥引用（巡检 flow 拨测账号等）
	BucketCursor        = "cursor"        // 事件/审计订阅游标（cluster -> last_seq）

	// IdP（OpsGaurd 作为 OIDC 身份提供者）相关 bucket。v2 新增，无历史数据迁移。
	BucketClient        = "idpclient"  // OIDC client（RP）注册表
	BucketAuthCode      = "idpcode"    // 授权码（一次性，短 TTL）
	BucketAccessToken   = "idpatoken"  // access token jti（用于 introspect / 吊销）
	BucketRefreshToken  = "idprtoken"  // refresh token（不透明随机串）
	BucketSigningKey    = "idpkey"     // IdP JWT 签名 RSA 私钥
	BucketIDPSession    = "idpsession" // IdP SSO 会话（cookie sid -> 记录）
)

// schemaVersion 当前数据版本；每次不兼容变更 +1 并追加 migrate 函数。
const schemaVersion = 2

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
	2: func(s *Store) error {
		// v2：新增 IdP bucket（client/authcode/atoken/rtoken/key/session）。
		// 纯结构新增，无需迁移旧数据；仅推进版本号。
		return s.putVersion(2)
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

// WriteBatch 原子地批量写入与删除（leveldb.Batch 单次 db.Write）。
// puts 为 (key,value) 对（value 经 JSON 序列化）；dels 为待删 key。
// 用于"签发 token + 落 session"等多 key 必须原子提交的场景。
func (s *Store) WriteBatch(puts []KV, dels []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	batch := new(leveldb.Batch)
	for _, p := range puts {
		data, err := json.Marshal(p.Value)
		if err != nil {
			return fmt.Errorf("marshal %q: %w", p.Key, err)
		}
		batch.Put([]byte(p.Key), data)
	}
	for _, k := range dels {
		batch.Delete([]byte(k))
	}
	return s.db.Write(batch, nil)
}

// KV 是 WriteBatch 的单个键值项。
type KV struct {
	Key   string
	Value any
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
