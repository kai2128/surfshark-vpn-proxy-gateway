# Surfshark VPN Proxy Gateway — Design Spec

## 概述

单 Docker 容器的 VPN 代理网关，使用 Go 实现。通过 Surfshark OpenVPN 连接提供 SOCKS5 和 HTTP 代理，支持 DataImpulse 风格的用户名参数控制 sticky/rotating 行为。

## 架构

```
客户端 (curl/浏览器/爬虫)
    │
    │  SOCKS5 (:1080) 或 HTTP (:8888)
    │  用户名: user__cr.us;sessid.abc;sessttl.30:pass
    ▼
┌──────────────────────────────────────┐
│  单个 Docker 容器 (NET_ADMIN)        │
│                                      │
│  Go Proxy Gateway (主进程, PID 1)    │
│  ├── SOCKS5 Server (:1080)           │
│  ├── HTTP Proxy Server (:8888)       │
│  ├── Username Parser                 │
│  ├── Session Manager (内存)          │
│  ├── Worker Manager                  │
│  │    ├── Worker 0 (netns-0)         │
│  │    │   └── OpenVPN → us-dal       │
│  │    ├── Worker 1 (netns-1)         │
│  │    │   └── OpenVPN → jp-tok       │
│  │    └── Worker N (netns-N)         │
│  │        └── OpenVPN → de-fra       │
│  └── Server Discovery                │
│       └── 扫描 /etc/openvpn/ovpn/    │
└──────────────────────────────────────┘
```

## 组件详情

### 1. Server Discovery

- 启动时扫描挂载的 `.ovpn` 文件目录（`/etc/openvpn/ovpn/`）
- 从文件名解析出国家代码和服务器标识
  - 例: `us-dal.prod.surfshark.com_udp.ovpn` → 国家 `us`，服务器 `us-dal`
- 构建可用服务器列表：`map[country][]ServerConfig`
- 支持一个国家有多个服务器（多个 .ovpn 文件）

### 2. Worker Manager

每个 worker 代表一个活跃的 VPN 连接。

**创建流程**：
1. 创建 Linux 网络命名空间 (`ip netns add worker-N`)
2. 创建 veth pair，一端在主 netns，一端在 worker netns
3. 在 worker netns 中启动 OpenVPN 子进程
4. 等待 tun 设备就绪（OpenVPN 连接建立）
5. 标记 worker 为可用

**生命周期管理**：
- 按需创建：首次请求某国家时创建 worker
- 健康检查：定期检查 OpenVPN 进程存活 + tun 设备状态
- 空闲回收：无活跃连接且无 sticky session 指向超过 10 分钟后关闭
- 异常处理：OpenVPN 进程退出 → 清理 netns → 清理关联 session

**状态**：`creating` → `ready` → `idle` → `closing`

### 3. Session Manager

内存中维护 sticky session 映射。

**数据结构**：
```go
type Session struct {
    ID        string        // sessid 值
    WorkerID  string        // 绑定的 worker
    Country   string        // 绑定的国家
    TTL       time.Duration // 过期时间，默认 30 分钟
    CreatedAt time.Time
    LastUsed  time.Time
}
```

**行为**：
- `sessid` 存在且未过期 → 路由到绑定的 worker
- `sessid` 存在但已过期 → 删除映射，分配新 worker
- `sessid` 不存在 → 创建新映射
- Worker 掉线 → 删除所有指向该 worker 的 session
- 定时清理过期 session（每分钟扫描一次）

### 4. Username Parser

解析 DataImpulse 风格的用户名参数。

**格式**：
```
{username}__{param1}.{value1};{param2}.{value2}:{password}
```

**支持的参数**：
| 参数 | 说明 | 默认值 |
|------|------|--------|
| `cr` | 国家代码 | 无（随机选择） |
| `sessid` | Sticky session ID | 无（rotating 模式） |
| `sessttl` | Session 过期时间（分钟） | 30 |

**解析示例**：
```
user__cr.us;sessid.abc123;sessttl.60:pass
→ username: "user"
→ password: "pass"
→ params: {country: "us", sessionID: "abc123", ttl: 60min}
```

### 5. 路由逻辑

```
请求到达
  │
  ├─ 认证检查 → 失败 → 拒绝
  │
  ├─ 解析用户名参数
  │
  ├─ 有 sessid？
  │   ├─ 是 → session 存在且未过期？
  │   │        ├─ 是 → 使用绑定的 worker
  │   │        └─ 否 → 选择/创建 worker → 创建新 session
  │   └─ 否 → Rotating 模式
  │            └─ Round-robin 选择可用 worker
  │
  ├─ 有 cr？
  │   ├─ 是 → 只在该国家的 worker 中选择
  │   │        └─ 没有可用 worker → 按需创建
  │   └─ 否 → 所有 worker 中选择
  │
  └─ 通过选中 worker 的 netns 转发流量
```

### 6. 流量转发

Go 主进程在主 netns 接收客户端连接，选择目标 worker 后：

1. 通过 `veth pair` 进入目标 worker 的 netns
2. 在 worker netns 中建立到目标服务器的 TCP 连接
3. 双向 pipe 数据（客户端 ↔ 目标服务器）

实现方式：使用 Go 的 `syscall` 包操作网络命名空间，或者通过 veth pair 的 IP 地址做 SOCKS5 代理链。

### 7. SOCKS5 Server

- 监听 `:1080`
- 支持 SOCKS5 认证（RFC 1928 + RFC 1929）
- 从认证阶段提取用户名参数
- 支持 CONNECT 命令（TCP 代理）

### 8. HTTP Proxy Server

- 监听 `:8888`
- 支持 HTTP CONNECT 方法（HTTPS 隧道）
- 支持普通 HTTP 代理（非 CONNECT）
- 从 Proxy-Authorization header 提取用户名参数

## 配置

### 环境变量

| 变量 | 说明 | 默认值 |
|------|------|--------|
| `SOCKS5_PORT` | SOCKS5 监听端口 | `1080` |
| `HTTP_PORT` | HTTP 代理监听端口 | `8888` |
| `PROXY_USER` | 代理认证用户名 | `user` |
| `PROXY_PASS` | 代理认证密码 | `pass` |
| `OVPN_DIR` | .ovpn 文件目录 | `/etc/openvpn/ovpn` |
| `AUTH_FILE` | Surfshark 认证文件 | `/etc/openvpn/auth.txt` |
| `DEFAULT_SESSION_TTL` | 默认 session TTL（分钟） | `30` |
| `WORKER_IDLE_TIMEOUT` | Worker 空闲回收时间（分钟） | `10` |

### Docker 运行

```bash
docker build -t surfshark-proxy .
docker run -d \
  --cap-add=NET_ADMIN \
  --cap-add=SYS_ADMIN \
  --device=/dev/net/tun \
  -v ./ovpn:/etc/openvpn/ovpn:ro \
  -v ./auth.txt:/etc/openvpn/auth.txt:ro \
  -e PROXY_USER=myuser \
  -e PROXY_PASS=mypass \
  -p 1080:1080 \
  -p 8888:8888 \
  surfshark-proxy
```

> 需要 `SYS_ADMIN`：Go 代码通过 `setns(2)` 切换到 worker 命名空间拨号，
> 没有此 capability 会 `operation not permitted`。

### 使用示例

```bash
# Rotating，随机国家
curl -x socks5://myuser:mypass@localhost:1080 https://api.ipify.org

# Rotating，指定美国
curl -x socks5://myuser__cr.us:mypass@localhost:1080 https://api.ipify.org

# Sticky，日本节点，默认30分钟
curl -x http://myuser__cr.jp;sessid.abc123:mypass@localhost:8888 https://api.ipify.org

# Sticky，60分钟过期
curl -x http://myuser__cr.de;sessid.xyz;sessttl.60:mypass@localhost:8888 https://api.ipify.org
```

## Docker 镜像

- 基础镜像：`alpine:3.20`
- 安装：`openvpn`, `iproute2`（提供 ip netns）, `iptables`
- Go 二进制静态编译，多阶段构建
- 入口点：Go 二进制

## 错误处理

- OpenVPN 连接失败：重试 2 次，不同服务器（同国家）；全部失败返回代理错误
- 请求的国家无 .ovpn 文件：返回认证错误
- Worker 创建中的请求：等待 worker 就绪（最多 30 秒超时）
- 所有 worker 不可用：返回代理错误

## 不在范围内

- Web 管理界面 / REST API（后续可加）
- WireGuard 支持
- 多用户认证
- 持久化 session（重启后丢失）
- UDP 代理
