package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// maxRecords 内存中最多保留的记录条数, 防止长期运行把内存撑爆.
const maxRecords = 200000

// Record 一次"解析下载地址"的审计记录.
// 只记元数据, 不含任何文件内容 —— 与"不落盘"的定位不冲突.
type Record struct {
	Time   int64  `json:"t"`           // unix 秒
	IP     string `json:"ip"`          // 请求方 IP
	Target string `json:"u"`           // 用户填写的下载地址
	Nodes  int    `json:"n"`           // 当时可用节点数
	UA     string `json:"ua,omitempty"`
}

// RecordStore 用 JSONL 落盘 + 内存索引, 支持按日期范围查询.
type RecordStore struct {
	mu    sync.RWMutex
	path  string
	items []Record
	f     *os.File
}

func loadRecords(dir string) (*RecordStore, error) {
	rs := &RecordStore{path: filepath.Join(dir, "records.jsonl")}

	f, err := os.Open(rs.path)
	if err == nil {
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			var r Record
			if json.Unmarshal([]byte(line), &r) == nil && r.Time > 0 {
				rs.items = append(rs.items, r)
			}
		}
		f.Close()
		if len(rs.items) > maxRecords {
			rs.items = append([]Record(nil), rs.items[len(rs.items)-maxRecords:]...)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}

	rs.f, _ = os.OpenFile(rs.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	return rs, nil
}

// Add 追加一条记录. 内存超限时整体重写文件.
func (rs *RecordStore) Add(r Record) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.items = append(rs.items, r)

	if len(rs.items) > maxRecords {
		rs.items = append([]Record(nil), rs.items[len(rs.items)-maxRecords:]...)
		rs.rewriteLocked()
		return
	}
	if rs.f != nil {
		if b, err := json.Marshal(r); err == nil {
			rs.f.Write(append(b, '\n'))
		}
	}
}

func (rs *RecordStore) rewriteLocked() {
	if rs.f != nil {
		rs.f.Close()
		rs.f = nil
	}
	tmp := rs.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err == nil {
		w := bufio.NewWriter(f)
		for _, r := range rs.items {
			if b, err := json.Marshal(r); err == nil {
				w.Write(append(b, '\n'))
			}
		}
		w.Flush()
		f.Close()
		os.Rename(tmp, rs.path)
	}
	rs.f, _ = os.OpenFile(rs.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
}

// Prune 按保留天数清理. days<0 表示永久保留; days==0 表示清空且不再记录.
// 返回被删除的条数.
func (rs *RecordStore) Prune(days int) int {
	if days < 0 {
		return 0
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()

	if days == 0 {
		n := len(rs.items)
		if n == 0 {
			return 0
		}
		rs.items = nil
		rs.rewriteLocked()
		return n
	}

	cutoff := time.Now().AddDate(0, 0, -days).Unix()
	kept := rs.items[:0]
	removed := 0
	for _, r := range rs.items {
		if r.Time >= cutoff {
			kept = append(kept, r)
		} else {
			removed++
		}
	}
	if removed > 0 {
		rs.items = kept
		rs.rewriteLocked()
	}
	return removed
}

// Query 按时间范围倒序返回, 并返回符合条件的总数(用于分页).
func (rs *RecordStore) Query(fromUnix, toUnix int64, keyword string, limit, offset int) ([]Record, int) {
	rs.mu.RLock()
	defer rs.mu.RUnlock()

	kw := strings.ToLower(strings.TrimSpace(keyword))
	matched := make([]Record, 0, 64)
	for i := len(rs.items) - 1; i >= 0; i-- {
		r := rs.items[i]
		if fromUnix > 0 && r.Time < fromUnix {
			continue
		}
		if toUnix > 0 && r.Time > toUnix {
			continue
		}
		if kw != "" && !strings.Contains(strings.ToLower(r.Target), kw) &&
			!strings.Contains(strings.ToLower(r.IP), kw) {
			continue
		}
		matched = append(matched, r)
	}

	total := len(matched)
	if offset < 0 {
		offset = 0
	}
	if offset >= total {
		return []Record{}, total
	}
	matched = matched[offset:]
	if limit > 0 && len(matched) > limit {
		matched = matched[:limit]
	}
	return matched, total
}

// Clear 清空全部记录.
func (rs *RecordStore) Clear() int {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	n := len(rs.items)
	rs.items = nil
	rs.rewriteLocked()
	return n
}

// Stats 返回记录总量与今日量.
func (rs *RecordStore) Stats() (total int, today int) {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	total = len(rs.items)
	y, m, d := time.Now().Date()
	cut := time.Date(y, m, d, 0, 0, 0, 0, time.Local).Unix()
	for i := len(rs.items) - 1; i >= 0; i-- {
		if rs.items[i].Time >= cut {
			today++
		} else {
			break
		}
	}
	return
}

// Close 关闭文件句柄.
func (rs *RecordStore) Close() {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.f != nil {
		rs.f.Close()
		rs.f = nil
	}
}
