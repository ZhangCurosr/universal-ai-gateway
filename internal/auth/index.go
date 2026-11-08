package auth

import (
	"slices"
	"sync/atomic"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/store"
)

// Principal 是已鉴权的客户端身份。由 Index 返回后只读（AUTH-08）。
type Principal struct {
	ClientID      int64
	Name          string
	Role          string
	RPMLimit      int
	AllowedModels []string // 空 = 全部允许
	Disabled      bool
}

// ModelAllowed 实现 AUTH-09。
func (p *Principal) ModelAllowed(model string) bool {
	return len(p.AllowedModels) == 0 || slices.Contains(p.AllowedModels, model)
}

// IsAdmin 报告该客户端是否为管理员。
func (p *Principal) IsAdmin() bool { return p.Role == store.RoleAdmin }

// Index 是 token_hash → Principal 的内存快照，读无锁（AUTH-05、AUTH-06）。
type Index struct {
	snap atomic.Pointer[map[string]*Principal]
}

// NewIndex 创建空索引。
func NewIndex() *Index {
	ix := &Index{}
	empty := map[string]*Principal{}
	ix.snap.Store(&empty)
	return ix
}

// Lookup 按 token_hash 查找。
func (ix *Index) Lookup(tokenHash string) (*Principal, bool) {
	p, ok := (*ix.snap.Load())[tokenHash]
	return p, ok
}

// Replace 用 clients 构建全新快照并原子替换（AUTH-06、AUTH-07）。
func (ix *Index) Replace(clients []store.Client) {
	m := make(map[string]*Principal, len(clients))
	for _, c := range clients {
		m[c.TokenHash] = &Principal{
			ClientID:      c.ID,
			Name:          c.Name,
			Role:          c.Role,
			RPMLimit:      c.RPMLimit,
			AllowedModels: slices.Clone(c.AllowedModels),
			Disabled:      c.Disabled,
		}
	}
	ix.snap.Store(&m)
}

// Len 返回索引中的客户端数量。
func (ix *Index) Len() int { return len(*ix.snap.Load()) }
