# Surfshark VPN Proxy Gateway

A multi-exit proxy gateway backed by Surfshark OpenVPN. A single container exposes both **SOCKS5** and **HTTP** proxies, and every concurrent request can be dispatched to a different country / different IP OpenVPN tunnel. Supports DataImpulse-style URL parameters to control country, sticky sessions, and rotating exits.

> 中文文档：[README_CN.md](./README_CN.md)

---

## Features

- **Multi-tunnel in a single container**: each worker runs its own `openvpn` process inside an isolated Linux network namespace — tunnels don't interfere with each other
- **SOCKS5 + HTTP on separate ports**: 1080 (SOCKS5) and 8888 (HTTP/CONNECT), usable simultaneously with the same credentials
- **Country routing**: `cr.us` in the username gives a US exit, `cr.jp` a Japan one, etc.
- **Sticky sessions**: requests with the same `sessid` land on the same worker; exit IP stays stable for the session TTL
- **Rotating exits**: when no `sessid` is given, requests round-robin across all ready workers
- **Pool warming**: the worker pool is automatically grown to `MIN_POOL_SIZE` at startup — rotation works out of the box
- **Forced rotation**: `WORKER_MAX_LIFETIME` drains and replaces expired workers so exit IPs are periodically refreshed
- **Idle reaping**: workers with no traffic are reclaimed to free resources

---

## Quick Start

### 1. Get your Surfshark credentials

Grab your **service credentials** (NOT your account login) from [Surfshark Manual Setup](https://my.surfshark.com/vpn/manual-setup/main).

Create `auth.txt` at the project root:

```
<service_username>
<service_password>
```

### 2. Download OpenVPN configs

Download `.ovpn` files from [Surfshark's config list](https://my.surfshark.com/vpn/api/v1/server/configurations) or the official download page, then drop them into `ovpn/` at the project root:

```
ovpn/
├── us-nyc.prod.surfshark.com_udp.ovpn
├── us-nyc.prod.surfshark.com_tcp.ovpn
├── jp-tyo.prod.surfshark.com_udp.ovpn
├── de-fra.prod.surfshark.com_udp.ovpn
└── ...
```

The first two letters of each filename are treated as the country code (`us`, `jp`, `de`, ...).

### 3. Launch

```bash
docker compose up -d --build
docker compose logs -f
```

On a successful start you'll see:

```
发现 100 个国家，共 284 个服务器
HTTP 代理已监听 :8888
SOCKS5 代理已监听 :1080
worker worker-0 已创建 [us/us-nyc]，当前共 1 个 worker
...
```

### 4. Test

```bash
# SOCKS5 rotating (random exit)
curl -x socks5://user:pass@127.0.0.1:1080 https://ipinfo.io/ip

# HTTP, pinned to a US exit
curl -x 'http://user__cr.us:pass@127.0.0.1:8888' https://ipinfo.io

# SOCKS5 sticky: same sessid → same exit IP
curl -x 'socks5://user__sessid.abc:pass@127.0.0.1:1080' https://ipinfo.io/ip
curl -x 'socks5://user__sessid.abc:pass@127.0.0.1:1080' https://ipinfo.io/ip
```

> In zsh/bash the `;` inside usernames is treated as a shell separator. Wrap the whole proxy URL in single quotes.

---

## Username parameters

Parameters go after `__` in the username, separated by `;`, each formatted as `key.value`:

```
user__cr.us;sessid.abc;sessttl.15
```

| Parameter | Description | Example |
|---|---|---|
| `cr.<country>` | Country code, lowercase, matching the `.ovpn` filename prefix | `cr.us`, `cr.jp`, `cr.tw` |
| `sessid.<id>` | Sticky session ID. All requests with the same id are pinned to the same worker | `sessid.abc` |
| `sessttl.<minutes>` | Session TTL in minutes; refreshed on every hit | `sessttl.30` |

Combinations:

| Scenario | Username |
|---|---|
| Pure rotation | `user` |
| Rotation within a country | `user__cr.us` |
| Sticky session | `user__sessid.abc` |
| Country + sticky + TTL | `user__cr.us;sessid.abc;sessttl.60` |

The **password** is always `PROXY_PASS`.

---

## Configuration

All tuned via environment variables (or the `environment:` block in `docker-compose.yml`):

| Variable | Default | Meaning |
|---|---|---|
| `PROXY_USER` | `user` | Proxy login username (the part before `__`) |
| `PROXY_PASS` | `pass` | Proxy login password |
| `SOCKS5_PORT` | `1080` | SOCKS5 listening port |
| `HTTP_PORT` | `8888` | HTTP proxy listening port |
| `MIN_POOL_SIZE` | `10` | Warm-up target; the pool is auto-grown whenever it drops below this |
| `DEFAULT_SESSION_TTL` | `30` | Default sticky-session TTL in minutes when `sessttl` is not set |
| `WORKER_IDLE_TIMEOUT` | `10` | Reap a worker after this many minutes of no traffic and no sessions |
| `WORKER_MAX_LIFETIME` | `60` | Max worker lifetime in minutes; on expiry the worker drains and is replaced with a fresh IP (0 = disabled) |
| `WORKER_VERBOSE` | `false` | Enable full OpenVPN stdout/stderr and netns diagnostics for troubleshooting |

---

## Architecture

```
                              ┌─────── Linux default netns ───────┐
           SOCKS5 :1080        │                                    │
  client ──▶ HTTP   :8888 ──▶ gateway ──┬──▶ veth0 ──▶ netns worker-0 ──▶ openvpn ──▶ Surfshark
                                        ├──▶ veth1 ──▶ netns worker-1 ──▶ openvpn ──▶ Surfshark
                                        ├──▶ ...
                                        └──▶ vethN ──▶ netns worker-N ──▶ openvpn ──▶ Surfshark
                              └────────────────────────────────────┘
```

- Each worker = dedicated netns + veth pair + openvpn process + its own default route + kill-switch
- The router picks a worker based on the username parameters; `NsDialer` switches into the target netns to open the TCP connection, then switches back
- Health check runs every 30 s: reap workers that exited, timed out idle, or hit max lifetime
- A pool-warmer goroutine watches for reaping events and serially creates replacements up to `MIN_POOL_SIZE`

---

## Container permissions

Docker's default AppArmor/seccomp profiles block the `mount(2)` / `unshare(CLONE_NEWNET)` calls required to create network namespaces. The provided `docker-compose.yml` is already set up — keep these entries:

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

## License

This project is licensed under the [MIT License](./LICENSE).

## Disclaimer

This project is not affiliated with, endorsed by, or sponsored by Surfshark. Before using it, make sure:

- You hold a valid Surfshark subscription
- Your usage complies with the [Surfshark Terms of Service](https://surfshark.com/terms-of-service) and all applicable laws
- You do not use it for scraping, attacks, or other abusive purposes

The authors accept no responsibility for any consequences arising from use of this project.
