package main

import (
	"context"
	"log"
	"net"
	"os"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/joho/godotenv"
	"github.com/redis/go-redis/v9"
)

type session struct {
	backend  *net.UDPConn
	lastSeen atomic.Int64
}

type pkt struct {
	addr *net.UDPAddr
	data []byte
}

type proxy struct {
	conn         *net.UDPConn
	rdb          *redis.Client
	backendKey   string
	sessions     map[string]*session
	portToClient map[string]string
	sessionsMu   sync.RWMutex
	idleSecs     int
	queue        chan pkt
	bufPool      sync.Pool
}

func main() {
	if err := godotenv.Load(); err != nil {
		log.Println("no .env file found, using environment variables")
	}

	proxyAddr := env("PROXY_ADDR", "0.0.0.0:19132")
	redisAddr := env("REDIS_ADDR", "127.0.0.1:6379")
	redisPass := env("REDIS_PASSWORD", "")
	redisDB := envInt("REDIS_DB", 0)
	backendKey := env("REDIS_BACKEND_KEY", "udp_gate:backend")
	disconnectChannel := env("REDIS_DISCONNECT_CHANNEL", "udp_gate:disconnect")
	idleSecs := envInt("SESSION_IDLE_TIMEOUT_SECS", 60)
	workers := envInt("WORKER_COUNT", runtime.GOMAXPROCS(0)*2)
	queueSize := envInt("QUEUE_SIZE", 4096)

	laddr, err := net.ResolveUDPAddr("udp", proxyAddr)
	must(err, "resolve listen addr")
	conn, err := net.ListenUDP("udp", laddr)
	must(err, "listen UDP")
	log.Printf("UDP proxy listening on %s (%d workers, queue %d)", proxyAddr, workers, queueSize)

	rdb := redis.NewClient(&redis.Options{
		Addr:     redisAddr,
		Password: redisPass,
		DB:       redisDB,
	})
	ctx := context.Background()
	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Fatalf("redis ping failed: %v", err)
	}
	log.Printf("connected to Redis at %s", redisAddr)

	p := &proxy{
		conn:         conn,
		rdb:          rdb,
		backendKey:   backendKey,
		sessions:     make(map[string]*session),
		portToClient: make(map[string]string),
		idleSecs:   idleSecs,
		queue:      make(chan pkt, queueSize),
		bufPool: sync.Pool{
			New: func() any {
				b := make([]byte, 64*1024)
				return &b
			},
		},
	}

	for i := 0; i < workers; i++ {
		go p.worker(ctx)
	}
	go p.subscribeDisconnects(ctx, disconnectChannel)
	go p.reapIdleSessions()
	p.serve()
}

func (p *proxy) serve() {
	for {
		bufp := p.bufPool.Get().(*[]byte)
		n, addr, err := p.conn.ReadFromUDP(*bufp)
		if err != nil {
			p.bufPool.Put(bufp)
			log.Printf("read error: %v", err)
			continue
		}

		data := make([]byte, n)
		copy(data, (*bufp)[:n])
		p.bufPool.Put(bufp)

		select {
		case p.queue <- pkt{addr, data}:
		default:
			log.Printf("queue full, dropping packet from %s", addr)
		}
	}
}

func (p *proxy) worker(ctx context.Context) {
	for pk := range p.queue {
		p.handlePacket(ctx, pk.addr, pk.data)
	}
}

func (p *proxy) handlePacket(ctx context.Context, clientAddr *net.UDPAddr, data []byte) {
	key := clientAddr.String()

	p.sessionsMu.RLock()
	sess, ok := p.sessions[key]
	p.sessionsMu.RUnlock()

	if !ok {
		backendAddr, err := p.rdb.Get(ctx, p.backendKey).Result()
		if err != nil {
			log.Printf("no backend in Redis (%v), dropping packet from %s", err, key)
			return
		}

		raddr, err := net.ResolveUDPAddr("udp", backendAddr)
		if err != nil {
			log.Printf("bad backend addr %q: %v", backendAddr, err)
			return
		}

		upstream, err := net.DialUDP("udp", nil, raddr)
		if err != nil {
			log.Printf("dial backend %s: %v", backendAddr, err)
			return
		}

		sess = &session{backend: upstream}
		sess.lastSeen.Store(time.Now().UnixNano())

		p.sessionsMu.Lock()
		if existing, exists := p.sessions[key]; exists {
			upstream.Close()
			sess = existing
		} else {
			p.sessions[key] = sess
			port := strconv.Itoa(upstream.LocalAddr().(*net.UDPAddr).Port)
			p.portToClient[port] = key
			log.Printf("new session: %s → %s (backend port %s)", key, backendAddr, port)
			go p.forwardReplies(clientAddr, sess)
		}
		p.sessionsMu.Unlock()
	}

	sess.lastSeen.Store(time.Now().UnixNano())

	if _, err := sess.backend.Write(data); err != nil {
		log.Printf("write to backend for %s: %v", key, err)
		p.removeSession(key)
	}
}

func (p *proxy) forwardReplies(clientAddr *net.UDPAddr, sess *session) {
	buf := make([]byte, 64*1024)
	for {
		n, err := sess.backend.Read(buf)
		if err != nil {
			return
		}
		sess.lastSeen.Store(time.Now().UnixNano())
		if _, err := p.conn.WriteToUDP(buf[:n], clientAddr); err != nil {
			log.Printf("write to client %s: %v", clientAddr, err)
			return
		}
	}
}

func (p *proxy) subscribeDisconnects(ctx context.Context, channel string) {
	sub := p.rdb.Subscribe(ctx, channel)
	defer sub.Close()
	log.Printf("subscribed to Redis channel %q for disconnect events", channel)
	for msg := range sub.Channel() {
		port := msg.Payload
		p.sessionsMu.RLock()
		key, ok := p.portToClient[port]
		p.sessionsMu.RUnlock()
		if !ok {
			log.Printf("disconnect event for unknown port %s", port)
			continue
		}
		log.Printf("disconnect event for port %s (%s)", port, key)
		p.removeSession(key)
	}
}

func (p *proxy) reapIdleSessions() {
	ticker := time.NewTicker(time.Duration(p.idleSecs/2) * time.Second)
	removalTime := -time.Duration(p.idleSecs) * time.Second
	defer ticker.Stop()
	for range ticker.C {
		cutoffNano := time.Now().Add(removalTime).UnixNano()

		p.sessionsMu.RLock()
		var idle []string
		for key, sess := range p.sessions {
			if sess.lastSeen.Load() < cutoffNano {
				idle = append(idle, key)
			}
		}
		p.sessionsMu.RUnlock()

		for _, key := range idle {
			log.Printf("reaping idle session: %s", key)
			p.removeSession(key)
		}
	}
}

func (p *proxy) removeSession(key string) {
	p.sessionsMu.Lock()
	defer p.sessionsMu.Unlock()
	if sess, ok := p.sessions[key]; ok {
		port := strconv.Itoa(sess.backend.LocalAddr().(*net.UDPAddr).Port)
		delete(p.portToClient, port)
		sess.backend.Close()
		delete(p.sessions, key)
		log.Printf("session removed: %s", key)
	}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func must(err error, msg string) {
	if err != nil {
		log.Fatalf("%s: %v", msg, err)
	}
}
