package main

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// NodeHealth 是某个从节点的最近一次探测结果.
type NodeHealth struct {
	ID        string `json:"id"`
	Online    bool   `json:"online"`
	LatencyMs int64  `json:"latency_ms"`
	CheckedAt int64  `json:"checked_at"`
	Error     string `json:"error,omitempty"`
	Fails     int    `json:"-"`
}

// HealthMonitor 后台周期性探测所有从节点.
// 用户页面只会看到在线节点, 所以探测结果直接决定节点是否对外可见.
type HealthMonitor struct {
	store    *Store
	client   *http.Client
	interval time.Duration

	mu     sync.RWMutex
	status map[string]*NodeHealth
}

func newHealthMonitor(store *Store, interval time.Duration) *HealthMonitor {
	return &HealthMonitor{
		store: store,
		client: &http.Client{
			Timeout: 6 * time.Second,
			Transport: &http.Transport{
				Proxy:               http.ProxyFromEnvironment,
				MaxIdleConns:        64,
				MaxIdleConnsPerHost: 4,
				IdleConnTimeout:     60 * time.Second,
				DisableCompression:  true,
			},
		},
		interval: interval,
		status:   map[string]*NodeHealth{},
	}
}

// Start 启动后台探测循环, 非阻塞.
func (h *HealthMonitor) Start() {
	go func() {
		time.Sleep(1500 * time.Millisecond) // 让服务先把端口监听起来
		for {
			h.checkAll()
			time.Sleep(h.interval)
		}
	}()
}

// CheckNow 异步探测一轮, 用于"刚添加节点".
func (h *HealthMonitor) CheckNow() {
	go h.checkAll()
}

// CheckAllSync 同步跑完一轮探测, 用于面板手动刷新(需要立刻看到结果).
func (h *HealthMonitor) CheckAllSync() {
	h.checkAll()
}

func (h *HealthMonitor) checkAll() {
	cfg := h.store.Get()
	nodes := cfg.Nodes

	var wg sync.WaitGroup
	for i := range nodes {
		n := nodes[i]
		if !n.Enabled {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.checkOne(n)
		}()
	}
	wg.Wait()
	h.prune(nodes)
}

func (h *HealthMonitor) checkOne(n Node) {
	base := strings.TrimRight(strings.TrimSpace(n.BaseURL), "/")
	if base == "" {
		return
	}

	t0 := time.Now()
	resp, err := h.client.Get(base + "/__ping")
	latency := int64(0)
	ok := false
	errMsg := ""

	if err != nil {
		errMsg = err.Error()
	} else {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 2048))
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			ok = true
			latency = time.Since(t0).Milliseconds()
		} else {
			errMsg = "HTTP " + strconv.Itoa(resp.StatusCode)
		}
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	st := h.status[n.ID]
	if st == nil {
		// 新节点先乐观放行, 避免刚添加就被隐藏
		st = &NodeHealth{ID: n.ID, Online: true}
		h.status[n.ID] = st
	}
	st.CheckedAt = nowUnix()

	if ok {
		st.Fails = 0
		st.Online = true
		st.LatencyMs = latency
		st.Error = ""
	} else {
		st.Fails++
		st.Error = errMsg
		// 连续 2 次失败才判离线, 抵抗瞬时网络抖动
		if st.Fails >= 2 {
			st.Online = false
		}
	}
}

// prune 清掉已删除节点的残留状态.
func (h *HealthMonitor) prune(nodes []Node) {
	alive := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		alive[n.ID] = true
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for id := range h.status {
		if !alive[id] {
			delete(h.status, id)
		}
	}
}

// IsOnline 返回节点是否应当对外展示. 状态未知时乐观放行.
func (h *HealthMonitor) IsOnline(id string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	st := h.status[id]
	if st == nil {
		return true
	}
	return st.Online
}

// Snapshot 返回状态副本, 供管理面板展示.
func (h *HealthMonitor) Snapshot() map[string]NodeHealth {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make(map[string]NodeHealth, len(h.status))
	for id, st := range h.status {
		out[id] = *st
	}
	return out
}

// Get 返回单个节点状态.
func (h *HealthMonitor) Get(id string) (NodeHealth, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	st, ok := h.status[id]
	if !ok {
		return NodeHealth{}, false
	}
	return *st, true
}
