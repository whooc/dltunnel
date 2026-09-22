package main

import (
	"log"
	"net/http"
	"os"
	"time"
)

// runAgent 启动一个从节点: 只有两个对外接口
//
//	GET /__ping  延迟探测 (无鉴权, 只返回节点名)
//	GET /dl?t=.. 流式中转下载 (HMAC 令牌鉴权)
func runAgent(listen, name, secret string) {
	if name == "" {
		if h, err := os.Hostname(); err == nil {
			name = h
		} else {
			name = "agent"
		}
	}

	mux := http.NewServeMux()
	mux.Handle("/__ping", handlePing(name))
	mux.HandleFunc("/dl", func(w http.ResponseWriter, r *http.Request) {
		token := r.URL.Query().Get("t")
		if token == "" {
			http.Error(w, "缺少下载令牌 (t)", http.StatusForbidden)
			return
		}
		p, err := verifyToken(secret, token)
		if err != nil {
			log.Printf("令牌校验失败 from=%s err=%v", r.RemoteAddr, err)
			http.Error(w, "令牌无效: "+err.Error(), http.StatusForbidden)
			return
		}
		streamDownload(w, r, p.URL, p.Priv)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("dltunnel agent [" + name + "] v" + version + " ok\n"))
	})

	srv := &http.Server{
		Addr:              listen,
		Handler:           mux,
		ReadHeaderTimeout: 20 * time.Second,
		IdleTimeout:       120 * time.Second,
		// 刻意不设置 WriteTimeout / ReadTimeout, 否则大文件长下载会被强制切断
	}
	log.Printf("从节点已启动 监听=%s 名称=%s", listen, name)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("从节点退出: %v", err)
	}
}
