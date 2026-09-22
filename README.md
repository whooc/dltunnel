# dltunnel — 网页中转下载器

粘贴一个下载地址，页面列出**所有中转节点**，并按**你的实测延迟**排序，挑最快的下载。
主从节点全程流式中转，**不落盘、不留存文件**，不占用服务器磁盘。

## 架构

```
                    ┌─────────────────────────────────────┐
   浏览器  ────────▶│  主服务器 :20808                     │
   (测速/下载)      │  ├─ /        用户页面                │
      │             │  ├─ /admin   管理面板                │
      │             │  ├─ /dl      自身也是一个下载节点     │
      │             │  └─ /api/*   签发令牌 / 节点管理      │
      │             └──────────────┬──────────────────────┘
      │                            │ HMAC-SHA256 令牌
      │                            ▼
      │            ┌──────────┬──────────┬──────────┐
      └───────────▶│ 从节点A  │ 从节点B  │ 从节点C  │  :20809
                   └──────────┴──────────┴──────────┘
```

**同一份二进制，两种模式**：`-mode master` 与 `-mode agent`。

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

## 分享 / 收藏下载链接

用户页面支持用查询参数直接触发解析，打开即自动测速排序：

```
http://<主服务器>:20808/?url=https://example.com/big.iso
```

可以直接存成书签，或者发给别人。

## 部署

### 主服务器

```bash
python tools/deploy_dltunnel.py master --host <IP> --password <SSH密码>
```

首次启动会生成 `/opt/dltunnel/data/config.json`，并在日志里打印管理面板初始密码：

```bash
journalctl -u dltunnel -n 20 | grep 密码
```

### 添加从节点

1. 管理面板 →「+ 添加从节点」→ 填写名称和地址，**复制弹窗里生成的命令和密钥**
2. 在从服务器上执行：

```bash
python tools/deploy_dltunnel.py agent --host <从服务器IP> --password <密码> \
    --name "HK-01" --secret <面板里的密钥>
```

3. 回到面板点「测试」，出现绿色延迟数字即接通

### 手动运行（不走 systemd）

```bash
./dltunnel -mode master -listen :20808 -data ./data
./dltunnel -mode agent  -listen :20809 -name "HK-01" -secret <密钥>
```

## 重新编译

```bash
cd dltunnel && ../tools/build.sh      # 或直接:
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o dist/dltunnel_linux_amd64 .
```

无第三方依赖，纯标准库，交叉编译即可。

## 测试

```bash
python dltunnel/test_e2e.py
```

45 项端到端断言，覆盖：接口可用性、SSRF 防护、鉴权、令牌伪造、
**中转字节 md5 完整性**、Range 断点续传、主从权限一致性、节点增删启停、64MB 流式传输。

## HTTP 接口

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/` | 用户页面 |
| GET | `/admin` | 管理面板 |
| POST | `/api/targets` | `{"url":"..."}` → 各节点的签名下载直链 |
| GET | `/dl?t=<令牌>` | 流式中转下载 |
| GET | `/__ping` | 延迟探测（无鉴权） |
| POST | `/api/login` | 面板登录 |
| GET/POST | `/api/admin/nodes` | 节点列表 / 新增 |
| PUT/DELETE | `/api/admin/nodes/{id}` | 修改 / 删除 |
| POST | `/api/admin/nodes/{id}/test` | 主服务器侧测节点延迟 |
| GET/PUT | `/api/admin/config` | 全局配置 |
| GET | `/api/admin/stats` | 运行统计 |

## 注意

- 管理面板统计的「累计中转」只统计**主服务器自身节点**的流量；各从节点在各自进程内独立统计。
- 服务默认监听 `:20808`（主）/ `:20809`（从），未占用 80/443。
- 换密钥后需同步更新从节点上 agent 的 `-secret` 并 `systemctl restart dltunnel-agent`。
