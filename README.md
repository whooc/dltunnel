# dltunnel — 网页中转下载器

[![CI](https://github.com/whooc/dltunnel/actions/workflows/ci.yml/badge.svg)](https://github.com/whooc/dltunnel/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/whooc/dltunnel)](https://github.com/whooc/dltunnel/releases/latest)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

粘贴一个下载地址，页面列出**所有中转节点**，并按**你的实测延迟**排序，挑最快的下载。
主从节点全程流式中转，**不落盘、不留存文件**，不占用服务器磁盘。

## 架构

```
                    ┌─────────────────────────────────────┐
   浏览器  ────────▶│  主服务器 :20808                     │
   (测速/下载)      │  ├─ /        用户页面                │
      │             │  ├─ /admin   管理面板                │
      │             │  ├─ /dl      自身也是一个下载节点     │
      │             │  ├─ /agent.sh 从节点一键安装脚本      │
      │             │  └─ /api/*   签发令牌 / 节点管理      │
      │             └──────────────┬──────────────────────┘
      │                            │ HMAC-SHA256 令牌
      │                            ▼
      │            ┌──────────┬──────────┬──────────┐
      └───────────▶│ 从节点A  │ 从节点B  │ 从节点C  │  :20809
                   └──────────┴──────────┴──────────┘
```

**同一份二进制，两种模式**：`-mode master` 与 `-mode agent`。纯 Go 标准库，零第三方依赖，
交叉编译出一个几 MB 的静态二进制，扔到服务器上就能跑。

- 主服务器**不承担下载流量**。它只做两件事：给每个节点签发带签名的下载令牌、提供管理面板。
- 浏览器拿到令牌后**直连**选中的节点下载，所以"延迟排序"是用户视角的真实延迟。
- 节点之间互相独立，每个节点一把独立密钥；一把泄露不影响其他节点。

## 关键设计

| 问题 | 做法 |
|---|---|
| 不占磁盘 | `io.CopyBuffer` 64KB 缓冲流式转发，全程不落盘 |
| 断点续传 / 多线程下载 | 透传 `Range` / `If-Range` / `ETag` 等头，`206` / `416` 原样返回 |
| 防盗链 | 下载必须带主服务器签发的 HMAC 令牌，含过期时间 |
| 防 SSRF | 默认禁止回环/私有段/链路本地/CGNAT/云元数据地址，域名解析后逐个 IP 校验 |
| 主从权限一致 | `allow_private` 开关随令牌下发，从节点按令牌执行，无需单独配置 |
| 长下载不中断 | 服务端刻意**不设** `WriteTimeout`，大文件可跑数十分钟 |
| 内存占用 | `GOMEMLIMIT` 限制 Go GC 目标，1.9G 内存的机器也稳 |
| 节点故障自动隐藏 | 后台每 30 秒探测 `/__ping`，连续 2 次失败才判离线，离线节点不出现在用户页面 |
| 用户页防传播 | 可选访问口令，未验证时 `/api/targets` 直接 `401`；访客会话主体由口令 HMAC 派生，改口令即失效 |
| 页面不暴露节点地址 | 卡片只显示地区名；下载直链仍需含地址（浏览器要直连测速），见下方说明 |

## 界面

- **用户页**：粘贴地址 → 列出全部节点 → 浏览器并发测速 → 按延迟升序排列，最快的一条标「推荐」。
- **主题**：白天 / 夜晚 / 跟随系统，三态切换，记住选择，首屏无闪烁。
- **国旗**：每个节点可按地区代码显示国旗，一眼看出落地在哪。
- **不暴露节点地址**：卡片上只显示地区名，不显示 IP 和端口。
- **访问验证（可选）**：开启后用户页先出验证页，输入口令才能用。
- **管理面板**：节点增删启停、实时健康状态、下载解析记录（含日期筛选、关键词搜索、分页）、
  令牌有效期、地区标识、记录保留策略、访问验证开关、从节点一键安装命令。

## 分享 / 收藏下载链接

用户页面支持用查询参数直接触发解析，打开即自动测速排序：

```
http://<主服务器>:20808/?url=https://example.com/big.iso
```

可以直接存成书签，或者发给别人。

## 用户页面访问验证（可选）

用户页面默认谁打开都能用。不想让链接被随手转发，就到 **管理面板 →「设置」→「用户页面访问验证」**
把开关打开，并设一个**访问口令**。开启后：

- 用户页先显示验证页，输入口令才能进主界面；
- 未通过验证时 `POST /api/targets` 直接返回 `401`，**不签发任何令牌，也不泄露任何节点信息**；
- 验证通过后下发一个 `HttpOnly` Cookie，有效期 **30 天**，过期或清 Cookie 后重新输入；
- 同一个浏览器里登录过管理面板的话，用户页**免验证**（复用管理会话）。

**口令回退规则**：访问口令留空时自动回退用**管理员密码** —— 不想多记一个密码就这么用。
两者独立，改访问口令不影响管理员密码。

> ⚠️ **但如果回退到管理员密码，就等于把下载页口令和管理面板口令变成同一个。**
> 你把用户页口令给了别人，别人同时也就拿到了 `/admin` 的登录凭据。
> 只要用户页需要给外人用，就**应该单独设一个访问口令**，别图省事用回退。

**改口令 = 旧会话立刻失效**：访客 Cookie 里存的不是口令本身，而是用 `session_key` 和口令
一起 HMAC 出来的主体标识。改口令后，之前发出去的访客 Cookie 立即失效，不用手动踢人；
Cookie 里也**不含口令的任何可逆信息**。

口令最少 4 位。失败没有锁定，但服务端每次失败强制延迟 400ms 拖慢爆破。

> 验证页是**入口门禁**，不是加密。它挡的是"链接被随手转发"，不挡有心人抓包分析。

## 节点地址不会显示在页面上

用户页的节点卡片**只显示地区名**（如「中国香港」「美国」），不显示节点的 IP 和端口。

> **固有限制**：卡片上看不到地址，但下载链接（`<a href>`）里**必须**带节点地址 ——
> 因为"按你的实测延迟排序"要求**浏览器直连各节点**测速和下载（主服务器不承担下载流量
> 是这个项目的核心设计）。所以懂技术的人打开浏览器 Network 面板仍能看到节点地址。
> 要彻底隐藏只能改成主服务器代理测速与下载，那等于推翻延迟排序的前提，代价是主服务器
> 承担全部流量。当前取舍：**页面不主动暴露，但不做防抓包承诺**。

## 部署

### 主服务器（一键）

```bash
curl -fsSL https://raw.githubusercontent.com/whooc/dltunnel/main/install.sh | bash
```

默认装到 `/opt/dltunnel`，监听 `:20808`，注册 systemd 服务 `dltunnel` 并开机自启。
可选参数：`--port 20808`、`--dir /opt/dltunnel`、`--version v1.1.0`。

安装完成后查看管理面板初始密码：

```bash
journalctl -u dltunnel -n 20 | grep 密码
```

### 主服务器（从源码）

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o dltunnel .
scp dltunnel root@<IP>:/opt/dltunnel/dltunnel
```

### 添加从节点（一键）

1. 管理面板 →「设置」页 → 生成节点密钥，复制那条 `curl ... | bash -s -- --secret ...` 命令
2. 在从服务器上以 root 执行这条命令
3. 脚本会打印该节点的公网地址，回到面板「节点」页把它填进去
4. 点「测试」出现绿色延迟数字即接通

安装脚本由**主服务器动态生成**，二进制也从主服务器 `/bin/` 拉取 ——
从节点不需要能访问 GitHub，也不需要能访问外网。

参数有两种给法，效果一样：

```bash
# 1) 命令行参数（面板「添加从节点」生成的就是这种）
curl -fsSL http://<主服务器>:20808/agent.sh | bash -s -- \
     --secret "<密钥>" --name "<名称>" --port 20809

# 2) 直接写在链接上
curl -fsSL "http://<主服务器>:20808/agent.sh?secret=<密钥>&name=<名称>&port=20809" | bash
```

其它可用参数：`--server <主服务器地址>`（默认已内嵌）、`--help` 看用法。
端口必须是纯数字，未知参数会被明确拒绝而不是静默忽略。
节点密钥会写进 `/etc/systemd/system/dltunnel-agent.service`，该文件权限已收紧为 `600`。

### 手动运行（不走 systemd）

```bash
./dltunnel -mode master -listen :20808 -data ./data -bindir ./bin
./dltunnel -mode agent  -listen :20809 -name "HK-01" -secret <密钥>
```

### 卸载

**主服务器**：

```bash
curl -fsSL https://raw.githubusercontent.com/whooc/dltunnel/main/uninstall.sh | bash
```

默认只移除服务和程序，**保留 `data/`（管理密码、节点配置、下载记录）**。
彻底清干净加 `--purge`：

```bash
curl -fsSL https://raw.githubusercontent.com/whooc/dltunnel/main/uninstall.sh | bash -s -- --purge
```

安装时用过 `--dir` 的，卸载也要带上同一个 `--dir`。

**从节点**：命令由主服务器提供，管理面板「设置 → 一键卸载」里可直接复制：

```bash
curl -fsSL http://<主服务器>:20808/agent-uninstall.sh | bash
```

两个卸载脚本都会先停服务、禁用开机自启、删除 systemd unit，
再结束残留进程并清理安装目录，最后打印结果。主服务器卸载后端口 20808 会释放。

> **卸载后用户页面不会立刻少一个节点。** 主服务器每 30 秒探测一次节点，
> 并且**连续 2 次失败**才判定离线（避免网络抖动把好节点踢掉），
> 所以节点从用户页面消失最长要等约 60 秒。实测：节点宕机后 52.6 秒被标记离线、
> 53.0 秒从用户侧摘除。
> 想立刻生效，就到管理面板「节点」页把这条记录删掉 —— 删除是立即生效的。

## 用域名访问（可选）

`dltunnel` 自身只监听高位端口（默认 20808），用 IP 直连最省事。要挂域名就在前面加一层反向代理。

**用 Nginx / OpenResty 反代时有两个必须关掉的开关**，否则会破坏"不落盘"：

```nginx
location ^~ / {
    proxy_pass http://127.0.0.1:20808;
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;

    # —— 关键 ——
    proxy_buffering off;          # 否则大文件先落盘再转发, 且无法边下边传
    proxy_request_buffering off;
    proxy_cache off;              # 若 http 块里有全局 proxy_cache, 必须显式关掉
    proxy_read_timeout 3600s;     # 大文件下载可能跑几十分钟
    proxy_send_timeout 3600s;
    chunked_transfer_encoding on;
    client_max_body_size 0;
}
```

HTTPS 直接在反代层做（Let's Encrypt / acme.sh 通配符证书均可），
`X-Forwarded-Proto` 会被程序读取，用于生成正确的下载直链。

**这两个开关不是"建议"，是必须。** 实测数据（Docker 起两个 nginx 容器反代，
源站 64MB、支持 `Range`、带 `Cache-Control: public`，客户端限速 4MB/s 模拟慢速用户）：

| 指标 | 配了 `proxy_buffering off; proxy_cache off;` | 漏了（继承 http 块的全局 `proxy_cache`） |
|---|---|---|
| 传输中磁盘峰值 | **0.0 MB** | **78.9 MB** |
| 传输结束后磁盘残留 | **0.0 MB** | **64.0 MB** |

漏掉后，**用户每下载一个文件，反代层就往磁盘留一份完整副本，而且永不清理** ——
因为下载令牌每次请求都不同，而 nginx 默认的 `proxy_cache_key` 含 `$request_uri`，
缓存**永远命中不了**，只堆垃圾。20G 的机器几十个下载就能写满。

> 验证时注意：别拿 `speed.cloudflare.com/__down` 这类源站测，它既不支持 `Range`
> （永远返回 200，看不出断点续传有没有坏）也不带缓存头（`proxy_cache` 不会落盘），
> 什么问题都测不出来。要带 `Content-Length`、支持 `Range`、声明可缓存的源站才行。

## HTTP 接口

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/` | 用户页面 |
| GET | `/admin` | 管理面板 |
| GET | `/health` | 纯文本健康检查 |
| GET | `/__ping` | 延迟探测（无鉴权，节点互相探测用） |
| GET | `/agent.sh?secret=&name=&port=` | 动态生成从节点安装脚本 |
| GET | `/agent-uninstall.sh` | 从节点一键卸载脚本 |
| GET | `/bin/{dltunnel-linux-amd64\|arm64}` | 分发二进制（白名单，防路径穿越） |
| POST | `/api/targets` | `{"url":"..."}` → 各节点的签名下载直链（开启验证后需先通过验证） |
| GET | `/api/access/status` | `{"required":bool,"ok":bool}` 用户页判断要不要出验证页 |
| POST | `/api/access/login` | `{"password":"..."}` → 下发访客 Cookie |
| POST | `/api/access/logout` | 清除访客 Cookie |
| GET | `/dl?t=<令牌>` | 流式中转下载 |
| POST | `/api/login` | 面板登录 |
| GET/POST | `/api/admin/nodes` | 节点列表（含健康状态）/ 新增 |
| PUT/DELETE | `/api/admin/nodes/{id}` | 修改 / 删除（PUT 是**部分更新**：只改传过来的字段） |
| POST | `/api/admin/nodes/{id}/test` | 主服务器侧测节点延迟 |
| POST | `/api/admin/health/check` | 立即重测全部节点 |
| GET/DELETE | `/api/admin/records` | 下载记录查询（`from`/`to`/`q`/`limit`/`offset`）/ 清空 |
| GET/PUT | `/api/admin/config` | 全局配置（令牌有效期、地区、保留天数、账号、访问验证） |
| GET | `/api/admin/stats` | 运行统计 |

## 测试

先编译（测试会启动真实进程）：

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o dist/dltunnel-linux-amd64 .
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o dist/dltunnel-linux-arm64 .
```

再跑：

```bash
python test_e2e.py        # 45 项: 基础回归
python test_features.py   # 154 项: 主题 / 记录 / 健康 / 一键安装卸载 / 配置 / 访问验证
```

覆盖：接口可用性、SSRF 防护、鉴权、令牌伪造、**中转字节 md5 完整性**、Range 断点续传、
主从权限一致性、节点增删启停、64MB 流式传输、记录筛选与保留策略、离线节点过滤、
安装/卸载脚本内容与护栏、版本号格式、**访问验证门禁与节点地址隐藏**。

测试会自动按平台选择被测二进制（Windows 用 `dist/dltunnel.exe`，
Linux 用 `dist/dltunnel-linux-{amd64,arm64}`），也可以用 `DL_BIN` 显式指定。

CI 在 `ubuntu-latest` 与 `windows-latest` 上各跑一遍，见
[`.github/workflows/ci.yml`](.github/workflows/ci.yml)。

## 注意

- 管理面板统计的「累计中转」只统计**主服务器自身节点**的流量；各从节点在各自进程内独立统计。
- 服务默认监听 `:20808`（主）/ `:20809`（从），不占用 80/443。
- 换密钥后需同步更新从节点上 agent 的 `-secret` 并 `systemctl restart dltunnel-agent`。
- 下载记录只保存**元数据**（时间、来源 IP、目标地址、可用节点数），不含任何下载内容。

## License

MIT
