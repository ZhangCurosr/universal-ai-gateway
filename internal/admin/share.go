package admin

import (
	"context"
	"net"
	"net/http"
	"runtime"
	"sort"
	"strconv"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/apperr"
	"github.com/ZhangCurosr/universal-ai-gateway/internal/auth"
)

// ShareDeps 是局域网共享（T71）需要的、由装配代码注入的能力。
type ShareDeps struct {
	// Writable 报告配置文件能否被网页修改（有路径且可写）。
	Writable func() bool
	// SetLANOpen 把 server.data_listen 改为 0.0.0.0:<端口> 或 127.0.0.1:<端口> 并重载（LAN-02、LAN-03）；
	// 返回值与重载 API 相同（ADM-18）。值没有变化时返回 200 {"changed": false}。
	SetLANOpen func(ctx context.Context, actor int64, open bool) (status int, body any)
	// Interfaces 列出网卡地址；nil 时用 net.Interfaces（测试注入）。
	Interfaces func() ([]ShareAddress, error)
}

// ShareAddress 是一个可供局域网设备访问的地址（LAN-01）。
type ShareAddress struct {
	IP        string `json:"ip"`
	Interface string `json:"interface"`
	URL       string `json:"url,omitempty"` // http://<IP>:<端口>，由服务端填写
}

// ShareView 是 GET /admin/api/share 的响应（LAN-01）。
type ShareView struct {
	Listen         string         `json:"listen"`
	Port           int            `json:"port"`
	LANOpen        bool           `json:"lan_open"`
	Addresses      []ShareAddress `json:"addresses"`
	OS             string         `json:"os"`
	ConfigWritable bool           `json:"config_writable"`
	// URLTemplate 是没有局域网地址时片段中使用的占位地址 http://<IP>:<端口>。
	URLTemplate string `json:"url_template"`
}

// LANOpen 按 LAN-01 判断监听地址是否对局域网开放：主机为空、0.0.0.0、:: 或非回环地址即为开放。
func LANOpen(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return false
	}
	if host == "" || host == "localhost" {
		return host == ""
	}
	ip := net.ParseIP(host)
	return ip == nil || !ip.IsLoopback()
}

// LANAddresses 返回启用的网卡上的非回环、非链路本地 IPv4 地址，按网卡名与地址排序（LAN-01）。
func LANAddresses() ([]ShareAddress, error) {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	out := []ShareAddress{}
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue // 读不到某块网卡的地址时跳过它
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipn.IP.To4()
			if ip4 == nil || ip4.IsLoopback() || ip4.IsLinkLocalUnicast() || ip4.IsUnspecified() {
				continue
			}
			out = append(out, ShareAddress{IP: ip4.String(), Interface: ifc.Name})
		}
	}
	SortAddresses(out)
	return out, nil
}

// addrRank：家庭与办公网络最常用的网段排在前面（192.168/16、172.16/12、10/8），其他（如 VPN 的 100.64/10）在后。
func addrRank(ip string) int {
	p := net.ParseIP(ip).To4()
	switch {
	case p == nil:
		return 4
	case p[0] == 192 && p[1] == 168:
		return 0
	case p[0] == 172 && p[1]&0xf0 == 16:
		return 1
	case p[0] == 10:
		return 2
	}
	return 3
}

// SortAddresses 按 addrRank、网卡名、地址排序（LAN-01：第一项是页面的缺省选择）。
func SortAddresses(a []ShareAddress) {
	sort.SliceStable(a, func(i, j int) bool {
		ri, rj := addrRank(a[i].IP), addrRank(a[j].IP)
		if ri != rj {
			return ri < rj
		}
		if a[i].Interface != a[j].Interface {
			return a[i].Interface < a[j].Interface
		}
		return a[i].IP < a[j].IP
	})
}

// handleShareGet 实现 LAN-01。
func (s *server) handleShareGet(w http.ResponseWriter, _ *http.Request) {
	listen := s.config().Server.DataListen
	_, portStr, _ := net.SplitHostPort(listen) // CFG-01 已保证可以解析
	port, _ := strconv.Atoi(portStr)           // 同上
	v := ShareView{Listen: listen, Port: port, LANOpen: LANOpen(listen), Addresses: []ShareAddress{}, OS: runtime.GOOS}
	sd := s.d.Share
	if sd != nil && sd.Writable != nil {
		v.ConfigWritable = sd.Writable()
	}
	list := LANAddresses
	if sd != nil && sd.Interfaces != nil {
		list = sd.Interfaces
	}
	v.URLTemplate = "http://" + net.JoinHostPort("<IP>", portStr)
	if addrs, err := list(); err == nil && addrs != nil {
		for i := range addrs {
			addrs[i].URL = "http://" + net.JoinHostPort(addrs[i].IP, portStr)
		}
		v.Addresses = addrs
	} else if err != nil {
		s.d.Logger.Warn("share: list network interfaces", "err", err)
	}
	writeJSON(w, http.StatusOK, v)
}

// handleShareSet 实现 LAN-02。
func (s *server) handleShareSet(w http.ResponseWriter, r *http.Request) {
	sd := s.d.Share
	if sd == nil || sd.SetLANOpen == nil || sd.Writable == nil || !sd.Writable() {
		writeError(w, apperr.New(apperr.CodeConflict, "the config file cannot be edited from the web page; change server.data_listen by hand"))
		return
	}
	var req struct {
		LANOpen *bool `json:"lan_open"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	if req.LANOpen == nil {
		writeError(w, apperr.New(apperr.CodeInvalidRequest, "lan_open is required"))
		return
	}
	var actor int64
	if p, ok := auth.PrincipalFrom(r.Context()); ok && p != nil {
		actor = p.ClientID
	}
	status, body := sd.SetLANOpen(r.Context(), actor, *req.LANOpen)
	writeJSON(w, status, body)
}
