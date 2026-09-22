#!/usr/bin/env python3
"""dltunnel 1.1 新功能测试.

覆盖:
  - 节点健康检查 (可达/不可达) 与"离线节点不出现在用户页面"
  - 下载记录 (写入/日期筛选/关键词/清空/保留期=0 时不记录)
  - 从节点一键安装脚本 /agent.sh 与二进制端点 /bin/
  - 全局配置新增项 (master_region / record_retain_days)
"""
import hashlib
import http.server
import json
import os
import shutil
import socket
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.request

ROOT = os.path.dirname(os.path.abspath(__file__))
EXE = os.path.join(ROOT, "dist", "dltunnel.exe")
DIST = os.path.join(ROOT, "dist")
MASTER = "http://127.0.0.1:18080"
AGENT = "http://127.0.0.1:18081"
DEAD = "http://127.0.0.1:18099"          # 没有任何服务在听
SRC_PORT = 18090
DATADIR = os.path.join(ROOT, "smoke3")

SRC_DATA = bytes((i * 40503) & 0xFF for i in range(256 * 1024))
TARGET = "http://127.0.0.1:%d/blob.bin" % SRC_PORT

PASS, FAIL = [], []


def check(name, cond, extra=""):
    (PASS if cond else FAIL).append(name)
    mark = "[OK]  " if cond else "[FAIL]"
    tail = "" if cond or extra == "" else "  -> " + str(extra)
    print("  %s %s%s" % (mark, name, tail))


def call(url, method="GET", body=None, cookie=None, timeout=60):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=data, method=method)
    if data is not None:
        req.add_header("Content-Type", "application/json")
    if cookie:
        req.add_header("Cookie", cookie)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, dict(r.headers), r.read()
    except urllib.error.HTTPError as e:
        return e.code, dict(e.headers), e.read()
    except Exception as e:
        return 0, {}, str(e).encode()


def wait_port(port, timeout=10):
    end = time.time() + timeout
    while time.time() < end:
        try:
            with socket.create_connection(("127.0.0.1", port), 0.4):
                return True
        except OSError:
            time.sleep(0.15)
    return False


class SrcHandler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def do_GET(self):
        self.send_response(200)
        self.send_header("Content-Length", str(len(SRC_DATA)))
        self.send_header("Content-Type", "application/octet-stream")
        self.end_headers()
        self.wfile.write(SRC_DATA)

    def log_message(self, *a):
        pass


def main():
    if os.path.isdir(DATADIR):
        shutil.rmtree(DATADIR, ignore_errors=True)

    srv = http.server.ThreadingHTTPServer(("127.0.0.1", SRC_PORT), SrcHandler)
    threading.Thread(target=srv.serve_forever, daemon=True).start()

    env = dict(os.environ)
    env["NO_PROXY"] = "127.0.0.1,localhost"
    env["no_proxy"] = "127.0.0.1,localhost"

    print("== 启动主服务器 ==")
    mlog = open(os.path.join(ROOT, "smoke3_master.log"), "w+", encoding="utf-8")
    master = subprocess.Popen([EXE, "-mode", "master", "-listen", "127.0.0.1:18080",
                               "-data", DATADIR, "-bindir", DIST],
                              stdout=mlog, stderr=subprocess.STDOUT, env=env)
    agent = None
    try:
        if not wait_port(18080):
            print("主服务器未能启动")
            return 1
        cfg = json.load(open(os.path.join(DATADIR, "config.json"), encoding="utf-8"))
        user, pwd = cfg["admin_user"], cfg["admin_pass"]

        st, h, b = call(MASTER + "/api/login", "POST", {"username": user, "password": pwd})
        cookie = h.get("Set-Cookie", "").split(";")[0]
        check("登录成功", st == 200, b[:100])

        # 打开内网下载, 便于用本地源做测试
        call(MASTER + "/api/admin/config", "PUT", {"allow_private": True}, cookie=cookie)

        print("\n== 全局配置新增项 ==")
        st, _, b = call(MASTER + "/api/admin/config", "PUT",
                        {"master_region": "hk", "record_retain_days": 30}, cookie=cookie)
        check("保存 master_region / record_retain_days", st == 200, b[:100])
        st, _, b = call(MASTER + "/api/admin/config", cookie=cookie)
        c = json.loads(b)
        check("master_region 被规范化为大写", c.get("master_region") == "HK", c.get("master_region"))
        check("record_retain_days 保存成功", c.get("record_retain_days") == 30, c.get("record_retain_days"))

        print("\n== 主服务器节点带地区标识 ==")
        st, _, b = call(MASTER + "/api/targets", "POST", {"url": TARGET})
        d = json.loads(b)
        check("内置节点 region = HK", d["nodes"][0].get("region") == "HK", d["nodes"][0])

        print("\n== 下载记录 ==")
        st, _, b = call(MASTER + "/api/admin/records", cookie=cookie)
        r = json.loads(b)
        check("解析后产生记录", r["total"] >= 1, r["total"])
        rec = r["records"][0]
        check("记录含目标地址", rec.get("u") == TARGET, rec.get("u"))
        check("记录含来源 IP", bool(rec.get("ip")), rec.get("ip"))
        check("记录含时间戳", rec.get("t", 0) > 0, rec.get("t"))

        today = time.strftime("%Y-%m-%d")
        st, _, b = call(MASTER + "/api/admin/records?from=%s&to=%s" % (today, today), cookie=cookie)
        check("按今天筛选能查到", json.loads(b)["total"] >= 1, json.loads(b)["total"])

        st, _, b = call(MASTER + "/api/admin/records?from=2000-01-01&to=2000-01-02", cookie=cookie)
        check("按历史日期筛选查不到", json.loads(b)["total"] == 0, json.loads(b)["total"])

        st, _, b = call(MASTER + "/api/admin/records?q=blob.bin", cookie=cookie)
        check("关键词命中", json.loads(b)["total"] >= 1)
        st, _, b = call(MASTER + "/api/admin/records?q=zzz-not-exist", cookie=cookie)
        check("关键词未命中", json.loads(b)["total"] == 0)

        st, _, b = call(MASTER + "/api/admin/records?limit=1&offset=0", cookie=cookie)
        r = json.loads(b)
        check("分页 limit 生效", len(r["records"]) == 1 and r["limit"] == 1, len(r["records"]))

        print("\n== 记录保留期 = 0 时不记录 ==")
        call(MASTER + "/api/admin/config", "PUT", {"record_retain_days": 0}, cookie=cookie)
        before = json.loads(call(MASTER + "/api/admin/records", cookie=cookie)[2])["total"]
        call(MASTER + "/api/targets", "POST", {"url": TARGET + "?x=1"})
        after = json.loads(call(MASTER + "/api/admin/records", cookie=cookie)[2])["total"]
        check("关闭记录后不再新增 (历史保留)", after == before and before >= 1,
              "before=%d after=%d" % (before, after))
        call(MASTER + "/api/admin/config", "PUT", {"record_retain_days": 30}, cookie=cookie)

        print("\n== 节点健康检查 ==")
        alog = open(os.path.join(ROOT, "smoke3_agent.log"), "w+", encoding="utf-8")
        agent = subprocess.Popen([EXE, "-mode", "agent", "-listen", "127.0.0.1:18081",
                                  "-name", "可达节点", "-secret", "s-alive"],
                                 stdout=alog, stderr=subprocess.STDOUT, env=env)
        check("agent 就绪", wait_port(18081))

        st, _, b = call(MASTER + "/api/admin/nodes", "POST",
                        {"name": "可达节点", "base_url": AGENT, "region": "JP",
                         "secret": "s-alive", "enabled": True}, cookie=cookie)
        alive_id = json.loads(b)["node"]["id"]

        st, _, b = call(MASTER + "/api/admin/nodes", "POST",
                        {"name": "离线节点", "base_url": DEAD, "region": "US",
                         "secret": "s-dead", "enabled": True}, cookie=cookie)
        dead_id = json.loads(b)["node"]["id"]

        # 不传 enabled 时应默认启用: 面板之外的调用(脚本/curl)常省略该字段
        st, _, b = call(MASTER + "/api/admin/nodes", "POST",
                        {"name": "默认启用节点", "base_url": AGENT, "region": "jp",
                         "secret": "s-default"}, cookie=cookie)
        dnode = json.loads(b)["node"]
        check("不传 enabled 时默认启用", dnode.get("enabled") is True, dnode.get("enabled"))
        check("地区代码自动转大写", dnode.get("region") == "JP", dnode.get("region"))
        call(MASTER + "/api/admin/nodes/" + dnode["id"], "DELETE", cookie=cookie)

        st, _, b = call(MASTER + "/api/admin/health/check", "POST", cookie=cookie)
        check("手动触发探测返回健康数据", st == 200 and "health" in json.loads(b), b[:120])
        st, _, b = call(MASTER + "/api/admin/health/check", "POST", cookie=cookie)
        health = json.loads(b)["health"]

        check("可达节点被判定在线", health.get(alive_id, {}).get("online") is True,
              health.get(alive_id))
        check("可达节点有延迟数据", health.get(alive_id, {}).get("latency_ms", -1) >= 0,
              health.get(alive_id))
        check("不可达节点被判定离线", health.get(dead_id, {}).get("online") is False,
              health.get(dead_id))

        st, _, b = call(MASTER + "/api/targets", "POST", {"url": TARGET})
        d = json.loads(b)
        ids = [n["id"] for n in d["nodes"]]
        check("用户页面只返回在线节点 (2 个)", len(ids) == 2, ids)
        check("离线节点不出现在用户页面", dead_id not in ids, ids)
        check("在线从节点正常出现", alive_id in ids, ids)

        st, _, b = call(MASTER + "/api/admin/stats", cookie=cookie)
        s = json.loads(b)
        check("统计的在线节点数正确", s["nodes_online"] == 2, s["nodes_online"])
        check("统计的节点总数正确", s["nodes_total"] == 3, s["nodes_total"])

        print("\n== 停用节点后立即隐藏 ==")
        call(MASTER + "/api/admin/nodes/" + alive_id, "PUT",
             {"name": "可达节点", "base_url": AGENT, "region": "JP",
              "secret": "s-alive", "enabled": False}, cookie=cookie)
        st, _, b = call(MASTER + "/api/targets", "POST", {"url": TARGET})
        check("停用后用户页面只剩主服务器", len(json.loads(b)["nodes"]) == 1,
              len(json.loads(b)["nodes"]))

        print("\n== 一键安装脚本 /agent.sh ==")
        st, h, b = call(MASTER + "/agent.sh?secret=abc123&name=HK-01&port=20809")
        txt = b.decode("utf-8", "replace")
        check("返回脚本且状态 200", st == 200, st)
        check("Content-Type 是 shellscript", "shellscript" in h.get("Content-Type", ""),
              h.get("Content-Type"))
        check("脚本含 shebang", txt.startswith("#!/usr/bin/env bash"), txt[:30])
        check("脚本内嵌主服务器地址", 'SERVER="http://127.0.0.1:18080"' in txt)
        check("脚本从主服务器拉二进制", '"$SERVER/bin/$BIN"' in txt)
        check("脚本内嵌密钥", 'SECRET="abc123"' in txt)
        check("脚本内嵌节点名", 'NODE_NAME="HK-01"' in txt)
        check("脚本内嵌端口", 'PORT="20809"' in txt)
        check("脚本会写 systemd 服务", 'SVC="dltunnel-agent"' in txt and "/etc/systemd/system/$SVC.service" in txt)
        check("脚本安装目录与主服务器隔离",
              'DIR="/opt/dltunnel-agent"' in txt and 'DIR="/opt/dltunnel"' not in txt,
              "避免同机部署覆盖主服务器二进制")
        check("脚本含架构分支", "dltunnel-linux-arm64" in txt and "dltunnel-linux-amd64" in txt)
        check("脚本做 root 检查", "id -u" in txt)
        check("脚本会打印面板回填地址", "管理面板" in txt)

        st, _, b = call(MASTER + "/agent.sh")
        check("无参数也返回脚本(由脚本自身报错)", st == 200 and b.startswith(b"#!/usr/bin/env bash"))

        print("\n== 从节点一键卸载脚本 /agent-uninstall.sh ==")
        st, h, b = call(MASTER + "/agent-uninstall.sh")
        u = b.decode("utf-8", "replace")
        check("返回脚本且状态 200", st == 200, st)
        check("Content-Type 是 shellscript", "shellscript" in h.get("Content-Type", ""),
              h.get("Content-Type"))
        check("脚本含 shebang", u.startswith("#!/usr/bin/env bash"), u[:30])
        check("卸载目录与安装一致", 'DIR="/opt/dltunnel-agent"' in u)
        check("服务名一致", 'SVC="dltunnel-agent"' in u)
        check("会停服务", "systemctl stop" in u)
        check("会禁用开机自启", "systemctl disable" in u)
        check("会删除 systemd unit", "rm -f \"/etc/systemd/system/$SVC.service\"" in u)
        check("会 reload systemd", "systemctl daemon-reload" in u)
        check("会兜底杀残留进程", "pkill" in u)
        check("会删除安装目录", 'rm -rf "$DIR"' in u)
        check("含 root 检查", "id -u" in u)
        check("不含任何密钥", "SECRET" not in u and "secret" not in u)
        check("提示回面板删除节点记录", "删除该节点" in u)

        print("\n== 主服务器卸载脚本 uninstall.sh ==")
        up = os.path.join(ROOT, "uninstall.sh")
        check("文件存在", os.path.isfile(up), up)
        if os.path.isfile(up):
            raw = open(up, "rb").read()
            t = raw.decode("utf-8", "replace")
            check("行尾为 LF", b"\r\n" not in raw)
            check("含 shebang", t.startswith("#!/usr/bin/env bash"))
            check("默认目录 /opt/dltunnel", 'DIR="/opt/dltunnel"' in t)
            check("服务名 dltunnel", 'SVC="dltunnel"' in t)
            check("支持 --purge", "--purge" in t)
            check("支持 --dir", "--dir" in t)
            check("默认保留 data 目录", "保留" in t and "rm -rf \"$DIR\"" in t)
            check("含系统路径护栏", "拒绝执行" in t)
            check("含 root 检查", "id -u" in t)

        print("\n== 二进制端点 /bin/ ==")
        st, h, b = call(MASTER + "/bin/dltunnel-linux-amd64", timeout=120)
        size = os.path.getsize(os.path.join(DIST, "dltunnel-linux-amd64"))
        check("下载 linux-amd64 二进制", st == 200 and len(b) == size,
              "st=%s len=%d expect=%d" % (st, len(b), size))
        check("二进制内容 md5 与本地一致",
              hashlib.md5(b).hexdigest() == hashlib.md5(
                  open(os.path.join(DIST, "dltunnel-linux-amd64"), "rb").read()).hexdigest())

        st, _, b = call(MASTER + "/bin/dltunnel-linux-arm64", timeout=120)
        check("下载 linux-arm64 二进制", st == 200, st)

        st, _, b = call(MASTER + "/bin/../../etc/passwd")
        check("路径穿越被拒绝", st == 404, st)
        st, _, b = call(MASTER + "/bin/not-exist-file")
        check("非白名单文件被拒绝", st == 404, st)

        print("\n== 清空记录 ==")
        call(MASTER + "/api/targets", "POST", {"url": TARGET})
        st, _, b = call(MASTER + "/api/admin/records", "DELETE", cookie=cookie)
        check("清空记录成功", st == 200 and json.loads(b)["removed"] >= 1, b[:100])
        st, _, b = call(MASTER + "/api/admin/records", cookie=cookie)
        check("清空后记录为 0", json.loads(b)["total"] == 0, json.loads(b)["total"])

        print("\n== 未登录保护 ==")
        st, _, b = call(MASTER + "/api/admin/records")
        check("记录接口需要登录", st == 401, st)
        st, _, b = call(MASTER + "/api/admin/health/check", "POST")
        check("探测接口需要登录", st == 401, st)

    finally:
        for p in (agent, master):
            if p and p.poll() is None:
                p.terminate()
                try:
                    p.wait(timeout=5)
                except Exception:
                    p.kill()
        mlog.close()
        srv.shutdown()

    print("\n" + "=" * 56)
    print("通过 %d 项, 失败 %d 项" % (len(PASS), len(FAIL)))
    for f in FAIL:
        print("  失败: " + f)
    return 1 if FAIL else 0


if __name__ == "__main__":
    sys.exit(main())
