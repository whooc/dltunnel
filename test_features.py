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
import platform
import re
import shutil
import socket
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.parse
import urllib.request

# Windows 控制台默认不是 UTF-8 (cp936/cp1252), 直接 print 中文会 UnicodeEncodeError.
# CI 的 windows-latest 上就踩过这个坑, 这里统一把输出流切成 UTF-8.
for _s in (sys.stdout, sys.stderr):
    try:
        _s.reconfigure(encoding="utf-8", errors="replace")
    except Exception:
        pass

ROOT = os.path.dirname(os.path.abspath(__file__))


def _find_exe():
    """定位被测二进制: 优先 DL_BIN 环境变量, 否则按平台推断 (CI 跑在 Linux 上)."""
    env = os.environ.get("DL_BIN")
    if env:
        return os.path.abspath(env)
    if os.name == "nt":
        return os.path.join(ROOT, "dist", "dltunnel.exe")
    arch = "arm64" if platform.machine().lower() in ("aarch64", "arm64") else "amd64"
    return os.path.join(ROOT, "dist", "dltunnel-linux-" + arch)


def _find_bash():
    """定位可用的 bash, 用于验证安装脚本的参数解析逻辑 (找不到就跳过相关用例)."""
    cands = ["bash"]
    if os.name == "nt":
        cands += [r"C:\Program Files\Git\bin\bash.exe",
                  r"C:\Program Files\Git\usr\bin\bash.exe",
                  r"C:\Program Files (x86)\Git\bin\bash.exe"]
    for c in cands:
        try:
            p = subprocess.run([c, "-c", "echo ok"], capture_output=True, timeout=20)
            if p.returncode == 0 and b"ok" in p.stdout:
                return c
        except Exception:
            continue
    return None


EXE = _find_exe()
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

        print("\n== 版本号展示 ==")
        st, _, b = call(MASTER + "/health")
        txt = b.decode("utf-8", "replace").strip()
        check("/health 格式为 'ok vX.Y.Z'", st == 200 and re.match(r"^ok v\d+\.\d+", txt), txt)
        check("版本号没有重复 v 前缀", "vv" not in txt, txt)
        st, _, b = call(AGENT + "/")
        atxt = b.decode("utf-8", "replace").strip()
        check("agent 横幅没有重复 v 前缀", "vv" not in atxt, atxt)

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

        print("\n== 节点部分更新不会误伤其它字段 ==")
        # 曾经 PUT 用普通 bool 无条件覆盖 enabled, "只想改个地区"的部分更新
        # 会把节点静默停用 —— 对面板之外的调用方(curl/脚本)是个很隐蔽的坑。
        call(MASTER + "/api/admin/nodes/" + alive_id, "PUT",
             {"region": "HK"}, cookie=cookie)
        st, _, b = call(MASTER + "/api/admin/nodes", cookie=cookie)
        node = [n for n in json.loads(b)["nodes"] if n["id"] == alive_id][0]
        check("只传 region 时 enabled 保持不变", node["enabled"] is False, node["enabled"])
        check("只传 region 时 region 已更新", node["region"] == "HK", node["region"])
        check("只传 region 时 name 不变", node["name"] == "可达节点", node["name"])
        check("只传 region 时 base_url 不变", node["base_url"] == AGENT, node["base_url"])

        call(MASTER + "/api/admin/nodes/" + alive_id, "PUT",
             {"enabled": True}, cookie=cookie)
        st, _, b = call(MASTER + "/api/admin/nodes", cookie=cookie)
        node = [n for n in json.loads(b)["nodes"] if n["id"] == alive_id][0]
        check("只传 enabled 时节点被启用", node["enabled"] is True, node["enabled"])
        check("只传 enabled 时 region 不变", node["region"] == "HK", node["region"])
        check("只传 enabled 时 name 不变", node["name"] == "可达节点", node["name"])
        check("只传 enabled 时 base_url 不变", node["base_url"] == AGENT, node["base_url"])

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

        # ---- 面板生成的是 bash 参数形式: /agent.sh | bash -s -- --secret .. ----
        # 服务端必须支持解析这些参数, 否则脚本里的密钥是空的, 装不上。
        check("脚本支持 --secret/--name/--port 参数", "while [ $# -gt 0 ]" in txt
              and "--secret)" in txt and "--name)" in txt and "--port)" in txt)
        check("脚本支持 --help", "--help)" in txt and "用法" in txt)
        check("脚本校验端口为纯数字", "*[!0-9]*" in txt)
        check("systemd unit 权限收紧(含密钥)", "chmod 0600" in txt)

        BASH = _find_bash()
        if not BASH:
            print("  [skip] 找不到 bash, 跳过参数解析的功能验证")
        else:
            print("\n== /agent.sh 参数解析功能验证 (截取参数解析段单独跑) ==")
            head = txt.split('if [ "$(id -u)"')[0]
            probe = head + '\necho "PARSED:$SECRET|$NODE_NAME|$PORT|$SERVER"\n'
            pf = os.path.join(ROOT, "agent_parse_probe.sh")
            with open(pf, "w", encoding="utf-8", newline="\n") as f:
                f.write(probe)

            def run_probe(args, script=None):
                path = pf
                if script is not None:
                    path = os.path.join(ROOT, "agent_parse_probe2.sh")
                    with open(path, "w", encoding="utf-8", newline="\n") as fh:
                        fh.write(script)
                p = subprocess.run([BASH, path] + args, capture_output=True, timeout=60)
                return (p.returncode, p.stdout.decode("utf-8", "replace").strip(),
                        p.stderr.decode("utf-8", "replace").strip())

            # 1) 面板形式: 参数放在 -- 之后, 应覆盖 query 里的默认值
            rc, out, err = run_probe(["--secret", "s3cr3t", "--name", "2THK", "--port", "20809"])
            check("bash 参数覆盖默认值", rc == 0 and
                  out == "PARSED:s3cr3t|2THK|20809|http://127.0.0.1:18080",
                  "rc=%s out=%s err=%s" % (rc, out, err[:120]))

            # 2) 不带参数: 沿用 query 里的默认值
            rc, out, err = run_probe([])
            check("不带参数时沿用 query 默认值", rc == 0 and
                  out == "PARSED:abc123|HK-01|20809|http://127.0.0.1:18080",
                  "rc=%s out=%s" % (rc, out))

            # 3) 用户实际遇到的场景: 无 query, 全靠参数
            st, _, b0 = call(MASTER + "/agent.sh")
            t0 = b0.decode("utf-8", "replace")
            head0 = t0.split('if [ "$(id -u)"')[0]
            probe0 = head0 + '\necho "PARSED:$SECRET|$NODE_NAME|$PORT|$SERVER"\n'
            rc, out, err = run_probe(
                ["--secret", "6fabd595a8d2d86056648c87155f762fb0d4e0740f337268",
                 "--name", "2THK", "--port", "20809"], script=probe0)
            check("无 query + 纯参数形式可用(用户报错的命令)", rc == 0 and out ==
                  "PARSED:6fabd595a8d2d86056648c87155f762fb0d4e0740f337268|2THK|20809|"
                  "http://127.0.0.1:18080", "rc=%s out=%s err=%s" % (rc, out, err[:120]))

            # 4) --help 退出码 0
            rc, out, err = run_probe(["--help"])
            check("--help 退出码 0 且打印用法", rc == 0 and "用法" in out, "rc=%s out=%s" % (rc, out[:80]))

            # 5) 参数缺取值要明确报错
            rc, out, err = run_probe(["--secret"])
            check("参数缺取值时报错退出", rc != 0 and "缺少取值" in (out + err),
                  "rc=%s out=%s" % (rc, out[:80]))

            # 6) 未知参数要报错, 不能静默忽略
            rc, out, err = run_probe(["--secrett", "x"])
            check("未知参数报错退出", rc != 0 and "未知参数" in (out + err),
                  "rc=%s out=%s" % (rc, out[:80]))

            # 7) 端口非数字要拦掉
            rc, out, err = run_probe(["--secret", "s", "--port", "abc"])
            check("非数字端口被拦下", rc != 0 and "纯数字" in (out + err),
                  "rc=%s out=%s" % (rc, out[:80]))

            # 8) 两种形式的脚本都要能过 bash 语法检查
            p = subprocess.run([BASH, "-n", pf], capture_output=True, timeout=60)
            check("query 形式脚本语法正确", p.returncode == 0,
                  p.stderr.decode("utf-8", "replace")[:160])
            p2 = os.path.join(ROOT, "agent_parse_probe2.sh")
            p = subprocess.run([BASH, "-n", p2], capture_output=True, timeout=60)
            check("无 query 形式脚本语法正确", p.returncode == 0,
                  p.stderr.decode("utf-8", "replace")[:160])
            for f in (pf, p2):
                try:
                    os.remove(f)
                except OSError:
                    pass

        print("\n== /agent.sh 嵌入值的净化 (query 里的危险字符) ==")
        bad_secret = 'a"; rm -rf /; echo "'
        bad_name = "x`id`y"
        st, _, b = call(MASTER + "/agent.sh?" + urllib.parse.urlencode(
            {"secret": bad_secret, "name": bad_name, "port": "20809"}))
        t2 = b.decode("utf-8", "replace")
        sl = [l for l in t2.splitlines() if l.startswith("SECRET=")]
        nl = [l for l in t2.splitlines() if l.startswith("NODE_NAME=")]
        check("query 值里的双引号被净化", sl and sl[0].count('"') == 2, sl[0] if sl else "(无 SECRET 行)")
        check("query 值里的反引号被净化", nl and "`" not in nl[0] and nl[0].count('"') == 2,
              nl[0] if nl else "(无 NODE_NAME 行)")
        check("净化后不会出现裸的分号命令注入",
              sl and 'rm -rf /' in sl[0] and sl[0].count('"') == 2,
              "分号留在引号内即安全")

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

        # ---- 用户页面访问验证 (设置里可开关) ----
        print("\n== 用户页面访问验证 ==")
        st, _, b = call(MASTER + "/api/access/status")
        d0 = json.loads(b)
        check("默认不要求验证", st == 200 and d0["required"] is False and d0["ok"] is True,
              b[:90])
        st, _, b = call(MASTER + "/api/targets", "POST", {"url": TARGET})
        check("默认免验证就能解析", st == 200, st)

        st, _, b = call(MASTER + "/api/admin/config", "PUT",
                        {"require_login": True}, cookie=cookie)
        check("开启访问验证", st == 200, b[:90])
        d1 = json.loads(call(MASTER + "/api/access/status")[2])
        check("状态变为需要验证", d1["required"] is True, d1)
        check("未验证时 status.ok 为 false", d1["ok"] is False, d1)

        st, _, b = call(MASTER + "/api/targets", "POST", {"url": TARGET})
        check("未验证时解析被拒 401", st == 401, st)
        check("拒绝原因可读", "需要验证" in b.decode("utf-8", "replace"), b[:80])

        st, _, b = call(MASTER + "/api/access/login", "POST", {"password": "wrong-pw"})
        check("错误口令被拒", st == 401, st)

        st, h, b = call(MASTER + "/api/access/login", "POST", {"password": pwd})
        check("口令留空时回退用管理员密码", st == 200, b[:90])
        sc = h.get("Set-Cookie", "")
        check("下发了 dlt_access cookie", "dlt_access=" in sc, sc[:70])
        check("验证 cookie 是 HttpOnly", "HttpOnly" in sc, sc[:90])
        check("验证 cookie 带 SameSite", "SameSite" in sc, sc[:130])
        acc = sc.split(";")[0]
        check("验证 cookie 与管理会话是不同名字", "dlt_session" not in acc, acc[:40])

        st, _, b = call(MASTER + "/api/targets", "POST", {"url": TARGET}, cookie=acc)
        check("带验证 cookie 可以解析", st == 200, st)
        st, _, b = call(MASTER + "/api/targets", "POST", {"url": TARGET}, cookie=cookie)
        check("管理员会话直接放行(不用再输一次)", st == 200, st)

        st, _, b = call(MASTER + "/api/admin/config", "PUT",
                        {"access_password": "guest-pw-123"}, cookie=cookie)
        check("设置自定义访问口令", st == 200, b[:90])
        st, _, b = call(MASTER + "/api/targets", "POST", {"url": TARGET}, cookie=acc)
        check("改口令后旧 cookie 立即失效", st == 401, st)
        st, _, b = call(MASTER + "/api/access/login", "POST", {"password": pwd})
        check("改口令后管理员密码不再能通过", st == 401, st)
        st, h, b = call(MASTER + "/api/access/login", "POST", {"password": "guest-pw-123"})
        check("新访问口令可用", st == 200, b[:90])
        acc2 = h.get("Set-Cookie", "").split(";")[0]
        st, _, b = call(MASTER + "/api/targets", "POST", {"url": TARGET}, cookie=acc2)
        check("用新口令换来的 cookie 可用", st == 200, st)

        st, _, b = call(MASTER + "/api/admin/config", cookie=cookie)
        raw = b.decode("utf-8", "replace")
        check("配置接口不回传口令本身", "guest-pw-123" not in raw, raw[:120])
        check("配置接口标记已单独设口令", json.loads(b)["has_access_password"] is True)

        st, _, b = call(MASTER + "/api/admin/config", "PUT",
                        {"access_password": "abc"}, cookie=cookie)
        check("过短口令被拒 400", st == 400, st)

        st, _, b = call(MASTER + "/api/access/logout", "POST", cookie=acc2)
        check("退出验证接口可用", st == 200, st)

        st, _, b = call(MASTER + "/api/admin/config", "PUT",
                        {"access_password": "", "require_login": False}, cookie=cookie)
        check("清除口令并关闭验证", st == 200, b[:90])
        st, _, b = call(MASTER + "/api/targets", "POST", {"url": TARGET})
        check("关闭后恢复免验证使用", st == 200, st)
        check("清除后标记为未单独设口令",
              json.loads(call(MASTER + "/api/admin/config", cookie=cookie)[2])
              ["has_access_password"] is False)

        # ---- 主页面不暴露节点地址 ----
        print("\n== 主页面不显示节点 IP / 端口 ==")
        with open(os.path.join(ROOT, "web", "index.html"), encoding="utf-8") as fh:
            idx = fh.read()
        check("卡片不再渲染 n.base 文本", "n.base || ''" not in idx and "class=\"nhost\"" not in idx)
        check("改显示地区名", "regionLabel(n.region)" in idx and "function regionLabel" in idx)
        check("地区名映射含中国香港/中国台湾等", "中国香港" in idx and "中国台湾" in idx
              and "中国澳门" in idx)
        check("仍保留 base 用于浏览器直连测速(否则延迟排序失效)", "_node.base" in idx)

        # ---- 验证页前端 ----
        print("\n== 验证页前端 ==")
        check("有验证页容器", 'id="gate"' in idx and 'id="gpw"' in idx and 'id="gbtn"' in idx)
        check("主界面默认隐藏, 避免验证前闪出内容", 'id="app" hidden' in idx)
        check("启动时先查 /api/access/status", "/api/access/status" in idx)
        check("提交走 /api/access/login", "/api/access/login" in idx)
        check("解析遇 401 会弹回验证页", "r.status === 401" in idx and "showGate(" in idx)
        check("验证通过后才解析分享链接", "PENDING_URL" in idx and "function startApp" in idx)
        with open(os.path.join(ROOT, "web", "admin.html"), encoding="utf-8") as fh:
            adm2 = fh.read()
        check("管理面板有访问验证开关", 'id="c-require"' in adm2)
        check("管理面板有访问口令输入", 'id="c-accesspass"' in adm2)
        check("管理面板可清除自定义口令", 'id="clearAccess"' in adm2)
        check("保存时带上 require_login", "require_login: $('#c-require').value === '1'" in adm2)
        check("空口令不会被误清空(需显式点清除)",
              "else if (accessCleared) body.access_password = ''" in adm2)

        # ---- 管理面板的复制按钮 ----
        # navigator.clipboard 只在安全上下文可用; 面板常用 http://<IP>:端口 打开,
        # 直接调用会同步抛 TypeError 打断 onclick —— 表现是"点按钮完全没反应"。
        print("\n== 管理面板复制按钮 (http 非安全上下文) ==")
        with open(os.path.join(ROOT, "web", "admin.html"), encoding="utf-8") as fh:
            adm = fh.read()
        check("定义了带降级的 copyText()", "function copyText(" in adm)
        check("clipboard 调用前判存在性", "navigator.clipboard && window.isSecureContext" in adm)
        check("有 execCommand 兜底", "document.execCommand('copy')" in adm)
        check("兜底用 textarea 且处理选区", "createElement('textarea')" in adm
              and "setSelectionRange" in adm)
        check("复制统一走 doCopy()", "function doCopy(" in adm)
        # 关键: 全文只允许有一处 navigator.clipboard.writeText, 且必须在安全上下文守卫之后。
        # 任何"散落在外的裸调"都会在 http 页面下同步抛 TypeError 打断 onclick。
        n_call = adm.count("navigator.clipboard.writeText")
        guard_at = adm.find("navigator.clipboard && window.isSecureContext")
        call_at = adm.find("navigator.clipboard.writeText")
        check("全站只有一处 clipboard 调用", n_call == 1, "出现 %d 次" % n_call)
        check("该调用在安全上下文守卫之后", guard_at != -1 and guard_at < call_at,
              "guard@%d call@%d" % (guard_at, call_at))
        check("安装命令复制按钮已接入", "doCopy(m.querySelector('#cmdbox').textContent" in adm)
        check("卸载命令复制按钮已接入", "doCopy($('#' + preId).textContent" in adm)
        check("生成命令复制按钮已接入", "doCopy($('#gen-cmd').textContent" in adm)

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
