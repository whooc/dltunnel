package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Node 一个可用的下载节点.
type Node struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	BaseURL   string `json:"base_url"` // 如 http://1.2.3.4:20809, 内置节点留空(运行时推断)
	Secret    string `json:"secret"`
	Region    string `json:"region,omitempty"`
	Remark    string `json:"remark,omitempty"`
	Enabled   bool   `json:"enabled"`
	Builtin   bool   `json:"builtin,omitempty"` // 主服务器自身, 不可删除
	CreatedAt int64  `json:"created_at"`
}

// Config 主服务器的持久化配置 (只有几 KB, 与"不保存下载数据"无关).
type Config struct {
	AdminUser    string `json:"admin_user"`
	AdminPass    string `json:"admin_pass"`
	SessionKey   string `json:"session_key"`
	MasterSecret string `json:"master_secret"` // 主服务器自身节点的密钥
	TokenTTL     int    `json:"token_ttl"`     // 下载令牌有效期(秒)
	AllowPrivate bool   `json:"allow_private"` // 是否允许下载内网地址
	MasterRegion string `json:"master_region,omitempty"` // 主服务器自身节点展示的地区代码(如 US)

	// RecordRetainDays 下载记录保留天数.
	// nil = 老配置未设置(按 30 天), 0 = 不记录, >0 = 保留这么多天, <0 = 永久.
	RecordRetainDays *int `json:"record_retain_days,omitempty"`

	// RequireLogin 打开后, 用户页面必须先通过口令验证才能解析下载地址.
	// 默认关闭 —— 老配置文件里没这个字段时反序列化成 false, 行为不变。
	RequireLogin bool `json:"require_login,omitempty"`

	// AccessPassword 是用户页面的访问口令.
	// 留空则回退用管理员密码(方便不想多记一个密码的人)。
	AccessPassword string `json:"access_password,omitempty"`

	Nodes []Node `json:"nodes"`
}

// RetainDays 返回生效的下载记录保留天数.
func (c Config) RetainDays() int {
	if c.RecordRetainDays == nil {
		return 30
	}
	return *c.RecordRetainDays
}

// accessPassword 返回用户页面实际生效的访问口令: 没单独设就用管理员密码.
func (c Config) accessPassword() string {
	if p := strings.TrimSpace(c.AccessPassword); p != "" {
		return p
	}
	return c.AdminPass
}

type Store struct {
	mu   sync.RWMutex
	path string
	cfg  Config
}

func loadStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	p := filepath.Join(dir, "config.json")
	s := &Store{path: p}

	raw, err := os.ReadFile(p)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
		s.cfg = Config{
			AdminUser:    "admin",
			AdminPass:    randHex(5),
			SessionKey:   randHex(32),
			MasterSecret: randHex(24),
			TokenTTL:     3600,
		}
		if err := s.save(); err != nil {
			return nil, err
		}
		log.Printf("已生成初始配置: %s", p)
		log.Printf(">>> 管理面板登录:  用户名 %s   密码 %s", s.cfg.AdminUser, s.cfg.AdminPass)
		return s, nil
	}

	if err := json.Unmarshal(raw, &s.cfg); err != nil {
		return nil, err
	}
	changed := false
	if s.cfg.SessionKey == "" {
		s.cfg.SessionKey = randHex(32)
		changed = true
	}
	if s.cfg.MasterSecret == "" {
		s.cfg.MasterSecret = randHex(24)
		changed = true
	}
	if s.cfg.TokenTTL <= 0 {
		s.cfg.TokenTTL = 3600
		changed = true
	}
	if changed {
		if err := s.save(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *Store) save() error {
	b, err := json.MarshalIndent(s.cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *Store) Get() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

func (s *Store) Update(fn func(*Config) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := fn(&s.cfg); err != nil {
		return err
	}
	return s.save()
}

func (s *Store) FindNode(id string) (Node, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, n := range s.cfg.Nodes {
		if n.ID == id {
			return n, true
		}
	}
	return Node{}, false
}

// EnabledNodes 返回所有启用的从节点 (不含内置节点).
func (s *Store) EnabledNodes() []Node {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Node, 0, len(s.cfg.Nodes))
	for _, n := range s.cfg.Nodes {
		if n.Enabled {
			out = append(out, n)
		}
	}
	return out
}

func nowUnix() int64 { return time.Now().Unix() }
