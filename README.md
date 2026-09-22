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

## 界面

- **用户页**：粘贴地址 → 列出全部节点 → 浏览器并发测速 → 按延迟升序排列，最快的一条标「推荐」。
- **主题**：白天 / 夜晚 / 跟随系统，三态切换，记住选择，首屏无闪烁。
- **国旗**：每个节点可按地区代码显示国旗，一眼看出落地在哪。
- **管理面板**：节点增删启停、实时健康状态、下载解析记录（含日期筛选、关键词搜索、分页）、
  令牌有效期、地区标识、记录保留策略、从节点一键安装命令。

## 分享 / 收藏下载链接

用户页面支持用查询参数直接触发解析，打开即自动测速排序：

```
http://<主服务器>:20808/?url=https://example.com/big.iso
```

可以直接存成书签，或者发给别人。

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
| POST | `/api/targets` | `{"url":"..."}` → 各节点的签名下载直链 |
| GET | `/dl?t=<令牌>` | 流式中转下载 |
| POST | `/api/login` | 面板登录 |
| GET/POST | `/api/admin/nodes` | 节点列表（含健康状态）/ 新增 |
| PUT/DELETE | `/api/admin/nodes/{id}` | 修改 / 删除 |
| POST | `/api/admin/nodes/{id}/test` | 主服务器侧测节点延迟 |
| POST | `/api/admin/health/check` | 立即重测全部节点 |
| GET/DELETE | `/api/admin/records` | 下载记录查询（`from`/`to`/`q`/`limit`/`offset`）/ 清空 |
| GET/PUT | `/api/admin/config` | 全局配置（令牌有效期、地区、保留天数、账号） |
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
python test_features.py   # 78 项: 主题 / 记录 / 健康 / 一键安装卸载 / 配置
```

覆盖：接口可用性、SSRF 防护、鉴权、令牌伪造、**中转字节 md5 完整性**、Range 断点续传、
主从权限一致性、节点增删启停、64MB 流式传输、记录筛选与保留策略、离线节点过滤、
安装/卸载脚本内容与护栏、版本号格式。

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
