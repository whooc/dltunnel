// dltunnel - 网页中转下载器
//
// 主服务器 (master): 提供前端页面 + 管理面板 + 自身作为一个下载节点
// 从服务器 (agent) : 只做流式中转下载, 由主服务器签发的 HMAC 令牌授权
//
// 两者都不落盘, 纯流式转发 (io.Copy), 不占用磁盘空间.
package main

import (
	"flag"
	"log"
)

const version = "1.1.0"

func main() {
	var (
		mode    = flag.String("mode", "master", "运行模式: master | agent")
		listen  = flag.String("listen", "", "监听地址 (master 默认 :20808, agent 默认 :20809)")
		dataDir = flag.String("data", "./data", "数据目录 (仅 master 使用)")
		binDir  = flag.String("bindir", "", "各架构二进制存放目录 (仅 master, 供从节点一键安装下载)")
		name    = flag.String("name", "", "节点名称 (仅 agent 使用)")
		secret  = flag.String("secret", "", "节点密钥 (仅 agent 使用, 必填)")
	)
	flag.Parse()

	log.SetFlags(log.LstdFlags)

	switch *mode {
	case "master":
		addr := *listen
		if addr == "" {
			addr = ":20808"
		}
		runMaster(addr, *dataDir, *binDir)
	case "agent":
		addr := *listen
		if addr == "" {
			addr = ":20809"
		}
		if *secret == "" {
			log.Fatal("agent 模式必须通过 --secret 指定节点密钥")
		}
		runAgent(addr, *name, *secret)
	default:
		log.Fatalf("未知运行模式: %q (只支持 master 或 agent)", *mode)
	}
}
