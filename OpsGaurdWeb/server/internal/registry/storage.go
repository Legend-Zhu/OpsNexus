// Package registry 内嵌 OCI 镜像仓库（分布协议 /v2）+ 页面传包构建。
//
// 存储布局（data/registry/ 下，distribution 简化版）：
//
//	blobs/sha256/<digest>/data                     blob 内容（按内容寻址，原子写入）
//	repositories/<name>/_manifests/revisions/sha256/<digest>/{data,ctype}
//	repositories/<name>/_manifests/tags/<tag>/current/link   -> "sha256:<digest>"
//	_uploads/<uuid>/{data,startedat}               推送会话（blob 分片上传）
//
// 设计要点：content-type 随 manifest 原样存取（docker schema2 / OCI / image
// index 透明，多架构 manifest 可存可拉）；manifest 引用的 blob 必须已存在
// （与 distribution 一致）；retention 按 tag 时间裁剪 + mark-sweep 清无引用 blob。
package registry

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// 错误哨兵（handlers 映射为 OCI 错误码）。
var (
	ErrNameUnknown     = errors.New("name unknown")
	ErrBlobUnknown     = errors.New("blob unknown")
	ErrUploadUnknown   = errors.New("blob upload unknown")
	ErrDigestInvalid   = errors.New("digest invalid")
	ErrManifestInvalid = errors.New("manifest invalid")
)

// Store 镜像仓库存储（单进程，文件系统）。
type Store struct {
	root string
}

// NewStore 打开（或创建）存储目录。
func NewStore(root string) (*Store, error) {
	for _, d := range []string{"blobs/sha256", "repositories", "_uploads"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			return nil, fmt.Errorf("init registry storage: %w", err)
		}
	}
	return &Store{root: root}, nil
}

// ---- 路径约定 ----

func validDigest(digest string) bool {
	rest, ok := strings.CutPrefix(digest, "sha256:")
	return ok && len(rest) == 64 && isHex(rest)
}

func isHex(s string) bool {
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// validName 仓库名：小写字母数字 + ._-/，多级路径允许（如 ops/myapp）。
func validName(name string) bool {
	if name == "" || len(name) > 255 {
		return false
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
		for _, c := range seg {
			if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' || c == '.' || c == '-') {
				return false
			}
		}
	}
	return true
}

// validTag 标签：字母数字开头 + ._-。
func validTag(tag string) bool {
	if tag == "" || len(tag) > 128 {
		return false
	}
	for i, c := range tag {
		ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			(i > 0 && (c == '_' || c == '.' || c == '-'))
		if !ok {
			return false
		}
	}
	return true
}

func (s *Store) blobPath(digest string) string {
	return filepath.Join(s.root, "blobs", "sha256", strings.TrimPrefix(digest, "sha256:"), "data")
}

func (s *Store) repoDir(name string) string {
	return filepath.Join(s.root, "repositories", filepath.FromSlash(name))
}

func (s *Store) revisionDir(name, digest string) string {
	return filepath.Join(s.repoDir(name), "_manifests", "revisions", "sha256", strings.TrimPrefix(digest, "sha256:"))
}

func (s *Store) tagLinkPath(name, tag string) string {
	return filepath.Join(s.repoDir(name), "_manifests", "tags", tag, "current", "link")
}

// atomicWrite 临时文件 + rename 原子落盘。
func atomicWrite(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp-" + randHex(6)
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// ---- blob ----

// HasBlob 判断 blob 是否存在。
func (s *Store) HasBlob(digest string) bool {
	if !validDigest(digest) {
		return false
	}
	_, err := os.Stat(s.blobPath(digest))
	return err == nil
}

// BlobSize 返回 blob 大小（不存在返回 ErrBlobUnknown）。
func (s *Store) BlobSize(digest string) (int64, error) {
	if !validDigest(digest) {
		return 0, ErrDigestInvalid
	}
	fi, err := os.Stat(s.blobPath(digest))
	if err != nil {
		return 0, ErrBlobUnknown
	}
	return fi.Size(), nil
}

// OpenBlob 打开 blob 内容流（调用方负责 Close）。
func (s *Store) OpenBlob(digest string) (io.ReadSeekCloser, int64, error) {
	if !validDigest(digest) {
		return nil, 0, ErrDigestInvalid
	}
	f, err := os.Open(s.blobPath(digest))
	if err != nil {
		return nil, 0, ErrBlobUnknown
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	return f, fi.Size(), nil
}

// putBlob 落盘 blob（已存在则跳过；校验内容 digest）。
func (s *Store) putBlob(digest string, data []byte) error {
	if !validDigest(digest) {
		return ErrDigestInvalid
	}
	sum := sha256.Sum256(data)
	if "sha256:"+hex.EncodeToString(sum[:]) != digest {
		return fmt.Errorf("%w: content digest mismatch", ErrDigestInvalid)
	}
	if s.HasBlob(digest) {
		return nil
	}
	return atomicWrite(s.blobPath(digest), data)
}

// ---- 推送会话（blob 上传） ----

func (s *Store) uploadDir(uuid string) string {
	return filepath.Join(s.root, "_uploads", uuid)
}

// StartUpload 开启推送会话，返回 uuid。
func (s *Store) StartUpload() string {
	uuid := randHex(16)
	dir := s.uploadDir(uuid)
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, "startedat"), []byte(time.Now().UTC().Format(time.RFC3339)), 0o644)
	return uuid
}

// UploadSize 已上传字节数。
func (s *Store) UploadSize(uuid string) (int64, error) {
	fi, err := os.Stat(filepath.Join(s.uploadDir(uuid), "data"))
	if os.IsNotExist(err) {
		if _, derr := os.Stat(s.uploadDir(uuid)); derr != nil {
			return 0, ErrUploadUnknown
		}
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

// AppendUpload 追加上传分片。
func (s *Store) AppendUpload(uuid string, r io.Reader) (int64, error) {
	dir := s.uploadDir(uuid)
	if _, err := os.Stat(dir); err != nil {
		return 0, ErrUploadUnknown
	}
	f, err := os.OpenFile(filepath.Join(dir, "data"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	if _, err := io.Copy(f, r); err != nil {
		return 0, err
	}
	return s.UploadSize(uuid)
}

// FinishUpload 完成上传（可带最后一片 body），校验 digest 后转为 blob。
func (s *Store) FinishUpload(uuid, digest string, r io.Reader) error {
	if !validDigest(digest) {
		return ErrDigestInvalid
	}
	dir := s.uploadDir(uuid)
	if _, err := os.Stat(dir); err != nil {
		return ErrUploadUnknown
	}
	defer os.RemoveAll(dir)
	if r != nil {
		if _, err := s.AppendUpload(uuid, r); err != nil {
			return err
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, "data"))
	if err != nil {
		return fmt.Errorf("%w: empty upload", ErrDigestInvalid)
	}
	return s.putBlob(digest, data)
}

// CancelUpload 取消推送会话。
func (s *Store) CancelUpload(uuid string) {
	os.RemoveAll(s.uploadDir(uuid))
}

// ---- manifest ----

// PutManifest 写入 manifest（tag 或 digest 引用）；校验引用的 blob/子 manifest 已存在。
// contentType 原样保存（GET 时回放）。返回规范 digest。
func (s *Store) PutManifest(name, ref, contentType string, data []byte) (string, error) {
	if !validName(name) {
		return "", fmt.Errorf("%w: invalid name %q", ErrManifestInvalid, name)
	}
	if !validTag(ref) && !validDigest(ref) {
		return "", fmt.Errorf("%w: invalid reference %q", ErrManifestInvalid, ref)
	}
	mf, err := parseManifest(data)
	if err != nil {
		return "", err
	}
	// 引用完整性（与 distribution 一致：blob/子 manifest 必须先推）
	for _, d := range mf.blobDigests() {
		if !s.HasBlob(d) {
			return "", fmt.Errorf("%w: %s", ErrBlobUnknown, d)
		}
	}
	for _, d := range mf.childManifests {
		if !s.HasManifest(name, d) {
			return "", fmt.Errorf("%w: child manifest %s", ErrManifestInvalid, d)
		}
	}

	sum := sha256.Sum256(data)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	dir := s.revisionDir(name, digest)
	if err := atomicWrite(filepath.Join(dir, "data"), data); err != nil {
		return "", err
	}
	if err := atomicWrite(filepath.Join(dir, "ctype"), []byte(contentType)); err != nil {
		return "", err
	}
	if validTag(ref) {
		if err := atomicWrite(s.tagLinkPath(name, ref), []byte(digest)); err != nil {
			return "", err
		}
	}
	return digest, nil
}

// HasManifest 判断 manifest 是否存在（ref 为 tag 或 digest）。
func (s *Store) HasManifest(name, ref string) bool {
	_, _, err := s.GetManifest(name, ref)
	return err == nil
}

// GetManifest 读取 manifest（digest 直接读 revision；tag 先解析 link）。
func (s *Store) GetManifest(name, ref string) (contentType string, data []byte, err error) {
	if !validName(name) {
		return "", nil, ErrNameUnknown
	}
	digest := ref
	if validTag(ref) {
		link, lerr := os.ReadFile(s.tagLinkPath(name, ref))
		if lerr != nil {
			if _, rerr := os.Stat(s.repoDir(name)); rerr != nil {
				return "", nil, ErrNameUnknown
			}
			return "", nil, ErrManifestInvalid // tag 不存在
		}
		digest = strings.TrimSpace(string(link))
	}
	if !validDigest(digest) {
		return "", nil, ErrDigestInvalid
	}
	dir := s.revisionDir(name, digest)
	data, err = os.ReadFile(filepath.Join(dir, "data"))
	if err != nil {
		if _, rerr := os.Stat(s.repoDir(name)); rerr != nil {
			return "", nil, ErrNameUnknown
		}
		return "", nil, ErrManifestInvalid
	}
	ct, _ := os.ReadFile(filepath.Join(dir, "ctype"))
	contentType = strings.TrimSpace(string(ct))
	return contentType, data, nil
}

// DeleteManifest 按 digest 删除 manifest（revision + 指向它的 tag link）。
func (s *Store) DeleteManifest(name, digest string) error {
	if !validDigest(digest) {
		return ErrDigestInvalid
	}
	if _, err := os.Stat(s.revisionDir(name, digest)); err != nil {
		return ErrManifestInvalid
	}
	os.RemoveAll(s.revisionDir(name, digest))
	for _, t := range s.listTags(name) {
		link, err := os.ReadFile(s.tagLinkPath(name, t.Tag))
		if err == nil && strings.TrimSpace(string(link)) == digest {
			os.RemoveAll(filepath.Join(s.repoDir(name), "_manifests", "tags", t.Tag))
		}
	}
	return nil
}

// DeleteTag 删除指定 tag（manifest 本体保留，待 GC 清理）。
func (s *Store) DeleteTag(name, tag string) bool {
	path := filepath.Join(s.repoDir(name), "_manifests", "tags", tag)
	if _, err := os.Stat(path); err != nil {
		return false
	}
	os.RemoveAll(path)
	return true
}

// DeleteRepo 删除整个仓库（全部 tag 与 manifest revision；blob 由 GC 清理）。
func (s *Store) DeleteRepo(name string) bool {
	if !validName(name) {
		return false
	}
	dir := s.repoDir(name)
	if _, err := os.Stat(dir); err != nil {
		return false
	}
	os.RemoveAll(dir)
	return true
}

// ---- catalog / tags ----

// TagInfo 一个 tag 的视图。
type TagInfo struct {
	Tag     string    `json:"tag"`
	Digest  string    `json:"digest"`
	Size    int64     `json:"size"`    // manifest 引用的 blob 总字节
	Updated time.Time `json:"updated"` // tag link 写入时间
}

func (s *Store) listTags(name string) []TagInfo {
	base := filepath.Join(s.repoDir(name), "_manifests", "tags")
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil
	}
	var out []TagInfo
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		link, err := os.ReadFile(s.tagLinkPath(name, e.Name()))
		if err != nil {
			continue
		}
		digest := strings.TrimSpace(string(link))
		var updated time.Time
		if fi, err := os.Stat(s.tagLinkPath(name, e.Name())); err == nil {
			updated = fi.ModTime()
		}
		out = append(out, TagInfo{Tag: e.Name(), Digest: digest, Size: s.manifestSize(name, digest), Updated: updated})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Updated.After(out[j].Updated) })
	return out
}

// Tags 列出仓库的全部 tag（最新在前）。
func (s *Store) Tags(name string) ([]TagInfo, error) {
	if _, err := os.Stat(s.repoDir(name)); err != nil {
		return nil, ErrNameUnknown
	}
	return s.listTags(name), nil
}

// manifestSize 计算 manifest 引用的 blob 总字节（index 递归一层）。
func (s *Store) manifestSize(name, digest string) int64 {
	_, data, err := s.GetManifest(name, digest)
	if err != nil {
		return 0
	}
	mf, err := parseManifest(data)
	if err != nil {
		return 0
	}
	var total int64
	for _, d := range mf.blobDigests() {
		if sz, err := s.BlobSize(d); err == nil {
			total += sz
		}
	}
	for _, child := range mf.childManifests {
		total += s.manifestSize(name, child)
	}
	return total
}

// Catalog 列出全部仓库名。
func (s *Store) Catalog() []string {
	out := []string{}
	root := filepath.Join(s.root, "repositories")
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || !info.IsDir() {
			return nil
		}
		if info.Name() == "_manifests" {
			rel, _ := filepath.Rel(root, filepath.Dir(path))
			out = append(out, filepath.ToSlash(rel))
			return filepath.SkipDir
		}
		return nil
	})
	sort.Strings(out)
	return out
}

// ---- retention + GC ----

// ApplyRetention 每仓库保留最近 keepN 个 tag（0/负数 = 不裁剪），随后 GC 无引用 blob。
// 返回（裁剪的 tag 数，删除的 blob 数）。
func (s *Store) ApplyRetention(keepN int) (int, int) {
	if keepN <= 0 {
		return 0, s.SweepBlobs()
	}
	trimmed := 0
	for _, repo := range s.Catalog() {
		tags := s.listTags(repo) // 最新在前
		for i, t := range tags {
			if i < keepN {
				continue
			}
			if s.DeleteTag(repo, t.Tag) {
				trimmed++
			}
		}
	}
	return trimmed, s.SweepBlobs()
}

// SweepBlobs 删除无任何 manifest 引用的 blob 与无 tag 引用的 revision。
func (s *Store) SweepBlobs() int {
	usedBlobs := map[string]bool{}
	usedManifests := map[string]bool{}
	for _, repo := range s.Catalog() {
		for _, t := range s.listTags(repo) {
			s.markManifest(repo, t.Digest, usedManifests, usedBlobs)
		}
	}
	removed := 0
	// 无 tag 引用的 revision
	for _, repo := range s.Catalog() {
		revBase := filepath.Join(s.repoDir(repo), "_manifests", "revisions", "sha256")
		entries, err := os.ReadDir(revBase)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			digest := "sha256:" + e.Name()
			if !usedManifests[digest] {
				os.RemoveAll(filepath.Join(revBase, e.Name()))
			}
		}
	}
	// 无引用的 blob
	blobBase := filepath.Join(s.root, "blobs", "sha256")
	entries, err := os.ReadDir(blobBase)
	if err != nil {
		return 0
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if !usedBlobs["sha256:"+e.Name()] {
			os.RemoveAll(filepath.Join(blobBase, e.Name()))
			removed++
		}
	}
	return removed
}

// markManifest 递归标记 manifest 及其引用（index → 子 manifest → blob）。
func (s *Store) markManifest(name, digest string, usedManifests, usedBlobs map[string]bool) {
	if usedManifests[digest] {
		return
	}
	_, data, err := s.GetManifest(name, digest)
	if err != nil {
		return
	}
	usedManifests[digest] = true
	mf, err := parseManifest(data)
	if err != nil {
		return
	}
	for _, d := range mf.blobDigests() {
		usedBlobs[d] = true
	}
	for _, child := range mf.childManifests {
		s.markManifest(name, child, usedManifests, usedBlobs)
	}
}

// ---- manifest 解析（最小 JSON 视图） ----

type manifestView struct {
	MediaType string `json:"mediaType"`
	Config    struct {
		Digest string `json:"digest"`
	} `json:"config"`
	Layers []struct {
		Digest string `json:"digest"`
	} `json:"layers"`
	// image index / manifest list
	Manifests []struct {
		Digest string `json:"digest"`
	} `json:"manifests"`
	// schema1 兼容（只校验可解析，不取引用）
	SchemaVersion int `json:"schemaVersion"`
}

type parsedManifest struct {
	mediaType      string
	blobs          []string
	childManifests []string
}

func (p *parsedManifest) blobDigests() []string { return p.blobs }

func parseManifest(data []byte) (*parsedManifest, error) {
	var mv manifestView
	if err := json.Unmarshal(data, &mv); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrManifestInvalid, err)
	}
	if mv.SchemaVersion == 0 && mv.MediaType == "" {
		return nil, fmt.Errorf("%w: missing mediaType/schemaVersion", ErrManifestInvalid)
	}
	p := &parsedManifest{mediaType: mv.MediaType}
	if mv.Config.Digest != "" {
		p.blobs = append(p.blobs, mv.Config.Digest)
	}
	for _, l := range mv.Layers {
		if l.Digest != "" {
			p.blobs = append(p.blobs, l.Digest)
		}
	}
	for _, m := range mv.Manifests {
		if m.Digest != "" {
			p.childManifests = append(p.childManifests, m.Digest)
		}
	}
	return p, nil
}
