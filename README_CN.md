# Surfshark VPN Proxy Gateway

一个基于 Surfshark OpenVPN 的多出口代理网关。单容器同时对外提供 **SOCKS5** 和 **HTTP** 代理，每个并发请求可以按参数分配到不同国家/不同 IP 的 OpenVPN 隧道上。支持 DataImpulse 风格的 URL 参数控制国家、粘性会话与轮换出口。

> English version: [README.md](./README.md)

---

## 核心特性

- **单容器多隧道**：每个 worker 在独立的 Linux network namespace 里跑一个 `openvpn` 进程，互不影响
- **SOCKS5 + HTTP 双端口**：1080（SOCKS5）、8888（HTTP/CONNECT），同一份账号同时可用
- **按国家选路**：username 里带 `cr.us` 就出美国 IP，带 `cr.jp` 就出日本
- **粘性会话（Sticky）**：同一 `sessid` 命中同一 worker，TTL 内出口 IP 稳定
- **轮询出口（Rotating）**：不带 `sessid` 时自动 round-robin 多个 ready worker
- **池预热**：启动时自动把池子补齐到 `MIN_POOL_SIZE`，rotation 开箱即用
- **强制轮换**：`WORKER_MAX_LIFETIME` 到期后 worker 进入 draining，连接排空后被销毁并自动换新 IP
- **空闲回收**：长时间无请求的 worker 会被自动清理，释放资源

---

## 快速开始

### 1. 准备 Surfshark 凭据

从 Surfshark 官网 [Manual Setup](https://my.surfshark.com/vpn/manual-setup/main) 页面拿到你的 **服务凭据**（不是账号密码）。

在项目根目录建 `auth.txt`：

```
<service_username>
<service_password>
```

### 2. 下载 OpenVPN 配置

从 [Surfshark OpenVPN 配置列表](https://my.surfshark.com/vpn/api/v1/server/configurations) 或官方下载页取得 `.ovpn` 文件，放到项目根目录的 `ovpn/` 下：

```
ovpn/
├── us-nyc.prod.surfshark.com_udp.ovpn
├── us-nyc.prod.surfshark.com_tcp.ovpn
├── jp-tyo.prod.surfshark.com_udp.ovpn
├── de-fra.prod.surfshark.com_udp.ovpn
└── ...
```

文件名的前两位字母会被当成国家代码（如 `us`、`jp`、`de`）。

### 3. 启动

```bash
docker compose up -d --build
docker compose logs -f
```

启动成功后会看到：

```
发现 100 个国家，共 284 个服务器
HTTP 代理已监听 :8888
SOCKS5 代理已监听 :1080
worker worker-0 已创建 [us/us-nyc]，当前共 1 个 worker
...
```

### 4. 测试

```bash
# SOCKS5 rotating（出口随机）
curl -x socks5://user:pass@127.0.0.1:1080 https://ipinfo.io/ip

# HTTP，指定美国出口
curl -x 'http://user__cr.us:pass@127.0.0.1:8888' https://ipinfo.io

# SOCKS5 粘性会话：同 sessid 始终出同一个 IP
curl -x 'socks5://user__sessid.abc:pass@127.0.0.1:1080' https://ipinfo.io/ip
curl -x 'socks5://user__sessid.abc:pass@127.0.0.1:1080' https://ipinfo.io/ip
```

> zsh/bash 下 username 里的 `;` 会被 shell 当分隔符，记得整个 URL 用单引号包起来。

---

## username 参数

所有参数都放在用户名的 `__` 后，用 `;` 分隔，每项格式 `key.value`：

```
user__cr.us;sessid.abc;sessttl.15
```

| 参数 | 说明 | 示例 |
|---|---|---|
| `cr.<country>` | 国家代码，小写，对应 `.ovpn` 文件名前缀 | `cr.us`、`cr.jp`、`cr.tw` |
| `sessid.<id>` | 粘性会话 ID。同一个 id 的请求固定落到同一个 worker | `sessid.abc` |
| `sessttl.<minutes>` | 会话 TTL（分钟），每次命中刷新过期 | `sessttl.30` |

组合：

| 场景 | username |
|---|---|
| 纯轮询 | `user` |
| 指定国家，轮询 | `user__cr.us` |
| 粘性会话 | `user__sessid.abc` |
| 指定国家 + 粘性 + TTL | `user__cr.us;sessid.abc;sessttl.60` |

**密码**固定用 `PROXY_PASS`。

---

## 配置项

全部通过环境变量或 `docker-compose.yml` 里的 `environment:` 调整：

| 变量 | 默认 | 含义 |
|---|---|---|
| `PROXY_USER` | `user` | 代理登录用户名（不含 `__` 参数部分） |
| `PROXY_PASS` | `pass` | 代理登录密码 |
| `SOCKS5_PORT` | `1080` | SOCKS5 监听端口 |
| `HTTP_PORT` | `8888` | HTTP 代理监听端口 |
| `MIN_POOL_SIZE` | `10` | 启动预热目标，池容量低于此值时后台自动补位 |
| `DEFAULT_SESSION_TTL` | `30` | 未指定 `sessttl` 时的粘性会话默认 TTL（分钟） |
| `WORKER_IDLE_TIMEOUT` | `10` | worker 无连接、无 session 持续多久后回收（分钟） |
| `WORKER_MAX_LIFETIME` | `60` | worker 最长存活时间，到期 draining 并换新 IP（分钟，0 = 关闭） |
| `WORKER_VERBOSE` | `false` | 打开后输出 OpenVPN 子进程全部日志与 netns 诊断，用于排障 |

---

## 架构简介

```
                              ┌─────── Linux default netns ───────┐
           SOCKS5 :1080        │                                    │
  client ──▶ HTTP   :8888 ──▶ gateway ──┬──▶ veth0 ──▶ netns worker-0 ──▶ openvpn ──▶ Surfshark
                                        ├──▶ veth1 ──▶ netns worker-1 ──▶ openvpn ──▶ Surfshark
                                        ├──▶ ...
                                        └──▶ vethN ──▶ netns worker-N ──▶ openvpn ──▶ Surfshark
                              └────────────────────────────────────┘
```

- 每个 worker = 独立 netns + 独立 veth pair + 独立 openvpn 进程 + 自己的默认路由 + kill-switch
- 路由器按 username 参数挑 worker，`NsDialer` 在切入目标 netns 后发起 TCP dial，再切回主 netns
- 健康检查每 30s 跑一次：回收进程已退出 / 空闲超时 / 达到最大寿命的 worker
- 池预热 goroutine 监听回收事件，按需串行补位到 `MIN_POOL_SIZE`

---

## 必需的容器权限

Docker 默认 AppArmor/seccomp 会拦住创建 netns 所需的 `mount(2)` / `unshare(CLONE_NEWNET)`。`docker-compose.yml` 已配置好以下项，保留即可：

```yaml
cap_add:
  - NET_ADMIN
  - SYS_ADMIN
security_opt:
  - apparmor:unconfined
  - seccomp:unconfined
devices:
  - /dev/net/tun
sysctls:
  net.ipv4.ip_forward: "1"
```

---

## 许可证

本项目采用 [MIT License](./LICENSE) 许可证。

## 免责声明

本项目与 Surfshark 无关，未获其授权或背书。使用前请确保：

- 你拥有合法的 Surfshark 订阅
- 使用方式符合 [Surfshark 服务条款](https://surfshark.com/terms-of-service) 及当地法律
- 切勿用于爬取、攻击或其他违规用途

仓库作者不对使用本项目产生的任何后果负责。
