// Package files 是文件中转的存储（T69，FS-01 ~ FS-05）：文件内容与元数据放在同一目录，
// 启动时扫描目录重建内存索引；不访问数据库。
package files

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
)

// Limits 是 FS-03 / FS-04 的上限。
type Limits struct {
	MaxFileBytes      int64
	MaxFilesPerClient int
	MaxBytesPerClient int64
	MaxTotalBytes     int64
	DefaultTTL        time.Duration
	MaxTTL            time.Duration
}

// Meta 是一个文件的元数据（FS-01），也是 FS-10 的响应（Owner 不输出）。
type Meta struct {
	ID          string    `json:"id"`
	Owner       int64     `json:"-"`
	Name        string    `json:"name"`
	Size        int64     `json:"size"`
	SHA256      string    `json:"sha256"`
	ContentType string    `json:"content_type"`
	CreatedAt   time.Time `json:"created_at"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// diskMeta 是 <id>.json 的内容（含 owner）。
type diskMeta struct {
	Meta
	Owner int64 `json:"owner"`
}

// Usage 是一个客户端（或全网关）的用量。
type Usage struct {
	Files int   `json:"files"`
	Bytes int64 `json:"bytes"`
}

// SweepInterval 是到期清理的间隔（FS-04）。
const SweepInterval = time.Minute

// MaxNameBytes 是文件名的上限（FS-10）。
const MaxNameBytes = 255

var (
	idPattern        = regexp.MustCompile(`^file_[a-z2-7]{26}$`)
	nameQueryPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`) // 与 config.FileNameQuery 相同
)

var idEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

type entry struct {
	meta    Meta
	readers int
	removed bool // 已从索引中删除；最后一个读者关闭时删除磁盘文件
}

// Store 是文件存储。方法可以并发调用。
type Store struct {
	dir string
	lim Limits
	now func() time.Time
	log *slog.Logger

	mu     sync.Mutex
	files  map[string]*entry
	owners map[int64]*Usage // 含正在写入的预占
	total  int64
}

// Open 打开（必要时创建）目录 dir，删除残留的临时文件与不完整的文件，重建索引（FS-01）。
// now 为 nil 时用 time.Now。
func Open(dir string, lim Limits, now func() time.Time, log *slog.Logger) (*Store, error) {
	if now == nil {
		now = time.Now
	}
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("files: create %s: %w", dir, err)
	}
	s := &Store{dir: dir, lim: lim, now: now, log: log, files: map[string]*entry{}, owners: map[int64]*Usage{}}
	if err := s.scan(); err != nil {
		return nil, err
	}
	s.Sweep()
	return s, nil
}

func (s *Store) path(id, ext string) string { return filepath.Join(s.dir, id+ext) }

func (s *Store) scan() error {
	des, err := os.ReadDir(s.dir)
	if err != nil {
		return fmt.Errorf("files: read %s: %w", s.dir, err)
	}
	seen := map[string]bool{}
	for _, de := range des {
		name := de.Name()
		if de.IsDir() {
			continue
		}
		id, ext := strings.TrimSuffix(name, filepath.Ext(name)), filepath.Ext(name)
		switch ext {
		case ".tmp":
			s.remove(name, "leftover temporary file")
		case ".json":
			if !idPattern.MatchString(id) {
				continue // 不是本包写的文件，不动
			}
			seen[id] = true
			m, ok := s.loadMeta(id)
			if !ok {
				s.remove(id+".json", "broken metadata")
				s.remove(id+".bin", "broken metadata")
				continue
			}
			s.files[id] = &entry{meta: m}
			s.charge(m.Owner, 1, m.Size)
		}
	}
	for _, de := range des {
		id, ext := strings.TrimSuffix(de.Name(), ".bin"), filepath.Ext(de.Name())
		if ext == ".bin" && idPattern.MatchString(id) && !seen[id] {
			s.remove(de.Name(), "content without metadata")
		}
	}
	return nil
}

// loadMeta 读取并校验 <id>.json，内容文件必须存在且大小一致。
func (s *Store) loadMeta(id string) (Meta, bool) {
	raw, err := os.ReadFile(s.path(id, ".json"))
	if err != nil || len(raw) > 64<<10 {
		return Meta{}, false
	}
	var dm diskMeta
	if json.Unmarshal(raw, &dm) != nil || dm.ID != id || dm.Size <= 0 {
		return Meta{}, false
	}
	st, err := os.Stat(s.path(id, ".bin"))
	if err != nil || st.Size() != dm.Size {
		return Meta{}, false
	}
	m := dm.Meta
	m.Owner = dm.Owner
	return m, true
}

// remove 删除目录中的一个文件；失败只记日志（只写文件名中的 ID）。
func (s *Store) remove(name, why string) {
	err := os.Remove(filepath.Join(s.dir, name))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		s.log.Warn("files: remove failed", "file", name, "err", err)
		return
	}
	if why != "" {
		s.log.Warn("files: removed "+why, "file", name)
	}
}

func (s *Store) charge(owner int64, n int, bytes int64) {
	u := s.owners[owner]
	if u == nil {
		u = &Usage{}
		s.owners[owner] = u
	}
	u.Files += n
	u.Bytes += bytes
	s.total += bytes
	if u.Files == 0 && u.Bytes == 0 {
		delete(s.owners, owner)
	}
}

// CleanName 实现 FS-10 的文件名规则：取最后一个 / 或 \ 之后的部分，≤ 255 字节，不含控制字符。
func CleanName(name string) (string, error) {
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	switch {
	case name == "" || name == "." || name == "..":
		return "", apperr.New(apperr.CodeInvalidRequest, "name is required")
	case len(name) > MaxNameBytes:
		return "", apperr.New(apperr.CodeInvalidRequest, fmt.Sprintf("name must be at most %d bytes", MaxNameBytes))
	case !utf8.ValidString(name) || strings.IndexFunc(name, unicode.IsControl) >= 0:
		return "", apperr.New(apperr.CodeInvalidRequest, "name must be valid UTF-8 without control characters")
	}
	return name, nil
}

// TTL 校验上传时指定的保存时间（FS-04）：0 取缺省值。
func (s *Store) TTL(ttl time.Duration) (time.Duration, error) {
	switch {
	case ttl == 0:
		return s.lim.DefaultTTL, nil
	case ttl < 0:
		return 0, apperr.New(apperr.CodeInvalidRequest, "ttl must be positive")
	case ttl > s.lim.MaxTTL:
		return 0, apperr.New(apperr.CodeInvalidRequest, fmt.Sprintf("ttl must be at most %s", s.lim.MaxTTL))
	}
	return ttl, nil
}

// Limits 返回上限。
func (s *Store) Limits() Limits { return s.lim }

// reserve 按 FS-03 预占一个文件的配额。
func (s *Store) reserve(owner int64, size int64) error {
	if size > s.lim.MaxFileBytes {
		return apperr.New(apperr.CodeRequestTooLarge, fmt.Sprintf("file exceeds files.max_file_bytes (%d bytes)", s.lim.MaxFileBytes))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.owners[owner]
	if u == nil {
		u = &Usage{}
	}
	switch {
	case u.Files+1 > s.lim.MaxFilesPerClient:
		return apperr.New(apperr.CodeConflict, fmt.Sprintf("this client already keeps %d files (files.max_files_per_client); delete old files or wait for them to expire", u.Files))
	case u.Bytes+size > s.lim.MaxBytesPerClient:
		return apperr.New(apperr.CodeConflict, fmt.Sprintf("this client would keep more than %d bytes (files.max_bytes_per_client); delete old files or wait for them to expire", s.lim.MaxBytesPerClient))
	case s.total+size > s.lim.MaxTotalBytes:
		return apperr.New(apperr.CodeConflict, "the gateway file store is full (files.max_total_bytes); try again later")
	}
	s.charge(owner, 1, size)
	return nil
}

func (s *Store) release(owner int64, size int64) {
	s.mu.Lock()
	s.charge(owner, -1, -size)
	s.mu.Unlock()
}

func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "file_" + strings.ToLower(idEncoding.EncodeToString(b[:])), nil
}

// Put 保存一个文件（FS-01 ~ FS-04、FS-10）：r 必须恰好提供 size 字节。
// name 必须已经过 CleanName，ttl 必须已经过 TTL。
func (s *Store) Put(owner int64, name, contentType string, size int64, ttl time.Duration, r io.Reader) (Meta, error) {
	if size <= 0 {
		return Meta{}, apperr.New(apperr.CodeInvalidRequest, "Content-Length is required and must be > 0")
	}
	if err := s.reserve(owner, size); err != nil {
		return Meta{}, err
	}
	m, err := s.write(owner, name, contentType, size, ttl, r)
	if err != nil {
		s.release(owner, size)
		return Meta{}, err
	}
	s.mu.Lock()
	s.files[m.ID] = &entry{meta: m}
	s.mu.Unlock()
	s.log.Info("files: stored", "client_id", owner, "file_id", m.ID, "size", m.Size)
	return m, nil
}

func (s *Store) write(owner int64, name, contentType string, size int64, ttl time.Duration, r io.Reader) (Meta, error) {
	id, err := newID()
	if err != nil {
		return Meta{}, apperr.Wrap(apperr.CodeInternal, "could not store the file", err)
	}
	tmp := s.path(id, ".tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return Meta{}, apperr.Wrap(apperr.CodeInternal, "could not store the file", err)
	}
	h := sha256.New()
	n, cerr := io.Copy(io.MultiWriter(f, h), io.LimitReader(r, size+1))
	serr := f.Sync()
	if err := f.Close(); err != nil && cerr == nil && serr == nil {
		serr = err
	}
	fail := func(e error) (Meta, error) {
		s.remove(id+".tmp", "")
		return Meta{}, e
	}
	switch {
	case cerr != nil:
		return fail(apperr.Wrap(apperr.CodeInvalidRequest, "could not read the request body", cerr))
	case n != size:
		return fail(apperr.New(apperr.CodeInvalidRequest, "request body length does not match Content-Length"))
	case serr != nil:
		return fail(apperr.Wrap(apperr.CodeInternal, "could not store the file", serr))
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	now := s.now().UTC().Truncate(time.Second)
	m := Meta{ID: id, Owner: owner, Name: name, Size: size, SHA256: hex.EncodeToString(h.Sum(nil)),
		ContentType: contentType, CreatedAt: now, ExpiresAt: now.Add(ttl)}
	if err := os.Rename(tmp, s.path(id, ".bin")); err != nil {
		return fail(apperr.Wrap(apperr.CodeInternal, "could not store the file", err))
	}
	raw, err := json.Marshal(diskMeta{Meta: m, Owner: owner})
	if err == nil {
		err = writeFileAtomic(s.path(id, ".json"), raw)
	}
	if err != nil {
		s.remove(id+".bin", "")
		return Meta{}, apperr.Wrap(apperr.CodeInternal, "could not store the file", err)
	}
	return m, nil
}

func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp) // 清理：失败无影响
		return err
	}
	return nil
}

var errNotFound = apperr.New(apperr.CodeNotFound, "file expired or not found")

// lookup 返回 owner 可见、未到期的文件；调用者持有锁。owner < 0 表示管理员（不检查归属）。
func (s *Store) lookup(owner int64, id string) (*entry, bool) {
	e, ok := s.files[id]
	if !ok || (owner >= 0 && e.meta.Owner != owner) || !s.now().Before(e.meta.ExpiresAt) {
		return nil, false
	}
	return e, true
}

// Get 返回元数据（FS-02：不属于 owner 时与不存在相同）。
func (s *Store) Get(owner int64, id string) (Meta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.lookup(owner, id)
	if !ok {
		return Meta{}, errNotFound
	}
	return e.meta, nil
}

// File 是一个打开的文件；Close 之后到期或删除的文件才会从磁盘删除（FS-04）。
type File struct {
	Meta
	f     *os.File
	s     *Store
	e     *entry
	close sync.Once
}

func (f *File) Read(p []byte) (int, error) { return f.f.Read(p) }

// Close 关闭文件并释放引用。
func (f *File) Close() error {
	var err error
	f.close.Do(func() {
		err = f.f.Close()
		f.s.mu.Lock()
		f.e.readers--
		gone := f.e.removed && f.e.readers == 0
		f.s.mu.Unlock()
		if gone {
			f.s.removeDisk(f.e.meta.ID)
		}
	})
	return err
}

// OpenFile 打开 owner 的文件用作请求体（FS-20）。
func (s *Store) OpenFile(owner int64, id string) (*File, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.lookup(owner, id)
	if !ok {
		return nil, errNotFound
	}
	f, err := os.Open(s.path(id, ".bin"))
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "could not read the file", err)
	}
	e.readers++
	return &File{Meta: e.meta, f: f, s: s, e: e}, nil
}

// List 返回 owner 未到期的文件（按创建时间倒序）与用量（FS-11）。
func (s *Store) List(owner int64) ([]Meta, Usage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Meta{}
	var u Usage
	for _, e := range s.files {
		if e.meta.Owner == owner && s.now().Before(e.meta.ExpiresAt) {
			out = append(out, e.meta)
			u.Files++
			u.Bytes += e.meta.Size
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, u
}

// Delete 删除 owner 的文件（FS-11）。
func (s *Store) Delete(owner int64, id string) error {
	return s.delete(owner, id, "deleted")
}

// AdminDelete 删除任意文件（FS-50）。
func (s *Store) AdminDelete(id string) error {
	return s.delete(-1, id, "deleted by admin")
}

func (s *Store) delete(owner int64, id, why string) error {
	s.mu.Lock()
	e, ok := s.lookup(owner, id)
	if !ok {
		s.mu.Unlock()
		return errNotFound
	}
	now := s.drop(e)
	s.mu.Unlock()
	if now {
		s.removeDisk(id)
	}
	s.log.Info("files: "+why, "client_id", e.meta.Owner, "file_id", id)
	return nil
}

// drop 从索引中删除并释放配额；返回是否可以立即删除磁盘文件。调用者持有锁。
func (s *Store) drop(e *entry) bool {
	delete(s.files, e.meta.ID)
	s.charge(e.meta.Owner, -1, -e.meta.Size)
	e.removed = true
	return e.readers == 0
}

func (s *Store) removeDisk(id string) {
	s.remove(id+".json", "")
	s.remove(id+".bin", "")
}

// Sweep 删除到期的文件（FS-04），返回删除的个数。
func (s *Store) Sweep() int {
	s.mu.Lock()
	var now []string
	n := 0
	for _, e := range s.files {
		if !s.now().Before(e.meta.ExpiresAt) {
			n++
			if s.drop(e) {
				now = append(now, e.meta.ID)
			}
		}
	}
	s.mu.Unlock()
	for _, id := range now {
		s.removeDisk(id)
	}
	if n > 0 {
		s.log.Info("files: expired", "count", n)
	}
	return n
}

// Run 每 SweepInterval 清理一次，直到 ctx 结束。
func (s *Store) Run(ctx context.Context) error {
	t := time.NewTicker(SweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			s.Sweep()
		}
	}
}

// ClientUsage 是 FS-50 中一个客户端的用量。
type ClientUsage struct {
	ClientID int64 `json:"client_id"`
	Usage
}

// Expiring 是 FS-50 中即将到期的文件（不含文件名）。
type Expiring struct {
	ID        string    `json:"id"`
	ClientID  int64     `json:"client_id"`
	Size      int64     `json:"size"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Stats 是 FS-50 的管理视图。
type Stats struct {
	Used     Usage         `json:"used"`
	Clients  []ClientUsage `json:"clients"`
	Expiring []Expiring    `json:"expiring"`
}

// Stats 返回总用量、每个客户端的用量（按客户端 ID 升序）与最近到期的 20 个文件。
func (s *Store) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Stats{Clients: []ClientUsage{}, Expiring: []Expiring{}}
	per := map[int64]*Usage{}
	for _, e := range s.files {
		u := per[e.meta.Owner]
		if u == nil {
			u = &Usage{}
			per[e.meta.Owner] = u
		}
		u.Files++
		u.Bytes += e.meta.Size
		st.Used.Files++
		st.Used.Bytes += e.meta.Size
		st.Expiring = append(st.Expiring, Expiring{ID: e.meta.ID, ClientID: e.meta.Owner, Size: e.meta.Size, ExpiresAt: e.meta.ExpiresAt})
	}
	for id, u := range per {
		st.Clients = append(st.Clients, ClientUsage{ClientID: id, Usage: *u})
	}
	sort.Slice(st.Clients, func(i, j int) bool { return st.Clients[i].ClientID < st.Clients[j].ClientID })
	sort.Slice(st.Expiring, func(i, j int) bool {
		if !st.Expiring[i].ExpiresAt.Equal(st.Expiring[j].ExpiresAt) {
			return st.Expiring[i].ExpiresAt.Before(st.Expiring[j].ExpiresAt)
		}
		return st.Expiring[i].ID < st.Expiring[j].ID
	})
	if len(st.Expiring) > 20 {
		st.Expiring = st.Expiring[:20]
	}
	return st
}

// ValidID 报告 id 是否符合文件 ID 的格式（FS-02）。
func ValidID(id string) bool { return idPattern.MatchString(id) }
