#!/usr/bin/env bash
#
# dltunnel 主服务器一键卸载
#
#   curl -fsSL https://raw.githubusercontent.com/whooc/dltunnel/main/uninstall.sh | bash
#
# 可选参数:
#   --dir <目录>   安装目录 (默认 /opt/dltunnel, 需与安装时一致)
#   --purge        连配置与下载记录一起删除 (默认保留)
#   -h, --help     显示帮助
#
# 默认只移除服务与程序, 保留 $DIR/data (管理密码、节点配置、下载记录),
# 想彻底清干净请加 --purge。
#
set -e

DIR="/opt/dltunnel"
SVC="dltunnel"
PURGE=0

usage() {
  cat <<'EOF'
用法: uninstall.sh [选项]

  --dir <目录>   安装目录 (默认 /opt/dltunnel)
  --purge        连配置与下载记录一起删除 (默认保留)
  -h, --help     显示本帮助

示例:
  # 只卸载程序, 保留配置和数据
  curl -fsSL https://raw.githubusercontent.com/whooc/dltunnel/main/uninstall.sh | bash

  # 彻底清除 (含配置与下载记录)
  curl -fsSL https://raw.githubusercontent.com/whooc/dltunnel/main/uninstall.sh | bash -s -- --purge
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    --dir)     DIR="$2"; shift 2 ;;
    --purge)   PURGE=1;  shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "未知参数: $1"; usage; exit 1 ;;
  esac
done

if [ "$(id -u)" != "0" ]; then
  echo "请用 root 运行 (或 sudo bash)"
  exit 1
fi

# —— 安全护栏: 拒绝明显危险的目录, 避免误删系统路径 ——
DIR="$(readlink -f "$DIR" 2>/dev/null || echo "$DIR")"
case "$DIR" in
  ""|"/"|"/usr"|"/usr/"*|"/etc"|"/etc/"*|"/bin"|"/sbin"|"/lib"|"/lib64"| \
  "/boot"|"/dev"|"/proc"|"/sys"|"/var"|"/var/"*|"/opt"|"/root"|"/home"|"/home/"*)
    echo "拒绝执行: 安装目录 $DIR 看起来是系统路径。"
    echo "请用 --dir 指定真实的 dltunnel 安装目录。"
    exit 1
    ;;
esac

echo "即将卸载 dltunnel 主服务器"
echo "  安装目录 : $DIR"
echo "  服务名   : $SVC"
if [ "$PURGE" = 1 ]; then
  echo "  数据处理 : 一并删除配置与下载记录 (--purge)"
else
  echo "  数据处理 : 保留 $DIR/data (配置与下载记录)"
fi
echo ""

# 1. 停止并移除 systemd 服务
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

# 2. 兜底: 杀掉可能残留的进程
if pgrep -f "$DIR/dltunnel" >/dev/null 2>&1; then
  echo "==> 结束残留进程"
  pkill -f "$DIR/dltunnel" 2>/dev/null || true
  sleep 1
  pkill -9 -f "$DIR/dltunnel" 2>/dev/null || true
fi

# 3. 删除文件
if [ ! -d "$DIR" ]; then
  echo "==> 安装目录不存在, 跳过: $DIR"
elif [ "$PURGE" = 1 ]; then
  echo "==> 删除整个安装目录"
  rm -rf "$DIR"
else
  echo "==> 删除程序文件 (保留 data/)"
  rm -f  "$DIR/dltunnel" "$DIR/dltunnel.new"
  rm -rf "$DIR/bin"
  # 兜底清掉可能存在的临时上传文件
  rm -f "$DIR"/*.new 2>/dev/null || true
fi

# 4. 结果
echo ""
echo "============================================"
echo " dltunnel 主服务器已卸载"
echo "============================================"
if [ "$PURGE" != 1 ] && [ -d "$DIR/data" ]; then
  echo " 配置与下载记录仍保留在:"
  echo "   $DIR/data"
  echo ""
  echo " 确认不再需要后, 可手动删除:"
  echo "   rm -rf $DIR"
  echo " 或重新运行本脚本并加 --purge"
fi
echo ""
echo " 端口 20808 已释放。"
echo " 各从节点不受影响; 如需一并移除, 请在从服务器上执行从节点卸载命令。"
echo "============================================"
