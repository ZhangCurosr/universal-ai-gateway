package auth

import "context"

type principalKey struct{}

// WithPrincipal 把已鉴权的 Principal 存入 context（由 httpapi.Authenticate 中间件调用）。
func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFrom 取出 Principal；未鉴权返回 nil, false。
func PrincipalFrom(ctx context.Context) (*Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(*Principal)
	return p, ok && p != nil
}
