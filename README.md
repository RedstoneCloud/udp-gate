# udp-gate

A lightweight UDP proxy written in Go. It sits on a public IP and forwards packets to backend instances, with routing decisions driven by Redis. Designed to front a fleet of game servers (e.g. WaterdogPE / Minecraft Bedrock) where a separate cloud system manages capacity and load distribution.

---

## How it works

```
                        ┌───────────────────────────────────┐
                        │              udp-gate             │
                        │                                   │
 Client A ──UDP──►  ────┤  read loop                        │
 Client B ──UDP──►  ────┤    │                              │
 Client C ──UDP──►  ────┤    ▼                              │
                        │  queue (buffered channel)         │
                        │    │                              │
                        │    ▼                              │
                        │  worker pool                      │
                        │    │                              │
                        │    ├─ existing session? ──────────┼──► backend A (instance-1)
                        │    │                              │
                        │    └─ new client? ──► Redis GET ──┼──► backend B (instance-2)
                        │                                   │
                        └───────────────────────────────────┘
                                        │
                                   Redis pub/sub
                                        │
                               Cloud system signals
                               disconnect events
```

### Packet flow (client → backend)

1. A single goroutine calls `ReadFromUDP` in a tight loop, pulling packets off the OS socket buffer as fast as possible.
2. The read buffer is recycled via `sync.Pool` to avoid per-packet heap allocations.
3. The packet (address + payload copy) is pushed onto a bounded channel queue.
4. A fixed pool of worker goroutines (`WORKER_COUNT`, default `GOMAXPROCS×2`) drain the queue and call `handlePacket`.

### Session routing

- **Existing session (fast path):** a read lock on the session map returns the already-dialed `*net.UDPConn` for that client. `lastSeen` is updated atomically. The packet is written to the backend.
- **New client (slow path):** the worker does `GET <REDIS_BACKEND_KEY>` to find the current best backend, dials a new UDP connection to it, and registers the session. A double-check under a write lock prevents races when two workers see the same new client simultaneously.

### Reply flow (backend → client)

Each session spawns a dedicated `forwardReplies` goroutine. It blocks on `Read` from the backend socket and writes each reply back to the original client address via the shared listen socket. This goroutine exits automatically when the backend connection is closed.

### Session cleanup

Two mechanisms remove sessions:

| Mechanism | How |
|---|---|
| **Redis pub/sub** | Cloud publishes `clientIP:port` to `REDIS_DISCONNECT_CHANNEL`. The proxy removes the session immediately. |
| **Idle timeout** | A background ticker scans for sessions with no traffic in `SESSION_IDLE_TIMEOUT_SECS`. Idle keys are collected under a read lock, then removed individually to keep write lock duration minimal. |

---

## Redis contract

The proxy expects a cloud/orchestration system to maintain two things in Redis:

### Backend key

```
SET udp_gate:backend "10.0.0.5:19132"
```

Read once per new client connection. The cloud system should update this key whenever it wants new connections routed to a different backend (e.g. after scaling up a new instance or when an existing one crosses a capacity threshold).

Existing sessions are **not** affected by changes to this key - they stay pinned to whichever backend they were assigned at connection time.

### Disconnect channel

```
PUBLISH udp_gate:disconnect "1.2.3.4:54321"
```

The proxy subscribes to this channel on startup. The message payload must be the exact `IP:port` string of the client to disconnect (as it appears in UDP source address).

---

## Configuration

All configuration is via environment variables. Copy `.env.example` to `.env` and adjust as needed.

```bash
cp .env.example .env
```

| Variable | Default | Description |
|---|---|---|
| `PROXY_ADDR` | `0.0.0.0:19132` | Address the proxy listens on |
| `REDIS_ADDR` | `127.0.0.1:6379` | Redis server address |
| `REDIS_PASSWORD` | _(empty)_ | Redis password |
| `REDIS_DB` | `0` | Redis database index |
| `REDIS_BACKEND_KEY` | `udp_gate:backend` | Redis key holding the current backend address |
| `REDIS_DISCONNECT_CHANNEL` | `udp_gate:disconnect` | Redis pub/sub channel for disconnect events |
| `SESSION_IDLE_TIMEOUT_SECS` | `60` | Seconds of inactivity before a session is reaped |
| `WORKER_COUNT` | `GOMAXPROCS×2` | Number of packet-processing workers |
| `QUEUE_SIZE` | `4096` | Inbound packet queue depth; packets are dropped when full |

---

## Building and running

```bash
go build -o udp-gate .
./udp-gate
```

The proxy loads `.env` automatically on startup if it exists. Environment variables always take precedence.

---

## Design notes

**Why a single read loop?**
UDP sockets on most platforms do not support multiple concurrent readers without `SO_REUSEPORT`. A single reader feeding a channel is the idiomatic Go approach and keeps socket reads serialized while processing stays parallel.

**Why a worker pool instead of goroutine-per-packet?**
Spawning a goroutine per packet means creating and destroying tens of thousands of goroutines per second under load. A fixed pool amortizes the scheduling overhead and gives predictable memory usage.

**Session stickiness**
Once a client is assigned to a backend, they stay there for the lifetime of the session regardless of what Redis says. This is intentional - mid-session re-routing would break stateful protocols. To move a client, publish a disconnect event; they will be reassigned on reconnect.
