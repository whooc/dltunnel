package main

import (
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

//go:embed web
var webFS embed.FS

// errAccessPasswordTooShort 访问口令太短 (在 Update 回调里返回, 由外层翻译成 400)。
var errAccessPasswordTooShort = errors.New("访问口令太短")

const sessionTTL = 12 * time.Hour

// accessTTL 用户页面验证通过后的有效期. 比管理会话长很多 ——
// 普通用户不该每天都被要求重新输一次口令。
const accessTTL = 30 * 24 * time.Hour

// accessCookie 用户页面的验证 cookie 名 (与管理面板的 dlt_session 分开)。
const accessCookie = "dlt_access"


// 节点健康探测间隔
const healthInterval = 30 * time.Second

type Master struct {
	store   *Store
	health  *HealthMonitor
	records *RecordStore
	binDir  string
}

func runMaster(listen, dataDir, binDir string) {
	store, err := loadStore(dataDir)
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}
	records, err := loadRecords(dataDir)
	if err != nil {
		log.Fatalf("加载下载记录失败: %v", err)
	}
	gStats.Started = nowUnix()

	if binDir == "" {
		binDir = filepath.Join(filepath.Dir(dataDir), "bin")
	}
	if abs, err := filepath.Abs(binDir); err == nil {
		binDir = abs
	}

	m := &Master{store: store, records: records, binDir: binDir}

	// 记录保留策略: 启动时先清一次, 之后每小时清一次
	m.pruneRecords()
	go func() {
		for {
			time.Sleep(time.Hour)
			m.pruneRecords()
		}
	}()

	// 节点健康探测: 离线的从节点不会出现在用户页面
	m.health = newHealthMonitor(store, healthInterval)
	m.health.Start()

	srv := &http.Server{
		Addr:              listen,
		Handler:           m,
		ReadHeaderTimeout: 20 * time.Second,
		IdleTimeout:       120 * time.Second,
		// 不设 WriteTimeout: 大文件中转可能持续数十分钟
	}
	log.Printf("主服务器已启动 监听=%s 版本=%s", listen, version)
	log.Printf("  用户页面:  http://<服务器IP>%s/", listen)
	log.Printf("  管理面板:  http://<服务器IP>%s/admin", listen)
	log.Printf("  从节点安装: curl -fsSL http://<服务器IP>%s/agent.sh | bash -s -- --secret <密钥>", listen)
	log.Printf("  二进制目录: %s", binDir)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("主服务器退出: %v", err)
	}
}

// pruneRecords 按配置的保留天数清理下载记录.
func (m *Master) pruneRecords() {
	days := m.store.Get().RetainDays()
	if days == 0 {
		// "不记录"模式: 只停止新增, 不清空已有历史.
		// 想清空历史请用面板上的「清空全部」按钮。
		return
	}
	if n := m.records.Prune(days); n > 0 {
		log.Printf("下载记录清理: 删除 %d 条 (保留策略 %d 天)", n, days)
	}
}

func (m *Master) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")

	p := r.URL.Path
	switch {
	case p == "/" || p == "/index.html":
		m.servePage(w, "index.html")
	case p == "/admin" || p == "/admin/":
		m.servePage(w, "admin.html")
	case p == "/health":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, "ok v"+version+"\n")
	case p == "/agent.sh":
		m.handleAgentScript(w, r)
	case p == "/agent-uninstall.sh":
		m.handleAgentUninstall(w, r)
	case strings.HasPrefix(p, "/bin/"):
		m.handleBinary(w, r)
	case p == "/__ping":
		handlePing("master")(w, r)
	case p == "/dl":
		m.handleDownload(w, r)
	case p == "/api/targets":
		m.apiTargets(w, r)
	case p == "/api/access/status":
		m.apiAccessStatus(w, r)
	case p == "/api/access/login":
		m.apiAccessLogin(w, r)
	case p == "/api/access/logout":
		m.apiAccessLogout(w, r)
	case p == "/api/admin/records":
		m.apiRecords(w, r)
	case p == "/api/admin/health/check":
		m.apiHealthCheck(w, r)
	case p == "/api/login":
		m.apiLogin(w, r)
	case p == "/api/logout":
		m.apiLogout(w, r)
	case p == "/api/admin/session":
		m.apiSession(w, r)
	case p == "/api/admin/stats":
		m.apiStats(w, r)
	case p == "/api/admin/config":
		m.apiConfig(w, r)
	case p == "/api/admin/nodes":
		m.apiNodes(w, r)
	case strings.HasPrefix(p, "/api/admin/nodes/"):
		m.apiNodeItem(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (m *Master) servePage(w http.ResponseWriter, name string) {
	b, err := webFS.ReadFile("web/" + name)
	if err != nil {
		http.Error(w, "页面资源缺失: "+name, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(b)
}

// ---- 主服务器自身也是一个下载节点 ----

func (m *Master) handleDownload(w http.ResponseWriter, r *http.Request) {
	cfg := m.store.Get()
	token := r.URL.Query().Get("t")
	if token == "" {
		http.Error(w, "缺少下载令牌 (t)", http.StatusForbidden)
		return
	}
	p, err := verifyToken(cfg.MasterSecret, token)
	if err != nil {
		http.Error(w, "令牌无效: "+err.Error(), http.StatusForbidden)
		return
	}
	streamDownload(w, r, p.URL, cfg.AllowPrivate)
}

// ---- 工具 ----

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": msg})
}

// requestBase 推断主服务器对外可访问的地址, 让内置节点的下载链接可用.
func requestBase(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if v := r.Header.Get("X-Forwarded-Proto"); v != "" {
		scheme = strings.TrimSpace(strings.Split(v, ",")[0])
	}
	host := r.Host
	if v := r.Header.Get("X-Forwarded-Host"); v != "" {
		host = strings.TrimSpace(strings.Split(v, ",")[0])
	}
	return scheme + "://" + host
}

// ---- 用户端接口 ----

type targetNode struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Region  string `json:"region,omitempty"`
	Remark  string `json:"remark,omitempty"`
	Base    string `json:"base"`
	Link    string `json:"link"`
	Builtin bool   `json:"builtin"`
}

// apiTargets 接收一个下载地址, 返回每个节点各自签名的下载直链.
// 主服务器只为每个节点签发令牌, 流量不经过主服务器 (浏览器直连节点).
func (m *Master) apiTargets(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, "只支持 POST")
		return
	}
	// 开启访问验证后, 未通过验证的请求拿不到任何节点信息(也就拿不到节点地址)
	if !m.accessOK(r) {
		fail(w, http.StatusUnauthorized, "需要验证后才能使用")
		return
	}
	var body struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		fail(w, http.StatusBadRequest, "请求体解析失败")
		return
	}
	target := strings.TrimSpace(body.URL)
	if target == "" {
		fail(w, http.StatusBadRequest, "请填写下载地址")
		return
	}
	cfg := m.store.Get()
	if _, err := validateTarget(target, cfg.AllowPrivate); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}

	exp := time.Now().Add(time.Duration(cfg.TokenTTL) * time.Second).Unix()
	nodes := make([]targetNode, 0, 8)

	// 内置节点: 主服务器自身
	base := requestBase(r)
	if tok, err := signToken(cfg.MasterSecret, TokenPayload{
		URL: target, Exp: exp, Node: "builtin", Priv: cfg.AllowPrivate,
	}); err == nil {
		nodes = append(nodes, targetNode{
			ID:      "builtin",
			Name:    "主服务器直连",
			Region:  cfg.MasterRegion,
			Base:    base,
			Link:    base + "/dl?t=" + tok,
			Builtin: true,
		})
	}

	// 从节点: 只保留当前在线的, 离线的直接不出现在用户页面上
	for _, n := range m.store.EnabledNodes() {
		b := strings.TrimRight(strings.TrimSpace(n.BaseURL), "/")
		if b == "" || !m.health.IsOnline(n.ID) {
			continue
		}
		tok, err := signToken(n.Secret, TokenPayload{
			URL: target, Exp: exp, Node: n.ID, Priv: cfg.AllowPrivate,
		})
		if err != nil {
			continue
		}
		nodes = append(nodes, targetNode{
			ID:     n.ID,
			Name:   n.Name,
			Region: n.Region,
			Remark: n.Remark,
			Base:   b,
			Link:   b + "/dl?t=" + tok,
		})
	}

	// 记录本次解析 (只有元数据, 不含任何下载内容)
	if cfg.RetainDays() != 0 {
		m.records.Add(Record{
			Time:   nowUnix(),
			IP:     clientIP(r),
			Target: target,
			Nodes:  len(nodes),
			UA:     truncate(r.UserAgent(), 200),
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"url":        target,
		"expires_in": cfg.TokenTTL,
		"nodes":      nodes,
	})
}

// ---- 管理面板: 登录 ----

func (m *Master) apiLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, "只支持 POST")
		return
	}
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		fail(w, http.StatusBadRequest, "请求体解析失败")
		return
	}
	cfg := m.store.Get()
	okUser := subtle.ConstantTimeCompare([]byte(body.Username), []byte(cfg.AdminUser)) == 1
	okPass := subtle.ConstantTimeCompare([]byte(body.Password), []byte(cfg.AdminPass)) == 1
	if !okUser || !okPass {
		time.Sleep(300 * time.Millisecond)
		fail(w, http.StatusUnauthorized, "用户名或密码错误")
		return
	}
	tok := makeSession(cfg.SessionKey, cfg.AdminUser, sessionTTL)
	http.SetCookie(w, &http.Cookie{
		Name:     "dlt_session",
		Value:    tok,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionTTL.Seconds()),
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "user": cfg.AdminUser})
}

func (m *Master) apiLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: "dlt_session", Value: "", Path: "/", HttpOnly: true, MaxAge: -1,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (m *Master) apiSession(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie("dlt_session")
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"authed": false})
		return
	}
	user, ok := checkSession(m.store.Get().SessionKey, c.Value)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"authed": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"authed": true, "user": user})
}

func (m *Master) requireAuth(w http.ResponseWriter, r *http.Request) bool {
	c, err := r.Cookie("dlt_session")
	if err != nil {
		fail(w, http.StatusUnauthorized, "未登录")
		return false
	}
	if _, ok := checkSession(m.store.Get().SessionKey, c.Value); !ok {
		fail(w, http.StatusUnauthorized, "登录已失效, 请重新登录")
		return false
	}
	return true
}

// ---- 用户页面的访问验证 (可选开启) ----
//
// 打开 require_login 后, 用户页面必须先输口令才能解析下载地址。
// 目的: 不把节点地址暴露给随便路过的人, 降低节点被扫出来打的风险。
// 口令没单独设置时回退用管理员密码; 管理员已登录管理面板的话直接放行。

// accessSubject 是访客会话的主体标识。
// 里面掺了访问口令的 HMAC —— 这样管理员改了口令之后, 之前发出去的会话立刻失效,
// 不用轮换 session_key。用 HMAC 而不是裸哈希是为了不让 cookie 里泄露口令摘要。
func accessSubject(cfg Config) string {
	return "guest." + hex.EncodeToString(hmacSum(cfg.SessionKey, "access|"+cfg.accessPassword()))[:16]
}

// accessOK 判断当前请求是否已通过用户页面验证 (未开启验证时永远为真)。
func (m *Master) accessOK(r *http.Request) bool {
	cfg := m.store.Get()
	if !cfg.RequireLogin {
		return true
	}
	// 管理员登录了管理面板, 就不用再输一次用户页口令
	if c, err := r.Cookie("dlt_session"); err == nil {
		if _, ok := checkSession(cfg.SessionKey, c.Value); ok {
			return true
		}
	}
	c, err := r.Cookie(accessCookie)
	if err != nil {
		return false
	}
	sub, ok := checkSession(cfg.SessionKey, c.Value)
	return ok && sub == accessSubject(cfg)
}

// apiAccessStatus 给前端判断: 要不要显示验证页, 以及当前是否已通过。
func (m *Master) apiAccessStatus(w http.ResponseWriter, r *http.Request) {
	cfg := m.store.Get()
	writeJSON(w, http.StatusOK, map[string]any{
		"required": cfg.RequireLogin,
		"ok":       m.accessOK(r),
	})
}

// apiAccessLogin 校验访问口令, 通过则下发 dlt_access cookie。
func (m *Master) apiAccessLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, "只支持 POST")
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		fail(w, http.StatusBadRequest, "请求体解析失败")
		return
	}
	cfg := m.store.Get()
	if !cfg.RequireLogin {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	if subtle.ConstantTimeCompare([]byte(body.Password), []byte(cfg.accessPassword())) != 1 {
		time.Sleep(400 * time.Millisecond) // 简单的爆破减速
		fail(w, http.StatusUnauthorized, "口令不正确")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     accessCookie,
		Value:    makeSession(cfg.SessionKey, accessSubject(cfg), accessTTL),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(accessTTL.Seconds()),
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// apiAccessLogout 清掉访客 cookie。
func (m *Master) apiAccessLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: accessCookie, Value: "", Path: "/", HttpOnly: true, MaxAge: -1,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}


// ---- 管理面板: 节点 ----

func (m *Master) apiNodes(w http.ResponseWriter, r *http.Request) {
	if !m.requireAuth(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		cfg := m.store.Get()
		nodes := cfg.Nodes
		if nodes == nil {
			nodes = []Node{} // 返回空数组而非 null, 方便前端直接遍历
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"nodes":  nodes,
			"health": m.health.Snapshot(),
		})
	case http.MethodPost:
		// enabled 用指针接收: 不传时默认启用, 避免"加完节点却不出现在用户页面"
		var in struct {
			Name    string `json:"name"`
			BaseURL string `json:"base_url"`
			Secret  string `json:"secret"`
			Region  string `json:"region"`
			Remark  string `json:"remark"`
			Enabled *bool  `json:"enabled"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&in); err != nil {
			fail(w, http.StatusBadRequest, "请求体解析失败")
			return
		}
		n := Node{
			Name:    strings.TrimSpace(in.Name),
			BaseURL: normalizeBase(in.BaseURL),
			Secret:  strings.TrimSpace(in.Secret),
			Region:  strings.ToUpper(strings.TrimSpace(in.Region)),
			Remark:  strings.TrimSpace(in.Remark),
			Enabled: in.Enabled == nil || *in.Enabled,
		}
		if n.Name == "" {
			fail(w, http.StatusBadRequest, "节点名称不能为空")
			return
		}
		// 地址允许留空: 表示从服务器还没部署好, 该节点暂不参与分发

		if n.Secret == "" {
			n.Secret = randHex(24)
		}
		n.ID = newID()
		n.CreatedAt = nowUnix()
		n.Builtin = false
		if err := m.store.Update(func(c *Config) error {
			c.Nodes = append(c.Nodes, n)
			return nil
		}); err != nil {
			fail(w, http.StatusInternalServerError, "保存失败: "+err.Error())
			return
		}
		m.health.CheckNow() // 新节点立刻探一次, 让面板尽快显示状态
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "node": n})
	default:
		fail(w, http.StatusMethodNotAllowed, "不支持的方法")
	}
}

func (m *Master) apiNodeItem(w http.ResponseWriter, r *http.Request) {
	if !m.requireAuth(w, r) {
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/admin/nodes/"), "/")
	parts := strings.Split(rest, "/")
	id := parts[0]
	if id == "" {
		http.NotFound(w, r)
		return
	}
	if len(parts) == 2 && parts[1] == "test" {
		m.apiNodeTest(w, r, id)
		return
	}

	switch r.Method {
	case http.MethodPut:
		// 用指针接收可空字段: 只更新传过来的字段, 没传的保持原值。
		// 之前 enabled 是普通 bool 且无条件赋值, 导致"只想改地区"的部分更新
		// 会把节点静默停用 (enabled 缺省 false)。region / remark 同理。
		var in struct {
			Name    string  `json:"name"`
			BaseURL string  `json:"base_url"`
			Secret  string  `json:"secret"`
			Region  *string `json:"region"`
			Remark  *string `json:"remark"`
			Enabled *bool   `json:"enabled"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&in); err != nil {
			fail(w, http.StatusBadRequest, "请求体解析失败")
			return
		}
		found := false
		err := m.store.Update(func(c *Config) error {
			for i := range c.Nodes {
				if c.Nodes[i].ID != id {
					continue
				}
				found = true
				if strings.TrimSpace(in.Name) != "" {
					c.Nodes[i].Name = strings.TrimSpace(in.Name)
				}
				if strings.TrimSpace(in.BaseURL) != "" {
					c.Nodes[i].BaseURL = normalizeBase(in.BaseURL)
				}
				if in.Secret != "" {
					c.Nodes[i].Secret = in.Secret
				}
				if in.Region != nil {
					c.Nodes[i].Region = strings.ToUpper(strings.TrimSpace(*in.Region))
				}
				if in.Remark != nil {
					c.Nodes[i].Remark = strings.TrimSpace(*in.Remark)
				}
				if in.Enabled != nil {
					c.Nodes[i].Enabled = *in.Enabled
				}
			}
			return nil
		})
		if err != nil {
			fail(w, http.StatusInternalServerError, "保存失败: "+err.Error())
			return
		}
		if !found {
			fail(w, http.StatusNotFound, "节点不存在")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})

	case http.MethodDelete:
		found := false
		err := m.store.Update(func(c *Config) error {
			out := c.Nodes[:0]
			for _, n := range c.Nodes {
				if n.ID == id {
					found = true
					continue
				}
				out = append(out, n)
			}
			c.Nodes = out
			return nil
		})
		if err != nil {
			fail(w, http.StatusInternalServerError, "删除失败: "+err.Error())
			return
		}
		if !found {
			fail(w, http.StatusNotFound, "节点不存在")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})

	default:
		fail(w, http.StatusMethodNotAllowed, "不支持的方法")
	}
}

// apiNodeTest 从主服务器侧探测节点延迟 (用户侧延迟由前端浏览器实测).
func (m *Master) apiNodeTest(w http.ResponseWriter, r *http.Request, id string) {
	if id == "builtin" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "latency_ms": 0, "note": "主服务器本机"})
		return
	}
	n, ok := m.store.FindNode(id)
	if !ok {
		fail(w, http.StatusNotFound, "节点不存在")
		return
	}
	base := strings.TrimRight(strings.TrimSpace(n.BaseURL), "/")
	client := &http.Client{Timeout: 8 * time.Second}
	best := int64(-1)
	lastErr := "无法连接"
	for i := 0; i < 3; i++ {
		t0 := time.Now()
		resp, err := client.Get(base + "/__ping")
		if err != nil {
			lastErr = err.Error()
			continue
		}
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			lastErr = "节点返回 HTTP " + strconv.Itoa(resp.StatusCode)
			continue
		}
		d := time.Since(t0).Milliseconds()
		if best < 0 || d < best {
			best = d
		}
	}
	if best < 0 {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": lastErr})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "latency_ms": best})
}

// ---- 管理面板: 全局配置 / 统计 ----

func (m *Master) apiStats(w http.ResponseWriter, r *http.Request) {
	if !m.requireAuth(w, r) {
		return
	}
	cfg := m.store.Get()
	s := gStats.snapshot()

	online := 1 // 内置节点(主服务器自身)始终在线
	for _, n := range cfg.Nodes {
		if n.Enabled && m.health.IsOnline(n.ID) {
			online++
		}
	}
	recTotal, recToday := m.records.Stats()

	writeJSON(w, http.StatusOK, map[string]any{
		"stats":         s,
		"nodes_total":   len(cfg.Nodes) + 1,
		"nodes_online":  online,
		"records_total": recTotal,
		"records_today": recToday,
		"version":       version,
	})
}

func (m *Master) apiConfig(w http.ResponseWriter, r *http.Request) {
	if !m.requireAuth(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		cfg := m.store.Get()
		writeJSON(w, http.StatusOK, map[string]any{
			"admin_user":         cfg.AdminUser,
			"token_ttl":          cfg.TokenTTL,
			"allow_private":      cfg.AllowPrivate,
			"master_secret":      cfg.MasterSecret,
			"master_region":      cfg.MasterRegion,
			"record_retain_days": cfg.RetainDays(),
			"require_login":      cfg.RequireLogin,
			// 只回"有没有单独设口令", 不回口令本身
			"has_access_password": strings.TrimSpace(cfg.AccessPassword) != "",
		})
	case http.MethodPut:
		var in struct {
			TokenTTL         *int    `json:"token_ttl"`
			AllowPrivate     *bool   `json:"allow_private"`
			NewPassword      string  `json:"new_password"`
			NewUsername      string  `json:"new_username"`
			MasterSecret     *string `json:"master_secret"`
			MasterRegion     *string `json:"master_region"`
			RecordRetainDays *int    `json:"record_retain_days"`
			RequireLogin     *bool   `json:"require_login"`
			// nil = 不改动; "" = 清空(回退用管理员密码); 其它 = 设为该值
			AccessPassword *string `json:"access_password"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&in); err != nil {
			fail(w, http.StatusBadRequest, "请求体解析失败")
			return
		}
		err := m.store.Update(func(c *Config) error {
			if in.TokenTTL != nil {
				if *in.TokenTTL < 60 {
					*in.TokenTTL = 60
				}
				if *in.TokenTTL > 30*86400 {
					*in.TokenTTL = 30 * 86400
				}
				c.TokenTTL = *in.TokenTTL
			}
			if in.AllowPrivate != nil {
				c.AllowPrivate = *in.AllowPrivate
			}
			if strings.TrimSpace(in.NewUsername) != "" {
				c.AdminUser = strings.TrimSpace(in.NewUsername)
			}
			if strings.TrimSpace(in.NewPassword) != "" {
				c.AdminPass = strings.TrimSpace(in.NewPassword)
			}
			if in.MasterSecret != nil && strings.TrimSpace(*in.MasterSecret) != "" {
				c.MasterSecret = strings.TrimSpace(*in.MasterSecret)
			}
			if in.MasterRegion != nil {
				c.MasterRegion = strings.ToUpper(strings.TrimSpace(*in.MasterRegion))
			}
			if in.RecordRetainDays != nil {
				v := *in.RecordRetainDays
				if v > 3650 {
					v = 3650
				}
				if v < -1 {
					v = -1
				}
				c.RecordRetainDays = &v
			}
			if in.RequireLogin != nil {
				c.RequireLogin = *in.RequireLogin
			}
			if in.AccessPassword != nil {
				p := strings.TrimSpace(*in.AccessPassword)
				if p != "" && len(p) < 4 {
					return errAccessPasswordTooShort
				}
				c.AccessPassword = p
			}
			return nil
		})
		if err == errAccessPasswordTooShort {
			fail(w, http.StatusBadRequest, "访问口令至少 4 位")
			return
		}
		if err != nil {
			fail(w, http.StatusInternalServerError, "保存失败: "+err.Error())
			return
		}
		m.pruneRecords() // 改完保留策略立刻生效
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		fail(w, http.StatusMethodNotAllowed, "不支持的方法")
	}
}

// apiHealthCheck 立即同步探测所有节点并返回最新状态, 供面板"刷新状态"用.
func (m *Master) apiHealthCheck(w http.ResponseWriter, r *http.Request) {
	if !m.requireAuth(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, "只支持 POST")
		return
	}
	m.health.CheckAllSync()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"health": m.health.Snapshot(),
	})
}

// ---- 管理面板: 下载记录 ----

func (m *Master) apiRecords(w http.ResponseWriter, r *http.Request) {
	if !m.requireAuth(w, r) {
		return
	}

	switch r.Method {
	case http.MethodGet:
		q := r.URL.Query()
		from := parseDayStart(q.Get("from"))
		to := parseDayEnd(q.Get("to"))
		limit := atoiDefault(q.Get("limit"), 100)
		offset := atoiDefault(q.Get("offset"), 0)
		if limit <= 0 || limit > 1000 {
			limit = 100
		}
		if offset < 0 {
			offset = 0
		}

		items, total := m.records.Query(from, to, q.Get("q"), limit, offset)
		writeJSON(w, http.StatusOK, map[string]any{
			"records": items,
			"total":   total,
			"limit":   limit,
			"offset":  offset,
		})

	case http.MethodDelete:
		n := m.records.Clear()
		log.Printf("下载记录已清空 (%d 条) by %s", n, clientIP(r))
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "removed": n})

	default:
		fail(w, http.StatusMethodNotAllowed, "不支持的方法")
	}
}

// parseDayStart 把 YYYY-MM-DD 解析成当天 00:00:00 的 unix 秒; 空值/非法返回 0.
func parseDayStart(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	t, err := time.ParseInLocation("2006-01-02", s, time.Local)
	if err != nil {
		return 0
	}
	return t.Unix()
}

// parseDayEnd 把 YYYY-MM-DD 解析成当天 23:59:59 的 unix 秒; 空值/非法返回 0.
func parseDayEnd(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	t, err := time.ParseInLocation("2006-01-02", s, time.Local)
	if err != nil {
		return 0
	}
	return t.Add(24*time.Hour - time.Second).Unix()
}

func atoiDefault(s string, def int) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return def
	}
	return n
}

// clientIP 取真实客户端 IP, 兼容反向代理场景.
func clientIP(r *http.Request) string {
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		return strings.TrimSpace(strings.Split(v, ",")[0])
	}
	if v := r.Header.Get("X-Real-IP"); v != "" {
		return strings.TrimSpace(v)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func normalizeBase(s string) string {
	s = strings.TrimRight(strings.TrimSpace(s), "/")
	if s == "" {
		return ""
	}
	if !strings.HasPrefix(s, "http://") && !strings.HasPrefix(s, "https://") {
		s = "http://" + s
	}
	return s
}
