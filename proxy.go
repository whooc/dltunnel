package main

import (
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"
)

// 上游连接池: 主从共用. 不落盘, 纯流式.
var upstreamTransport = &http.Transport{
	Proxy: http.ProxyFromEnvironment,
	DialContext: (&net.Dialer{
		Timeout:   15 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext,
	ForceAttemptHTTP2:     true,
	MaxIdleConns:          200,
	MaxIdleConnsPerHost:   32,
	IdleConnTimeout:       90 * time.Second,
	TLSHandshakeTimeout:   15 * time.Second,
	ExpectContinueTimeout: 2 * time.Second,
	ResponseHeaderTimeout: 45 * time.Second,
	DisableCompression:    true, // 原样透传文件字节, 不做透明压缩
}

// 不设总超时: 大文件下载可能持续很久. 只靠上下文取消.
var upstreamClient = &http.Client{Transport: upstreamTransport}

// 允许透传给浏览器的上游响应头
var passRespHeaders = map[string]bool{
	"content-type":        true,
	"content-length":      true,
	"content-range":       true,
	"accept-ranges":       true,
	"content-disposition": true,
	"last-modified":       true,
	"etag":                true,
	"cache-control":       true,
	"expires":             true,
}

const upstreamUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/122.0.0.0 Safari/537.36"

// flushWriter 每写一块就 flush, 保证流式效果 (进度条能实时动).
type flushWriter struct {
	w io.Writer
	f http.Flusher
}

func (fw flushWriter) Write(p []byte) (int, error) {
	n, err := fw.w.Write(p)
	if fw.f != nil {
		fw.f.Flush()
	}
	return n, err
}

// streamDownload 拉取上游并流式转发给客户端, 全程不落盘.
func streamDownload(w http.ResponseWriter, r *http.Request, target string, allowPrivate bool) {
	u, err := validateTarget(target, allowPrivate)
	if err != nil {
		http.Error(w, "目标地址不可用: "+err.Error(), http.StatusBadRequest)
		return
	}

	method := r.Method
	if method != http.MethodHead {
		method = http.MethodGet
	}
	req, err := http.NewRequestWithContext(r.Context(), method, u.String(), nil)
	if err != nil {
		http.Error(w, "构建上游请求失败", http.StatusInternalServerError)
		return
	}
	// 透传断点续传 / 缓存相关头, 保证多线程下载器和续传可用
	for _, h := range []string{"Range", "If-Range", "If-None-Match", "If-Modified-Since"} {
		if v := r.Header.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	req.Header.Set("User-Agent", upstreamUA)
	req.Header.Set("Accept", "*/*")

	gStats.begin()
	start := time.Now()
	resp, err := upstreamClient.Do(req)
	if err != nil {
		gStats.end(0, true)
		log.Printf("上游请求失败 target=%s err=%v", u.String(), err)
		http.Error(w, "上游请求失败: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	dst := w.Header()
	for k, vv := range resp.Header {
		if passRespHeaders[strings.ToLower(k)] {
			for _, v := range vv {
				dst.Add(k, v)
			}
		}
	}
	dst.Set("X-Dltunnel", "stream")
	dst.Set("Access-Control-Allow-Origin", "*")
	dst.Set("Access-Control-Expose-Headers", "Content-Length,Content-Range,Content-Disposition")

	w.WriteHeader(resp.StatusCode)
	if method == http.MethodHead {
		gStats.end(0, false)
		return
	}

	fw := flushWriter{w: w}
	if f, ok := w.(http.Flusher); ok {
		fw.f = f
	}
	buf := make([]byte, 64*1024)
	n, copyErr := io.CopyBuffer(fw, resp.Body, buf)
	gStats.end(n, copyErr != nil)
	log.Printf("中转完成 %s -> %s  status=%d bytes=%d 耗时=%s err=%v",
		r.RemoteAddr, u.Host, resp.StatusCode, n, time.Since(start).Round(time.Millisecond), copyErr)
}

// handlePing 供前端/主服务器测延迟, 无需鉴权, 返回极小的 JSON.
func handlePing(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"ok":true,"name":%q,"version":%q,"ts":%d}`,
			name, version, time.Now().UnixMilli())
	}
}
