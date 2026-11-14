package files

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

var testLimits = Limits{MaxFileBytes: 100, MaxFilesPerClient: 3, MaxBytesPerClient: 150, MaxTotalBytes: 200, DefaultTTL: time.Hour, MaxTTL: 24 * time.Hour}

func newStore(t *testing.T) (*Store, *clock, string) {
	t.Helper()
	dir := t.TempDir()
	c := &clock{t: time.Date(2026, 11, 14, 8, 0, 0, 0, time.UTC)}
	s, err := Open(dir, testLimits, c.now, nil)
	if err != nil {
		t.Fatal(err)
	}
	return s, c, dir
}

func put(t *testing.T, s *Store, owner int64, body string) Meta {
	t.Helper()
	m, err := s.Put(owner, "a.pdf", "application/pdf", int64(len(body)), time.Hour, strings.NewReader(body))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	return m
}

func code(err error) apperr.Code {
	var ae *apperr.Error
	if errors.As(err, &ae) {
		return ae.Code
	}
	return ""
}

func TestPutAndOpen(t *testing.T) {
	s, _, dir := newStore(t)
	m := put(t, s, 7, "hello pdf")
	sum := sha256.Sum256([]byte("hello pdf"))
	if !ValidID(m.ID) || m.Size != 9 || m.SHA256 != hex.EncodeToString(sum[:]) || m.ContentType != "application/pdf" || !m.ExpiresAt.Equal(m.CreatedAt.Add(time.Hour)) {
		t.Fatalf("meta = %+v", m)
	}
	f, err := s.OpenFile(7, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(f)
	_ = f.Close() // 测试：关闭失败无影响
	if string(b) != "hello pdf" {
		t.Fatalf("content = %q", b)
	}
	if _, err := os.Stat(filepath.Join(dir, m.ID+".json")); err != nil {
		t.Fatal(err)
	}
	// FS-02：其他客户端与不存在相同
	if _, err := s.OpenFile(8, m.ID); code(err) != apperr.CodeNotFound {
		t.Fatalf("other owner: %v", err)
	}
	if _, err := s.Get(8, m.ID); code(err) != apperr.CodeNotFound {
		t.Fatalf("other owner get: %v", err)
	}
	if err := s.Delete(8, m.ID); code(err) != apperr.CodeNotFound {
		t.Fatalf("other owner delete: %v", err)
	}
}

func TestPutDefaultContentType(t *testing.T) {
	s, _, _ := newStore(t)
	m, err := s.Put(1, "x", "", 1, time.Hour, strings.NewReader("x"))
	if err != nil || m.ContentType != "application/octet-stream" {
		t.Fatalf("%+v %v", m, err)
	}
}

func TestPutLengthMismatchReleases(t *testing.T) {
	s, _, dir := newStore(t)
	for _, body := range []string{"short", "much longer than ten"} {
		if _, err := s.Put(1, "a", "", 10, time.Hour, strings.NewReader(body)); code(err) != apperr.CodeInvalidRequest {
			t.Fatalf("%q: %v", body, err)
		}
	}
	if _, u := s.List(1); u.Files != 0 || s.total != 0 || len(s.owners) != 0 {
		t.Fatalf("reservation not released: %+v total=%d", u, s.total)
	}
	des, _ := os.ReadDir(dir)
	if len(des) != 0 {
		t.Fatalf("leftover files: %d", len(des))
	}
	if _, err := s.Put(1, "a", "", 0, time.Hour, strings.NewReader("")); code(err) != apperr.CodeInvalidRequest {
		t.Fatalf("empty: %v", err)
	}
}

type failReader struct{}

func (failReader) Read([]byte) (int, error) { return 0, errors.New("connection reset") }

func TestPutReadErrorReleases(t *testing.T) {
	s, _, _ := newStore(t)
	if _, err := s.Put(1, "a", "", 10, time.Hour, failReader{}); code(err) != apperr.CodeInvalidRequest {
		t.Fatalf("%v", err)
	}
	if s.total != 0 {
		t.Fatalf("total = %d", s.total)
	}
}

func TestLimits(t *testing.T) {
	s, _, _ := newStore(t)
	if _, err := s.Put(1, "a", "", 101, time.Hour, strings.NewReader(strings.Repeat("x", 101))); code(err) != apperr.CodeRequestTooLarge {
		t.Fatalf("too large: %v", err)
	}
	put(t, s, 1, strings.Repeat("x", 100))
	if _, err := s.Put(1, "a", "", 51, time.Hour, strings.NewReader(strings.Repeat("x", 51))); code(err) != apperr.CodeConflict ||
		!strings.Contains(err.Error(), "max_bytes_per_client") {
		t.Fatalf("per-client bytes: %v", err)
	}
	put(t, s, 1, "a")
	put(t, s, 1, "b")
	if _, err := s.Put(1, "a", "", 1, time.Hour, strings.NewReader("c")); code(err) != apperr.CodeConflict || !strings.Contains(err.Error(), "max_files_per_client") {
		t.Fatalf("per-client count: %v", err)
	}
	put(t, s, 2, strings.Repeat("y", 90))
	if _, err := s.Put(3, "a", "", 20, time.Hour, strings.NewReader(strings.Repeat("z", 20))); code(err) != apperr.CodeConflict || !strings.Contains(err.Error(), "max_total_bytes") {
		t.Fatalf("total: %v", err)
	}
}

func TestTTL(t *testing.T) {
	s, _, _ := newStore(t)
	if d, err := s.TTL(0); err != nil || d != time.Hour {
		t.Fatalf("default: %v %v", d, err)
	}
	if d, err := s.TTL(2 * time.Hour); err != nil || d != 2*time.Hour {
		t.Fatalf("explicit: %v %v", d, err)
	}
	for _, d := range []time.Duration{-time.Second, 25 * time.Hour} {
		if _, err := s.TTL(d); code(err) != apperr.CodeInvalidRequest {
			t.Fatalf("%v: %v", d, err)
		}
	}
}

func TestCleanName(t *testing.T) {
	for in, want := range map[string]string{"a.pdf": "a.pdf", `C:\docs\报告.pdf`: "报告.pdf", "/tmp/x/y.txt": "y.txt"} {
		if got, err := CleanName(in); err != nil || got != want {
			t.Errorf("%q: %q %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "dir/", "..", "a\nb", strings.Repeat("a", 256), "\xff"} {
		if _, err := CleanName(bad); code(err) != apperr.CodeInvalidRequest {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestExpiryAndDeferredDelete(t *testing.T) {
	s, c, dir := newStore(t)
	m := put(t, s, 1, "abc")
	f, err := s.OpenFile(1, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	c.add(time.Hour)
	if _, err := s.Get(1, m.ID); code(err) != apperr.CodeNotFound {
		t.Fatalf("expired get: %v", err)
	}
	if n := s.Sweep(); n != 1 {
		t.Fatalf("swept %d", n)
	}
	// 正在读取：磁盘文件保留到关闭
	if _, err := os.Stat(filepath.Join(dir, m.ID+".bin")); err != nil {
		t.Fatalf("removed while open: %v", err)
	}
	b, _ := io.ReadAll(f)
	if string(b) != "abc" {
		t.Fatalf("read %q", b)
	}
	_ = f.Close() // 测试：关闭失败无影响
	_ = f.Close() // 第二次关闭不重复释放
	if des, _ := os.ReadDir(dir); len(des) != 0 {
		t.Fatalf("leftover after close: %d", len(des))
	}
	if s.total != 0 {
		t.Fatalf("total = %d", s.total)
	}
}

func TestDeleteAndList(t *testing.T) {
	s, c, dir := newStore(t)
	a := put(t, s, 1, "a")
	c.add(time.Second)
	b := put(t, s, 1, "bb")
	put(t, s, 2, "c")
	list, u := s.List(1)
	if len(list) != 2 || list[0].ID != b.ID || list[1].ID != a.ID || u != (Usage{Files: 2, Bytes: 3}) {
		t.Fatalf("list = %+v %+v", list, u)
	}
	if err := s.Delete(1, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, a.ID+".bin")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("not removed: %v", err)
	}
	if err := s.Delete(1, a.ID); code(err) != apperr.CodeNotFound {
		t.Fatalf("second delete: %v", err)
	}
	if err := s.AdminDelete(b.ID); err != nil {
		t.Fatal(err)
	}
	st := s.Stats()
	if st.Used != (Usage{Files: 1, Bytes: 1}) || len(st.Clients) != 1 || st.Clients[0].ClientID != 2 || len(st.Expiring) != 1 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestReopenRebuildsAndCleans(t *testing.T) {
	s, c, dir := newStore(t)
	keep := put(t, s, 1, "keep")
	broken := put(t, s, 1, "broken")
	expired, err := s.Put(2, "e", "", 1, time.Minute, strings.NewReader("e"))
	if err != nil {
		t.Fatal(err)
	}
	// 损坏的元数据、残留的临时文件、没有元数据的内容、不是本包的文件
	if err := os.WriteFile(filepath.Join(dir, broken.ID+".json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	orphan := "file_" + strings.Repeat("a", 26)
	for name, body := range map[string]string{orphan + ".bin": "x", "file_x.tmp": "x", "notes.txt": "keep me"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	c.add(2 * time.Minute)
	s2, err := Open(dir, testLimits, c.now, nil)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := s2.Get(1, keep.ID); err != nil || m.Name != "a.pdf" || m.Owner != 1 {
		t.Fatalf("kept file: %+v %v", m, err)
	}
	for _, id := range []string{broken.ID, expired.ID} {
		if _, err := s2.Get(-1, id); err == nil {
			t.Fatalf("%s survived", id)
		}
	}
	var names []string
	des, _ := os.ReadDir(dir)
	for _, de := range des {
		names = append(names, de.Name())
	}
	want := []string{keep.ID + ".bin", keep.ID + ".json", "notes.txt"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("dir = %v, want %v", names, want)
	}
	if st := s2.Stats(); st.Used != (Usage{Files: 1, Bytes: 4}) {
		t.Fatalf("usage = %+v", st.Used)
	}
}

func TestConcurrentPut(t *testing.T) {
	s, _, _ := newStore(t)
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Put(1, "a", "", 1, time.Hour, strings.NewReader("x")); err == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if ok != testLimits.MaxFilesPerClient {
		t.Fatalf("stored %d, want %d", ok, testLimits.MaxFilesPerClient)
	}
}
