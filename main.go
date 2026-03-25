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
	"golang.org/x/net/ipv4"
)

const bufSize = 64 * 1024

type session struct {
	backend  *net.UDPConn
	port     int
	lastSeen atomic.Int64
}

func (s *session) touch() {
	s.lastSeen.Store(time.Now().UnixNano())
}

type proxy struct {
	conn         *net.UDPConn
	rdb          *redis.Client
	backendKey   string
	sessions     map[string]*session
	portToClient map[int]string
	sessionsMu   sync.RWMutex
	idleTimeout  time.Duration
	batchSize    int
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
	idleTimeout := time.Duration(envInt("SESSION_IDLE_TIMEOUT_SECS", 60)) * time.Second
	listeners := envInt("LISTENER_COUNT", runtime.GOMAXPROCS(0))
	batchSize := envInt("BATCH_SIZE", 64)

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

	conns := make([]*net.UDPConn, listeners)
	for i := range conns {
		conn, err := listenUDP(proxyAddr)
		must(err, "listen UDP")
		conns[i] = conn
	}
	log.Printf("UDP proxy listening on %s (%d listeners, batch size %d)", proxyAddr, listeners, batchSize)

	p := &proxy{
		conn:         conns[0],
		rdb:          rdb,
		backendKey:   backendKey,
		sessions:     make(map[string]*session),
		portToClient: make(map[int]string),
		idleTimeout:  idleTimeout,
		batchSize:    batchSize,
	}

	for _, conn := range conns {
		go p.readLoop(ctx, conn)
	}
	go p.subscribeDisconnects(ctx, disconnectChannel)
	go p.reapIdleSessions()

	select {}
}

func (p *proxy) readLoop(ctx context.Context, conn *net.UDPConn) {
	pc := ipv4.NewPacketConn(conn)
	msgs := make([]ipv4.Message, p.batchSize)
	for i := range msgs {
		msgs[i].Buffers = [][]byte{make([]byte, bufSize)}
	}
	for {
		n, err := pc.ReadBatch(msgs, 0)
		if err != nil {
			log.Printf("read error: %v", err)
			continue
		}
		for i := 0; i < n; i++ {
			p.handlePacket(ctx, msgs[i].Addr.(*net.UDPAddr), msgs[i].Buffers[0][:msgs[i].N])
		}
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

		port := upstream.LocalAddr().(*net.UDPAddr).Port
		sess = &session{backend: upstream, port: port}

		p.sessionsMu.Lock()
		if existing, exists := p.sessions[key]; exists {
			upstream.Close()
			sess = existing
		} else {
			p.sessions[key] = sess
			p.portToClient[port] = key
			log.Printf("new session: %s → %s (backend port %d)", key, backendAddr, port)
			go p.forwardReplies(clientAddr, sess)
		}
		p.sessionsMu.Unlock()
	}

	sess.touch()

	if _, err := sess.backend.Write(data); err != nil {
		log.Printf("write to backend for %s: %v", key, err)
		p.removeSession(key)
	}
}

func (p *proxy) forwardReplies(clientAddr *net.UDPAddr, sess *session) {
	defer p.removeSession(clientAddr.String())
	buf := make([]byte, bufSize)
	for {
		n, err := sess.backend.Read(buf)
		if err != nil {
			return
		}
		sess.touch()
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
		port, err := strconv.Atoi(msg.Payload)
		if err != nil {
			log.Printf("disconnect event with invalid port %q", msg.Payload)
			continue
		}
		p.sessionsMu.RLock()
		key, ok := p.portToClient[port]
		p.sessionsMu.RUnlock()
		if !ok {
			log.Printf("disconnect event for unknown port %d", port)
			continue
		}
		log.Printf("disconnect event for port %d (%s)", port, key)
		p.removeSession(key)
	}
}

func (p *proxy) reapIdleSessions() {
	ticker := time.NewTicker(p.idleTimeout / 2)
	defer ticker.Stop()
	for range ticker.C {
		cutoffNano := time.Now().Add(-p.idleTimeout).UnixNano()

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
		delete(p.portToClient, sess.port)
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
