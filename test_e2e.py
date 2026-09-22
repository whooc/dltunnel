#!/usr/bin/env python3
"""dltunnel 端到端测试.

核心验证: 中转后的字节与源文件逐字节一致 (md5 校验), 支持 Range 断点续传.
用一个自建的、内容已知的 HTTP 源做基准, 不依赖外部站点.
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
MASTER = "http://127.0.0.1:18080"
AGENT = "http://127.0.0.1:18081"
SRC_PORT = 18090
DATADIR = os.path.join(ROOT, "smoke2")

# 3 MiB 确定性伪随机数据
SRC_DATA = bytes(((i * 2654435761) ^ (i >> 7)) & 0xFF for i in range(3 * 1024 * 1024))
SRC_MD5 = hashlib.md5(SRC_DATA).hexdigest()
TARGET = "http://127.0.0.1:%d/blob.bin" % SRC_PORT

PASS, FAIL = [], []


def check(name, cond, extra=""):
    (PASS if cond else FAIL).append(name)
    mark = "[OK]  " if cond else "[FAIL]"
    tail = "" if cond or extra == "" else "  -> " + str(extra)
    print("  %s %s%s" % (mark, name, tail))


def call(url, method="GET", body=None, cookie=None, headers=None, timeout=60):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=data, method=method)
    if data is not None:
        req.add_header("Content-Type", "application/json")
    if cookie:
        req.add_header("Cookie", cookie)
    for k, v in (headers or {}).items():
        req.add_header(k, v)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, dict(r.headers), r.read()
    except urllib.error.HTTPError as e:
        return e.code, dict(e.headers), e.read()
    except Exception as e:
        return 0, {}, str(e).encode()


def sign_token(secret, url, priv):
    """按 dltunnel 的令牌格式在测试侧独立签发, 用于验证 agent 的校验逻辑."""
    import base64
    import hmac as _hmac
    payload = {"u": url, "e": int(time.time()) + 300, "n": "test"}
    if priv:
        payload["p"] = True
    body = base64.urlsafe_b64encode(json.dumps(payload).encode()).rstrip(b"=").decode()
    sig = base64.urlsafe_b64encode(
        _hmac.new(secret.encode(), body.encode(), hashlib.sha256).digest()).rstrip(b"=").decode()
    return body + "." + sig


def wait_port(port, timeout=10):
    end = time.time() + timeout
    while time.time() < end:
        try:
            with socket.create_connection(("127.0.0.1", port), 0.4):
                return True
        except OSError:
            time.sleep(0.15)
    return False


class RangeHandler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def do_GET(self):
        data = SRC_DATA
        rng = self.headers.get("Range")
        if rng and rng.startswith("bytes="):
            s, _, e = rng[6:].partition("-")
            start = int(s) if s else 0
            end = int(e) if e else len(data) - 1
            end = min(end, len(data) - 1)
            if start > end:
                self.send_response(416)
                self.send_header("Content-Length", "0")
                self.end_headers()
                return
            chunk = data[start:end + 1]
            self.send_response(206)
            self.send_header("Content-Range", "bytes %d-%d/%d" % (start, end, len(data)))
            self.send_header("Content-Length", str(len(chunk)))
            self.send_header("Accept-Ranges", "bytes")
            self.send_header("Content-Type", "application/octet-stream")
            self.end_headers()
            self.wfile.write(chunk)
        else:
            self.send_response(200)
            self.send_header("Content-Length", str(len(data)))
            self.send_header("Accept-Ranges", "bytes")
            self.send_header("Content-Type", "application/octet-stream")
            self.end_headers()
            self.wfile.write(data)

    def do_HEAD(self):
        self.send_response(200)
        self.send_header("Content-Length", str(len(SRC_DATA)))
        self.send_header("Accept-Ranges", "bytes")
        self.end_headers()

    def log_message(self, *a):
        pass


def main():
    global SRC_DATA
    if os.path.isdir(DATADIR):
        shutil.rmtree(DATADIR, ignore_errors=True)

    srv = http.server.ThreadingHTTPServer(("127.0.0.1", SRC_PORT), RangeHandler)
    threading.Thread(target=srv.serve_forever, daemon=True).start()

    env = dict(os.environ)
    env["NO_PROXY"] = "127.0.0.1,localhost"
    env["no_proxy"] = "127.0.0.1,localhost"

    print("== 启动主服务器 ==")
    mlog = open(os.path.join(ROOT, "smoke2_master.log"), "w+", encoding="utf-8")
    master = subprocess.Popen([EXE, "-mode", "master", "-listen", "127.0.0.1:18080",
                               "-data", DATADIR], stdout=mlog, stderr=subprocess.STDOUT, env=env)
    agent = None
    try:
        if not wait_port(18080):
            print("主服务器未能启动")
            return 1
        cfg = json.load(open(os.path.join(DATADIR, "config.json"), encoding="utf-8"))
        user, pwd = cfg["admin_user"], cfg["admin_pass"]
        print("  管理凭据: %s / %s" % (user, pwd))
        print("  测试源:   %s  (%d 字节, md5=%s)" % (TARGET, len(SRC_DATA), SRC_MD5))

        print("\n== 基础接口 ==")
        st, _, _ = call(MASTER + "/health")
        check("GET /health", st == 200, st)
        st, h, b = call(MASTER + "/__ping")
        check("GET /__ping 带 CORS 头", st == 200 and h.get("Access-Control-Allow-Origin") == "*")
        st, _, b = call(MASTER + "/")
        check("GET / 用户页面", st == 200 and "中转下载器".encode() in b, st)
        st, _, b = call(MASTER + "/admin")
        check("GET /admin 管理页面", st == 200 and "管理面板".encode() in b, st)

        print("\n== SSRF / 协议防护 ==")
        st, _, b = call(MASTER + "/api/targets", "POST", {"url": "http://127.0.0.1:22/x"})
        check("回环地址被拒绝", st == 400 and "内网".encode() in b, b[:100])
        st, _, b = call(MASTER + "/api/targets", "POST", {"url": "http://10.0.0.1/a"})
        check("私有网段被拒绝", st == 400, b[:100])
        st, _, b = call(MASTER + "/api/targets", "POST", {"url": "http://169.254.169.254/latest/meta-data/"})
        check("云元数据地址被拒绝", st == 400, b[:100])
        st, _, b = call(MASTER + "/api/targets", "POST", {"url": "file:///etc/passwd"})
        check("非 http(s) 协议被拒绝", st == 400, b[:100])
        st, _, b = call(MASTER + "/api/targets", "POST", {"url": "not-a-url"})
        check("非法地址被拒绝", st == 400, b[:100])
        st, _, b = call(MASTER + "/api/admin/nodes")
        check("未登录访问管理接口 401", st == 401, st)
        st, _, b = call(MASTER + "/api/login", "POST", {"username": user, "password": "wrong"})
        check("错误密码被拒绝", st == 401, st)

        print("\n== 登录 ==")
        st, h, b = call(MASTER + "/api/login", "POST", {"username": user, "password": pwd})
        check("正确凭据登录", st == 200, b[:100])
        cookie = h.get("Set-Cookie", "").split(";")[0]
        check("下发会话 Cookie", cookie.startswith("dlt_session="))
        st, _, b = call(MASTER + "/api/admin/nodes", cookie=cookie)
        check("带 Cookie 访问管理接口", st == 200, st)

        print("\n== 令牌安全 ==")
        st, _, b = call(MASTER + "/dl?t=forged.token")
        check("伪造令牌被拒绝", st == 403, st)
        st, _, b = call(MASTER + "/dl")
        check("无令牌被拒绝", st == 403, st)

        print("\n== 开启内网下载 (用于本地基准测试) ==")
        st, _, b = call(MASTER + "/api/admin/config", "PUT", {"allow_private": True}, cookie=cookie)
        check("配置接口修改 allow_private", st == 200, b[:100])

        print("\n== 主服务器节点: 字节完整性 ==")
        st, _, b = call(MASTER + "/api/targets", "POST", {"url": TARGET})
        check("解析下载地址", st == 200, b[:150])
        data = json.loads(b)
        check("返回内置节点", any(n.get("builtin") for n in data["nodes"]))
        link = data["nodes"][0]["link"]
        st, h, body = call(link)
        check("中转下载 HTTP 200", st == 200, st)
        check("响应头 Content-Length 与实际字节一致",
              h.get("Content-Length") == str(len(body)),
              "%s vs %d" % (h.get("Content-Length"), len(body)))
        check("中转内容 md5 与源文件完全一致",
              hashlib.md5(body).hexdigest() == SRC_MD5,
              "%s vs %s" % (hashlib.md5(body).hexdigest(), SRC_MD5))
        check("标记为流式中转", h.get("X-Dltunnel") == "stream")

        print("\n== Range 断点续传 ==")
        st, h, rb = call(link, headers={"Range": "bytes=1000000-1999999"})
        check("Range 返回 206", st == 206, st)
        check("Range 返回 1000000 字节", len(rb) == 1000000, len(rb))
        check("Range 内容与源文件对应片段一致", rb == SRC_DATA[1000000:2000000])
        check("Content-Range 头正确",
              h.get("Content-Range") == "bytes 1000000-1999999/%d" % len(SRC_DATA),
              h.get("Content-Range"))
        st, h, rb = call(link, headers={"Range": "bytes=3145728-"})
        check("越界 Range 返回 416", st == 416, st)

        print("\n== 从节点 (agent) ==")
        secret = "testsecret" + str(int(time.time()))
        alog = open(os.path.join(ROOT, "smoke2_agent.log"), "w+", encoding="utf-8")
        agent = subprocess.Popen([EXE, "-mode", "agent", "-listen", "127.0.0.1:18081",
                                  "-name", "本地测试节点", "-secret", secret],
                                 stdout=alog, stderr=subprocess.STDOUT, env=env)
        check("agent 端口就绪", wait_port(18081))
        st, h, b = call(AGENT + "/__ping")
        check("agent /__ping 可用", st == 200 and b'"ok":true' in b.replace(b" ", b""))

        st, _, b = call(MASTER + "/api/admin/nodes", "POST",
                        {"name": "本地测试节点", "base_url": AGENT, "region": "LOCAL",
                         "secret": secret, "enabled": True}, cookie=cookie)
        check("添加从节点", st == 200, b[:150])
        node_id = json.loads(b)["node"]["id"]

        st, _, b = call(MASTER + "/api/admin/nodes/%s/test" % node_id, "POST", cookie=cookie)
        check("主服务器测节点延迟", st == 200 and json.loads(b).get("ok"), b[:150])

        st, _, b = call(MASTER + "/api/targets", "POST", {"url": TARGET})
        data = json.loads(b)
        check("解析结果含 2 个节点", len(data["nodes"]) == 2, len(data["nodes"]))
        agent_link = [n for n in data["nodes"] if not n.get("builtin")][0]["link"]
        st, h, body = call(agent_link)
        check("从节点中转下载 HTTP 200", st == 200, st)
        check("从节点中转内容 md5 一致",
              hashlib.md5(body).hexdigest() == SRC_MD5, hashlib.md5(body).hexdigest())

        print("\n== 权限随令牌下发 (主从一致) ==")
        st, _, b = call(AGENT + "/dl?t=" + sign_token(secret, TARGET, False))
        check("令牌未授权内网时 agent 拒绝内网地址", st == 400, "%s %s" % (st, b[:80]))
        st, _, b = call(AGENT + "/dl?t=" + sign_token(secret, TARGET, True))
        check("令牌授权内网时 agent 正常中转且内容一致",
              st == 200 and hashlib.md5(b).hexdigest() == SRC_MD5, st)
        st, _, b = call(AGENT + "/dl?t=" + sign_token("wrong-secret", TARGET, True))
        check("agent 拒绝其他密钥签发的令牌", st == 403, st)

        print("\n== 从节点密钥隔离 ==")
        st, _, b = call(agent_link.replace("t=", "t=xx", 1))
        check("从节点拒绝伪造令牌", st == 403, st)
        st, _, b = call(MASTER + "/api/admin/nodes", "POST",
                        {"name": "错误密钥节点", "base_url": AGENT, "secret": "another-secret",
                         "enabled": True}, cookie=cookie)
        bad_id = json.loads(b)["node"]["id"]
        st, _, b = call(MASTER + "/api/targets", "POST", {"url": TARGET})
        wrong_link = [n for n in json.loads(b)["nodes"]
                      if not n.get("builtin") and n["id"] == bad_id][0]["link"]
        st, _, b = call(wrong_link)
        check("密钥不匹配的节点拒绝服务", st == 403, st)
        call(MASTER + "/api/admin/nodes/" + bad_id, "DELETE", cookie=cookie)

        print("\n== 节点启停 ==")
        st, _, b = call(MASTER + "/api/admin/nodes/" + node_id, "PUT",
                        {"name": "本地测试节点", "base_url": AGENT, "secret": secret,
                         "region": "LOCAL", "enabled": False}, cookie=cookie)
        check("停用节点", st == 200, b[:100])
        st, _, b = call(MASTER + "/api/targets", "POST", {"url": TARGET})
        check("停用后不出现在列表", len(json.loads(b)["nodes"]) == 1, len(json.loads(b)["nodes"]))
        st, _, b = call(MASTER + "/api/admin/nodes/" + node_id, "DELETE", cookie=cookie)
        check("删除节点", st == 200, b[:100])

        print("\n== 统计 ==")
        st, _, b = call(MASTER + "/api/admin/stats", cookie=cookie)
        s = json.loads(b)["stats"]
        check("统计记录中转次数与流量 (主服务器自身节点)", s["total"] >= 3 and s["bytes"] >= len(SRC_DATA), s)

        print("\n== 大文件流式传输 ==")
        SRC_DATA = bytes((i * 7919) & 0xFF for i in range(64 * 1024 * 1024))
        big_md5 = hashlib.md5(SRC_DATA).hexdigest()
        st, _, b = call(MASTER + "/api/targets", "POST", {"url": TARGET})
        link2 = json.loads(b)["nodes"][0]["link"]
        t0 = time.time()
        st, h, body = call(link2, timeout=300)
        dt = time.time() - t0
        check("64MB 文件完整中转 (md5 一致)",
              hashlib.md5(body).hexdigest() == big_md5, hashlib.md5(body).hexdigest())
        print("       %d 字节 / %.2f 秒 = %.1f MB/s" % (len(body), dt, len(body) / dt / 1048576))

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
