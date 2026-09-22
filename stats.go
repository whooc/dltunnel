package main

import "sync"

// Stats 进程内运行统计 (重启清零, 不落盘).
type Stats struct {
	mu      sync.Mutex
	Total   int64 // 累计中转次数
	Bytes   int64 // 累计中转字节
	Active  int64 // 当前进行中的中转数
	Failed  int64 // 失败次数
	Started int64 // 进程启动时间
}

var gStats = &Stats{}

func (s *Stats) begin() {
	s.mu.Lock()
	s.Total++
	s.Active++
	s.mu.Unlock()
}

func (s *Stats) end(bytes int64, failed bool) {
	s.mu.Lock()
	s.Active--
	s.Bytes += bytes
	if failed {
		s.Failed++
	}
	s.mu.Unlock()
}

func (s *Stats) snapshot() map[string]int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return map[string]int64{
		"total":   s.Total,
		"bytes":   s.Bytes,
		"active":  s.Active,
		"failed":  s.Failed,
		"started": s.Started,
	}
}
