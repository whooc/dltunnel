#!/usr/bin/env bash
#
# dltunnel 主服务器一键安装脚本
#
#   curl -fsSL https://raw.githubusercontent.com/whooc/dltunnel/main/install.sh | bash
#
# 可选参数:
#   --port 20808      服务监听端口 (默认 20808)
#   --dir  /opt/dltunnel   安装目录
#   --version v1.1.0  指定版本 (默认最新 release)
#
set -e

REPO="whooc/dltunnel"
PORT=20808
DIR="/opt/dltunnel"
SVC="dltunnel"
VER="latest"

usage() {
  cat <<'EOF'
用法: install.sh [选项]

  --port <端口>      服务监听端口 (默认 20808)
  --dir  <目录>      安装目录 (默认 /opt/dltunnel)
  --version <版本>   指定版本 tag, 如 v1.1.0 (默认取最新 release)
  -h, --help         显示本帮助
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    --port)    PORT="$2"; shift 2 ;;
    --dir)     DIR="$2";  shift 2 ;;
    --version) VER="$2";  shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "未知参数: $1"; usage; exit 1 ;;
  esac
done

if [ "$(id -u)" != "0" ]; then
  echo "请用 root 运行 (或 sudo bash)"
  exit 1
fi

if ! command -v curl >/dev/null 2>&1; then
  echo "需要 curl，请先安装: apt install -y curl  或  yum install -y curl"
  exit 1
fi

case "$(uname -m)" in
  x86_64|amd64)   BIN="dltunnel-linux-amd64" ;;
  aarch64|arm64)  BIN="dltunnel-linux-arm64" ;;
  *) echo "不支持的架构: $(uname -m)"; exit 1 ;;
esac

if [ "$VER" = "latest" ]; then
  BASE="https://github.com/$REPO/releases/latest/download"
else
  BASE="https://github.com/$REPO/releases/download/$VER"
fi

echo "==> 架构 $(uname -m) -> $BIN"
mkdir -p "$DIR/data" "$DIR/bin"

echo "==> 下载主程序"
if ! curl -fL --progress-bar "$BASE/$BIN" -o "$DIR/dltunnel.new"; then
  echo ""
  echo "下载失败。如果这台服务器访问 GitHub 有困难，可以："
  echo "  1) 在能上网的机器上下载 $BIN"
  echo "  2) 上传到 $DIR/dltunnel 并 chmod +x"
  echo "  3) 重新运行本脚本"
  exit 1
fi
chmod 0755 "$DIR/dltunnel.new"
mv -f "$DIR/dltunnel.new" "$DIR/dltunnel"

# 各架构二进制放一份到 bin/, 供从节点一键安装脚本下载
echo "==> 准备从节点安装用的二进制"
cp -f "$DIR/dltunnel" "$DIR/bin/$BIN"
for OTHER in dltunnel-linux-amd64 dltunnel-linux-arm64; do
  [ "$OTHER" = "$BIN" ] && continue
  curl -fL -s "$BASE/$OTHER" -o "$DIR/bin/$OTHER" 2>/dev/null && chmod 0755 "$DIR/bin/$OTHER" \
    || rm -f "$DIR/bin/$OTHER"
done
ls -1 "$DIR/bin" 2>/dev/null | sed 's/^/    /'

echo "==> 写入 systemd 服务"
cat > "/etc/systemd/system/$SVC.service" <<UNIT
[Unit]
Description=dltunnel master - web relay downloader
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=$DIR
ExecStart=$DIR/dltunnel -mode master -listen :$PORT -data $DIR/data -bindir $DIR/bin
Restart=always
RestartSec=3
LimitNOFILE=1048576
Environment=GOMEMLIMIT=256MiB
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
UNIT

systemctl daemon-reload
systemctl enable "$SVC" >/dev/null 2>&1
systemctl restart "$SVC"
sleep 2

if ! systemctl is-active --quiet "$SVC"; then
  echo ""
  echo "服务启动失败，最近日志："
  journalctl -u "$SVC" --no-pager -n 25
  exit 1
fi

IP="$(curl -fsSL -m 5 https://api.ipify.org 2>/dev/null || echo '<本机公网IP>')"

echo ""
echo "============================================"
echo " dltunnel 主服务器安装完成"
echo "============================================"
echo " 用户页面 : http://$IP:$PORT/"
echo " 管理面板 : http://$IP:$PORT/admin"
echo " 安装目录 : $DIR"
echo " 服务名   : $SVC"
echo "============================================"
echo ""
PWLINES="$(journalctl -u "$SVC" --no-pager -n 60 2>/dev/null | grep "管理面板登录" | tail -1 | sed 's/^.*dltunnel.*: //')"
if [ -n "$PWLINES" ]; then
  echo "管理面板初始密码："
  echo "    $PWLINES"
else
  echo "管理面板密码："
  echo "    本次是覆盖安装，密码沿用原有配置（未重置）。"
  echo "    忘记密码可查看: cat $DIR/data/config.json"
  echo "    首次安装的密码在日志里: journalctl -u $SVC | grep 管理面板登录"
fi
echo ""
echo "下一步：登录管理面板 → 添加从节点 → 复制生成的命令到其它服务器执行。"
echo "查看日志: journalctl -u $SVC -f"
