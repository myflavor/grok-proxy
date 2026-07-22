// grok-proxy: 多账号 Grok 反向代理
//
// 从目录加载 CPA JSON，自动轮换、自动刷新、429 自动切号。
// 对客户端始终是一个端点一个 api_key。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ---------- 硬编码的 grok 常量 ----------

const (
	defaultTokenURL  = "https://auth.x.ai/oauth2/token"
	defaultClientID  = "b1a00492-073a-47ea-816f-4c329264a828"
	defaultScope     = "openid profile email offline_access grok-cli:access api:access"
	defaultBaseURL   = "https://cli-chat-proxy.grok.com/v1"
	defaultCoolSec   = 65
)

// ---------- 配置 ----------

type Config struct {
	Listen          string `json:"listen"`
	APIKey          string `json:"api_key"`
	CPADir          string `json:"cpa_dir"`
	RefreshInterval int    `json:"refresh_interval"` // 秒
}

// ---------- 账号凭证 ----------

type Account struct {
	Email        string            `json:"email"`
	AccessToken  string            `json:"access_token"`
	RefreshToken string            `json:"refresh_token"`
	ExpiresAt    time.Time         `json:"-"`
	RawExpired   string            `json:"expired"`
	Headers      map[string]string `json:"headers"`

	mu            sync.Mutex
	dead          bool
	filePath      string    // 对应的 CPA JSON 文件路径
	cooldownUntil time.Time // 429 冷却截止时间
}

func (a *Account) valid() bool {
	return a.AccessToken != "" && a.RefreshToken != ""
}

func (a *Account) inCooldown() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return time.Now().Before(a.cooldownUntil)
}

func (a *Account) isDead() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.dead
}

func (a *Account) markDead() {
	a.mu.Lock()
	a.dead = true
	a.mu.Unlock()
	// 改后缀为 .json.dead，避免下次加载
	newPath := a.filePath + ".dead"
	if err := os.Rename(a.filePath, newPath); err != nil {
		log.Printf("[dead] rename %s -> %s failed: %v", a.filePath, newPath, err)
	} else {
		log.Printf("[dead] %s → %s.dead", a.Email, filepath.Base(a.filePath))
	}
}

// ---------- 账号池 ----------

type Pool struct {
	accounts []*Account
	counter  atomic.Uint64
	dir      string
	mu       sync.RWMutex
}

func (p *Pool) load() error {
	matches, err := filepath.Glob(p.dir)
	if err != nil {
		return fmt.Errorf("glob %s: %w", p.dir, err)
	}

	var list []*Account
	seen := map[string]bool{}

	for _, path := range matches {
		if !strings.HasSuffix(strings.ToLower(path), ".json") {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			log.Printf("[load] skip %s: %v", path, err)
			continue
		}
		var acct Account
		if err := json.Unmarshal(data, &acct); err != nil {
			log.Printf("[load] skip %s: %v", path, err)
			continue
		}
		if !acct.valid() {
			log.Printf("[load] skip %s: missing tokens", path)
			continue
		}
		if seen[acct.Email] {
			continue
		}
		seen[acct.Email] = true

		acct.filePath = path

		if acct.RawExpired != "" {
			if t, err := time.Parse(time.RFC3339, acct.RawExpired); err == nil {
				acct.ExpiresAt = t
			} else {
				acct.ExpiresAt = time.Now()
			}
		} else {
			acct.ExpiresAt = time.Now()
		}

		list = append(list, &acct)
		log.Printf("[load] %s (expires %s)", acct.Email, acct.ExpiresAt.Format(time.RFC3339))
	}

	if len(list) == 0 {
		return fmt.Errorf("no valid CPA JSON found in %s", p.dir)
	}

	p.mu.Lock()
	p.accounts = list
	p.mu.Unlock()

	log.Printf("[pool] loaded %d accounts", len(list))
	return nil
}

func (p *Pool) selectAccount() *Account {
	p.mu.RLock()
	n := len(p.accounts)
	p.mu.RUnlock()
	if n == 0 {
		return nil
	}

	start := int(p.counter.Add(1) % uint64(n))

	// 找第一个未死亡 + 未冷却 + 未过期的
	for i := 0; i < n; i++ {
		idx := (start + i) % n
		p.mu.RLock()
		a := p.accounts[idx]
		p.mu.RUnlock()

		if a.isDead() || a.inCooldown() || time.Now().After(a.ExpiresAt) {
			continue
		}
		return a
	}

	// 都冷却了，返回第一个未死亡能用的（强制）
	for i := 0; i < n; i++ {
		idx := (start + i) % n
		p.mu.RLock()
		a := p.accounts[idx]
		p.mu.RUnlock()
		if a.isDead() {
			continue
		}
		if !a.inCooldown() {
			return a
		}
	}

	// 全凉了，返回第一个未死亡的
	for i := 0; i < n; i++ {
		idx := (start + i) % n
		p.mu.RLock()
		a := p.accounts[idx]
		p.mu.RUnlock()
		if !a.isDead() {
			return a
		}
	}
	return nil
}

func (p *Pool) cooldown(email string, sec int) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, a := range p.accounts {
		if a.Email == email {
			a.mu.Lock()
			a.cooldownUntil = time.Now().Add(time.Duration(sec) * time.Second)
			a.mu.Unlock()
			log.Printf("[cooldown] %s %ds", email, sec)
			return
		}
	}
}

func (p *Pool) refreshAll(client *http.Client, scope string) {
	p.mu.RLock()
	accounts := p.accounts
	p.mu.RUnlock()

	for _, a := range accounts {
		if a.isDead() {
			continue
		}
		a.mu.Lock()
		need := a.ExpiresAt.Before(time.Now().Add(5 * time.Minute)) || a.ExpiresAt.Before(time.Now())
		a.mu.Unlock()
		if !need {
			continue
		}
		if err := refreshAccount(client, a, scope); err != nil {
			log.Printf("[refresh] %s failed: %v", a.Email, err)
			// invalid_grant / revoked → 标记死亡，改后缀 .dead
			errStr := err.Error()
			if strings.Contains(errStr, "invalid_grant") ||
				strings.Contains(errStr, "revoked") ||
				strings.Contains(errStr, "Token has been revoked") {
				a.markDead()
			}
		} else {
			log.Printf("[refresh] %s ok, expires %s", a.Email, a.ExpiresAt.Format(time.RFC3339))
		}
	}
}

func refreshAccount(client *http.Client, a *Account, scope string) error {
	a.mu.Lock()
	rt := a.RefreshToken
	a.mu.Unlock()
	if rt == "" {
		return fmt.Errorf("no refresh_token")
	}

	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {rt},
		"client_id":     {defaultClientID},
		"scope":         {scope},
	}
	req, err := http.NewRequest(http.MethodPost, defaultTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
	}

	var tr struct {
		AccessToken  string `json:"access_token"`
		ExpiresIn    int    `json:"expires_in"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(body, &tr); err != nil {
		return fmt.Errorf("parse: %w", err)
	}
	if tr.AccessToken == "" {
		return fmt.Errorf("no access_token")
	}

	ttl := tr.ExpiresIn
	if ttl <= 0 {
		ttl = 21600
	}

	a.mu.Lock()
	a.AccessToken = tr.AccessToken
	a.ExpiresAt = time.Now().Add(time.Duration(ttl) * time.Second)
	if tr.RefreshToken != "" && tr.RefreshToken != a.RefreshToken {
		a.RefreshToken = tr.RefreshToken
	}
	a.cooldownUntil = time.Time{}
	a.mu.Unlock()
	return nil
}

// ---------- Server ----------

type Server struct {
	config Config
	pool   *Pool
	client *http.Client
	proxy  *httputil.ReverseProxy
}

func NewServer(cfg Config) (*Server, error) {
	pool := &Pool{dir: cfg.CPADir}
	if err := pool.load(); err != nil {
		return nil, err
	}

	target, _ := url.Parse(defaultBaseURL)
	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			// 透传路径和 host
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.Host = target.Host

			// 选账号
			acct := pool.selectAccount()
			if acct == nil {
				req.Header.Set("X-Proxy-Error", "no account available")
				return
			}

			acct.mu.Lock()
			tok := acct.AccessToken
			hdr := acct.Headers
			email := acct.Email
			acct.mu.Unlock()

			if tok == "" {
				req.Header.Set("X-Proxy-Error", "no token")
				return
			}

			req.Header.Set("Authorization", "Bearer "+tok)
			for k, v := range hdr {
				req.Header.Set(k, v)
			}
			req.Header.Set("X-Proxy-Account", email)
		},
		Transport: &roundTripper{
			pool:      pool,
			scope:     defaultScope,
			client:    &http.Client{Timeout: 60 * time.Second},
			transport: http.DefaultTransport,
		},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("[proxy] %v", err)
			writeJSON(w, 502, map[string]any{"error": err.Error()})
		},
	}

	s := &Server{
		config: cfg,
		pool:   pool,
		client: &http.Client{Timeout: 60 * time.Second},
		proxy:  proxy,
	}
	return s, nil
}

func (s *Server) refreshLoop(ctx context.Context) {
	interval := s.config.RefreshInterval
	if interval <= 0 {
		interval = 300
	}
	ticker := time.NewTicker(time.Duration(interval) * time.Second)
	defer ticker.Stop()

	s.pool.refreshAll(s.client, defaultScope)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.pool.refreshAll(s.client, defaultScope)
		}
	}
}

// ---------- Transport ----------

type roundTripper struct {
	pool      *Pool
	scope     string
	client    *http.Client
	transport http.RoundTripper
}

func (rt *roundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if e := req.Header.Get("X-Proxy-Error"); e != "" {
		return nil, fmt.Errorf(e)
	}

	// 缓冲请求体（重试需要）
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
		req.Body.Close()
		req.Body = io.NopCloser(bytes.NewReader(body))
	}

	resp, err := rt.transport.RoundTrip(req)
	if err != nil {
		return nil, err
	}

	// 429/403 → 冷却当前账号 + 切号重试一次
	if resp.StatusCode == 429 || resp.StatusCode == 403 {
		email := req.Header.Get("X-Proxy-Account")
		resp.Body.Close()

		if email != "" {
			rt.pool.cooldown(email, defaultCoolSec)
		}

		acct := rt.pool.selectAccount()
		if acct != nil && acct.Email != email {
			log.Printf("[retry] %s %d → %s", email, resp.StatusCode, acct.Email)

			acct.mu.Lock()
			tok := acct.AccessToken
			hdr := acct.Headers
			acct.mu.Unlock()

			if tok != "" {
				req.Header.Set("Authorization", "Bearer "+tok)
				for k, v := range hdr {
					req.Header.Set(k, v)
				}
				req.Header.Set("X-Proxy-Account", acct.Email)
				if body != nil {
					req.Body = io.NopCloser(bytes.NewReader(body))
				}
				return rt.transport.RoundTrip(req)
			}
		}

		return resp, nil
	}

	return resp, nil
}

// ---------- HTTP handler ----------

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 健康检查
	if r.URL.Path == "/healthz" || r.URL.Path == "/health" {
		writeJSON(w, 200, map[string]any{"ok": true, "accounts": len(s.pool.accounts)})
		return
	}

	// API Key 校验
	if s.config.APIKey != "" {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if got == "" {
			got = r.Header.Get("x-api-key")
		}
		if got != s.config.APIKey {
			writeJSON(w, 401, map[string]any{"error": "unauthorized"})
			return
		}
	}

	// 检查池状态
	if s.pool == nil || len(s.pool.accounts) == 0 {
		writeJSON(w, 503, map[string]any{"error": "no accounts loaded"})
		return
	}

	// 转发
	s.proxy.ServeHTTP(w, r)
}

// ---------- 启动 ----------

func main() {
	cfgPath := "config.json"
	if len(os.Args) > 1 {
		cfgPath = os.Args[1]
	}

	data, err := os.ReadFile(cfgPath)
	if err != nil {
		log.Fatalf("read config: %v", err)
	}
	data = []byte(os.ExpandEnv(string(data)))

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		log.Fatalf("parse config: %v", err)
	}
	if cfg.Listen == "" {
		cfg.Listen = ":5001"
	}
	if cfg.CPADir == "" {
		log.Fatalf("cpa_dir is required")
	}

	srv, err := NewServer(cfg)
	if err != nil {
		log.Fatalf("init: %v", err)
	}

	// 后台刷新
	go srv.refreshLoop(context.Background())

	log.Printf("grok-proxy listening on %s (%d accounts)", cfg.Listen, len(srv.pool.accounts))
	if cfg.APIKey != "" {
		log.Printf("auth: enabled")
	} else {
		log.Printf("auth: disabled")
	}
	log.Fatal(http.ListenAndServe(cfg.Listen, srv))
}

// ---------- 工具 ----------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}
