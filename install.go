package main

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// 允许对外提供的二进制文件名白名单 (防止路径穿越).
var allowedBinaries = map[string]bool{
	"dltunnel-linux-amd64": true,
	"dltunnel-linux-arm64": true,
}

const agentScriptTmpl = `#!/usr/bin/env bash
# dltunnel 从节点一键安装脚本 (由主服务器动态生成)
set -e

SERVER="__SERVER__"
SECRET="__SECRET__"
NODE_NAME="__NAME__"
PORT="__PORT__"
# 刻意与主服务器的 /opt/dltunnel 分开, 避免同机部署时互相覆盖二进制
DIR="/opt/dltunnel-agent"
SVC="dltunnel-agent"

if [ "$(id -u)" != "0" ]; then
  echo "请用 root 运行 (或 sudo bash)"; exit 1
fi

if [ -z "$SECRET" ]; then
  echo "缺少节点密钥。请到主服务器管理面板「添加从节点」重新复制命令。"; exit 1
fi

if ! command -v curl >/dev/null 2>&1; then
  echo "需要 curl，请先安装: apt install -y curl"; exit 1
fi

ARCH="$(uname -m)"
case "$ARCH" in
  x86_64|amd64) BIN="dltunnel-linux-amd64" ;;
  aarch64|arm64) BIN="dltunnel-linux-arm64" ;;
  *) echo "不支持的架构: $ARCH"; exit 1 ;;
esac

if command -v ss >/dev/null 2>&1 && ss -lnt 2>/dev/null | grep -q ":$PORT "; then
  echo "警告: 端口 $PORT 已被占用，安装后可能无法启动。"
  echo "      可用 --port 指定其它端口重新生成脚本。"
fi

echo "==> 架构 $ARCH -> $BIN"
echo "==> 从主服务器下载二进制"
mkdir -p "$DIR"
curl -fsSL "$SERVER/bin/$BIN" -o "$DIR/dltunnel.new"
chmod 0755 "$DIR/dltunnel.new"
mv -f "$DIR/dltunnel.new" "$DIR/dltunnel"

echo "==> 写入 systemd 服务"
cat > "/etc/systemd/system/$SVC.service" <<UNIT
[Unit]
Description=dltunnel agent - relay download node
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=$DIR
ExecStart=$DIR/dltunnel -mode agent -listen :$PORT -name "$NODE_NAME" -secret "$SECRET"
Restart=always
RestartSec=3
LimitNOFILE=1048576
Environment=GOMEMLIMIT=128MiB
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
UNIT

systemctl daemon-reload
systemctl enable "$SVC" >/dev/null 2>&1
systemctl restart "$SVC"
sleep 2

if systemctl is-active --quiet "$SVC"; then
  echo ""
  echo "=========================================="
  echo " 从节点安装完成"
  echo " 服务名   : $SVC"
  echo " 节点名称 : $NODE_NAME"
  echo " 监听端口 : $PORT"
  echo "=========================================="
  echo ""
  IP="$(curl -fsSL -m 5 https://api.ipify.org 2>/dev/null || echo '<本机公网IP>')"
  echo "请回到主服务器管理面板，把该节点的地址填成:"
  echo "    http://$IP:$PORT"
  echo ""
  echo "面板里点「测试」出现绿色延迟数字即接通。"
  echo "查看日志: journalctl -u $SVC -f"
else
  echo "服务启动失败，最近日志："
  journalctl -u "$SVC" --no-pager -n 20
  exit 1
fi
`

// handleAgentScript 动态生成从节点安装脚本.
// 脚本内嵌主服务器地址, 从节点直接从主服务器拉二进制 —— 不依赖 GitHub, 也不依赖外网。
func (m *Master) handleAgentScript(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	secret := strings.TrimSpace(q.Get("secret"))
	name := strings.TrimSpace(q.Get("name"))
	port := strings.TrimSpace(q.Get("port"))
	if port == "" {
		port = "20809"
	}
	if name == "" {
		name = "node"
	}

	body := agentScriptTmpl
	body = strings.ReplaceAll(body, "__SERVER__", requestBase(r))
	body = strings.ReplaceAll(body, "__SECRET__", secret)
	body = strings.ReplaceAll(body, "__NAME__", name)
	body = strings.ReplaceAll(body, "__PORT__", port)

	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	io.WriteString(w, body)
}

// agentUninstallScript 从节点一键卸载脚本.
// 从节点安装位置固定, 不含任何密钥, 所以静态生成即可, 无需模板替换.
const agentUninstallScript = `#!/usr/bin/env bash
# dltunnel 从节点一键卸载 (由主服务器提供)
set -e

DIR="/opt/dltunnel-agent"
SVC="dltunnel-agent"

if [ "$(id -u)" != "0" ]; then
  echo "请用 root 运行 (或 sudo bash)"; exit 1
fi

echo "即将卸载 dltunnel 从节点"
echo "  安装目录 : $DIR"
echo "  服务名   : $SVC"
echo ""

if [ -f "/etc/systemd/system/$SVC.service" ]; then
  echo "==> 停止并移除 systemd 服务"
  systemctl stop "$SVC" 2>/dev/null || true
  systemctl disable "$SVC" >/dev/null 2>&1 || true
  rm -f "/etc/systemd/system/$SVC.service"
  systemctl daemon-reload
  systemctl reset-failed "$SVC" 2>/dev/null || true
else
  echo "==> 未发现 $SVC.service (可能不是用 systemd 安装的)"
fi

if pgrep -f "$DIR/dltunnel" >/dev/null 2>&1; then
  echo "==> 结束残留进程"
  pkill -f "$DIR/dltunnel" 2>/dev/null || true
  sleep 1
  pkill -9 -f "$DIR/dltunnel" 2>/dev/null || true
fi

# 安全护栏: 目录名不符预期时不动它
if [ "$DIR" = "/opt/dltunnel-agent" ] && [ -d "$DIR" ]; then
  echo "==> 删除安装目录"
  rm -rf "$DIR"
else
  echo "==> 安装目录不存在, 跳过: $DIR"
fi

echo ""
echo "=========================================="
echo " 从节点已卸载"
echo "=========================================="
echo " 请回到主服务器管理面板, 在「节点」页删除该节点的记录。"
echo "=========================================="
`

// handleAgentUninstall 输出从节点卸载脚本.
func (m *Master) handleAgentUninstall(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	io.WriteString(w, agentUninstallScript)
}

// handleBinary 对外提供各架构的二进制, 供从节点安装脚本下载.
func (m *Master) handleBinary(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/bin/")
	if !allowedBinaries[name] {
		http.NotFound(w, r)
		return
	}
	p := filepath.Join(m.binDir, name)
	f, err := os.Open(p)
	if err != nil {
		http.Error(w, "该架构的二进制未部署到本服务器 (缺少 "+name+")", http.StatusNotFound)
		return
	}
	defer f.Close()

	if st, err := f.Stat(); err == nil {
		w.Header().Set("Content-Length", strconv.FormatInt(st.Size(), 10))
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	io.Copy(w, f)
}
