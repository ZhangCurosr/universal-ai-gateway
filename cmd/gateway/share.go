package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"sync"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/admin"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/config"
)

var dataListenPath = []string{"server", "data_listen"}

// shareEditor 实现局域网共享的开关（T71，LAN-02 ~ LAN-04）：改配置文件中的 server.data_listen，再按 HR 规则重载。
type shareEditor struct {
	rl *configReloader
	mu sync.Mutex
}

func (e *shareEditor) deps() *admin.ShareDeps {
	return &admin.ShareDeps{Writable: e.writable, SetLANOpen: e.setLANOpen}
}

// writable 报告配置文件能否被修改：有路径、是普通文件、可以以写方式打开。
func (e *shareEditor) writable() bool {
	if e.rl.path == "" {
		return false
	}
	st, err := os.Stat(e.rl.path)
	if err != nil || !st.Mode().IsRegular() {
		return false
	}
	f, err := os.OpenFile(e.rl.path, os.O_WRONLY, 0)
	if err != nil {
		return false
	}
	_ = f.Close() // 只是探测，没有写入
	return true
}

func shareError(status int, code, msg string) (int, any) {
	return status, map[string]any{"error": map[string]any{"code": code, "message": msg}}
}

func (e *shareEditor) setLANOpen(ctx context.Context, actor int64, open bool) (int, any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	cur := e.rl.cur.Load().Server.DataListen
	_, port, err := net.SplitHostPort(cur)
	if err != nil {
		return shareError(http.StatusInternalServerError, "internal", "server.data_listen cannot be parsed")
	}
	if admin.LANOpen(cur) == open {
		return http.StatusOK, map[string]any{"changed": false, "listen": cur}
	}
	host := "127.0.0.1"
	if open {
		host = "0.0.0.0"
	}
	want := net.JoinHostPort(host, port)
	orig, old, err := config.EditFile(e.rl.path, dataListenPath, want)
	wrote := err == nil
	switch {
	case errors.Is(err, config.ErrUnchanged): // 文件已经是这个值（手工改过但还没重载）：直接重载
	case err != nil:
		return shareError(http.StatusConflict, "conflict", err.Error())
	}
	e.rl.logger.Info("config edited by admin", "field", "server.data_listen", "old", old, "new", want, "actor", actor) // LAN-04
	status, body := e.rl.Reload(ctx, actor, false)
	if status >= http.StatusBadRequest {
		if wrote { // LAN-03：重载失败时恢复原文件
			if rerr := config.WriteFileAtomic(e.rl.path, orig); rerr != nil {
				e.rl.logger.Error("restore config file after a failed reload", "err", rerr)
			} else {
				e.rl.logger.Warn("config file restored after a failed reload", "field", "server.data_listen")
			}
		}
		return status, body
	}
	return status, map[string]any{"changed": true, "listen": want, "reload": body}
}
