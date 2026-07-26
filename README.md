# mini-docker

A container runtime from scratch in ~600 lines of Go — built to understand what Docker does under the hood.

## What it does

Pulls OCI images from Docker Hub and runs them in a real container with:

- **Namespace isolation** — UTS, PID, mount, network, user (with UID/GID 0–65536 mapping)
- **cgroup v2** — CPU (1 core) and memory (128 MB) limits
- **OverlayFS** — layered root filesystem from pulled image layers
- **Bridge networking** — `mdbr0` bridge, veth pairs, NAT (MASQUERADE), inter-container communication
- **Image cache** — layers cached under `/tmp/layers/`; second run is instant

## Build

```bash
# Local macOS → Linux server
GOOS=linux GOARCH=amd64 go build -o mini-docker .
scp mini-docker your-server:
```

## Usage

```bash
sudo ./mini-docker run <image>[:tag] [--no-entry] [command...]
```

The first `run` pulls the image; subsequent runs use the cache.

### Examples

```bash
# Interactive shell
sudo ./mini-docker run alpine /bin/sh

# Python
sudo ./mini-docker run python:3.11 python3 -c "import sys; print(sys.version)"

# Nginx (skip docker-entrypoint.sh to avoid daemonizing)
sudo ./mini-docker run nginx nginx -g "daemon off;"

# Redis — uses its entrypoint script
sudo ./mini-docker run redis redis-server

# Bypass entrypoint for debugging
sudo ./mini-docker run redis --no-entry /bin/ls
```

## Architecture

```
main.go    — container lifecycle: namespaces, cgroups, networking, overlay mount
pull.go    — Docker Hub client: auth, manifest parsing, layer download & extraction
```

### How `run` works

1. **Pull** the image if not cached → OCI manifest → config → layer blobs → tar.gz extracted to `/tmp/layers/<repo>/<tag>/<digest>/`
2. **Fork** itself with `CLONE_NEWUTS | CLONE_NEWPID | CLONE_NEWNS | CLONE_NEWNET | CLONE_NEWUSER`
3. **Parent** sets up cgroup limits, creates veth pair, assigns IP, does cleanup on exit
4. **Child** mounts OverlayFS (`lowerdir` = all image layers), chroots into it, mounts `/proc`, execs the command

### Networking

```
container (10.100.0.x) ←→ veth pair ←→ mdbr0 bridge (10.100.0.1) ←→ NAT → internet
```

## Prerequisites

- Linux host with root access
- Kernel with cgroup v2 (`/sys/fs/cgroup/` mounted as cgroup2)
- `iptables` (for NAT)
- No Docker required

## Limits

| What | Status |
|---|---|
| **alpine** | Works |
| **python:3.11** | Works |
| **nginx** | Works |
| **redis** | ❌ Known issue — one empty 32-byte layer blob 404s from Docker Hub. The manifest references it but the registry returns 404 for our HTTP client. Docker pulls it fine (different client behavior). Skipping it is the fix but needs debugging. |
| Filesystem | Read-only lower layers + ephemeral writable upper layer (lost on exit) |
| Port publishing | Not yet — containers have IPs, but no iptables DNAT |
| Multi-container | Each `run` gets its own IP on `mdbr0`, they can talk to each other |
| Security | Not production-safe. The user namespace blocks device escape by default (`mknod` denied for unprivileged UID), and `safeRemoveAll` protects against follow-through-deletion on mount boundaries. Still: run as root, full capabilities in container. |

## File layout

```
mini-docker          # single binary
/tmp/layers/         # image cache (survives reboots on tmpfs? nope. Cache in /var)
/tmp/maingo-<PID>/   # per-container overlay workdirs (auto-cleaned)
```

## Next

- **端口发布** — iptables DNAT，`-p 8080:80` 把容器端口暴露到宿主机
- **卷挂载** — `-v /host/path:/container/path`，bind mount 进容器
- **docker-compose** — 解析一个简化的 `compose.yml`，起多个容器 + 共享网络

## Why

Docker internals are not magic — they're a careful assembly of Linux primitives. `mini-docker` is about touching each one:

- `unshare(CLONE_NEW*)` — what `docker run --pid=host` means
- `mount("overlay", ...)` — why images are layers, not snapshots  
- cgroup files in `/sys/fs/cgroup/` — CPU shares, memory limits, OOM kills
- veth pairs + bridge + iptables MASQUERADE — the same recipe Docker's `docker0` bridge uses
- Docker Hub's OCI API — manifest lists, manifests, config blobs, layer blobs — no SDK, raw HTTP
