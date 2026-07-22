package main

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"
)

// Reviver tries to re-mint CPA via SSO when refresh_token is dead.
type Reviver struct {
	pool    *Pool
	sso     *SSOStore
	oauth   *SSOOAuth
	cpaDir  string
	workers int

	mu     sync.Mutex
	queue  map[string]bool // email set
	inProg map[string]bool
	wake   chan struct{}
}

func NewReviver(pool *Pool, sso *SSOStore, oauth *SSOOAuth, cpaDir string, workers int) *Reviver {
	if workers <= 0 {
		workers = 2
	}
	return &Reviver{
		pool:    pool,
		sso:     sso,
		oauth:   oauth,
		cpaDir:  cpaDir,
		workers: workers,
		queue:   map[string]bool{},
		inProg:  map[string]bool{},
		wake:    make(chan struct{}, 1),
	}
}

func (r *Reviver) Enqueue(email string) {
	if r == nil || r.sso == nil || email == "" {
		return
	}
	if _, ok := r.sso.Get(email); !ok {
		return
	}
	r.mu.Lock()
	if !r.inProg[email] {
		r.queue[email] = true
	}
	r.mu.Unlock()
	// wake drain loop (non-blocking)
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

func (r *Reviver) pending() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.queue))
	for e := range r.queue {
		if r.inProg[e] {
			continue
		}
		out = append(out, e)
		// don't remove yet — mark in progress when worker takes it
	}
	return out
}

func (r *Reviver) take(email string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.inProg[email] {
		return false
	}
	if !r.queue[email] {
		return false
	}
	delete(r.queue, email)
	r.inProg[email] = true
	return true
}

func (r *Reviver) done(email string, requeue bool) {
	r.mu.Lock()
	delete(r.inProg, email)
	if requeue {
		r.queue[email] = true
	}
	r.mu.Unlock()
}

// SeedDead scans pool for dead accounts into queue.
func (r *Reviver) SeedDead() {
	if r == nil {
		return
	}
	r.pool.mu.RLock()
	list := make([]*Account, len(r.pool.accounts))
	copy(list, r.pool.accounts)
	r.pool.mu.RUnlock()
	n := 0
	for _, a := range list {
		if a.isDead() {
			r.Enqueue(a.Email)
			n++
		}
	}
	log.Printf("[revive] seeded %d dead account(s), queue=%d", n, r.queueSize())
}

func (r *Reviver) queueSize() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.queue) + len(r.inProg)
}

// Loop drains revive queue on wake (markDead) and periodically.
func (r *Reviver) Loop(ctx context.Context, interval time.Duration) {
	if r == nil || r.oauth == nil {
		return
	}
	if interval <= 0 {
		interval = 10 * time.Minute
	}
	// initial seed + pass
	r.SeedDead()
	r.drain(ctx)

	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.wake:
			// brief settle so batch of markDead can queue
			time.Sleep(200 * time.Millisecond)
			r.drain(ctx)
		case <-t.C:
			r.SeedDead()
			r.drain(ctx)
		}
	}
}

func (r *Reviver) drain(ctx context.Context) {
	sem := make(chan struct{}, r.workers)
	var wg sync.WaitGroup
	for _, email := range r.pending() {
		if ctx.Err() != nil {
			break
		}
		if !r.take(email) {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(em string) {
			defer wg.Done()
			defer func() { <-sem }()
			err := r.reviveOne(ctx, em)
			requeue := false
			if err != nil {
				log.Printf("[revive] %s failed: %v", em, err)
				// rate limit → requeue later
				if strings.Contains(err.Error(), "429") || strings.Contains(err.Error(), "rate") {
					requeue = true
				}
				// hard SSO failure → drop SSO entry
				if strings.Contains(err.Error(), "sso challenge") ||
					strings.Contains(err.Error(), "oauth_denied") ||
					strings.Contains(err.Error(), "invalid") && strings.Contains(err.Error(), "session") {
					r.sso.MarkBad(em)
				}
			} else {
				log.Printf("[revive] %s ok", em)
			}
			r.done(em, requeue)
		}(email)
	}
	wg.Wait()
}

func (r *Reviver) reviveOne(ctx context.Context, email string) error {
	entry, ok := r.sso.Get(email)
	if !ok {
		return errNoSSO
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	tok, err := r.oauth.Exchange(ctx, entry.SSO)
	if err != nil {
		return err
	}

	// find existing account or create
	acct := r.pool.findByEmail(email)
	if acct == nil {
		acct = &Account{
			Email:   email,
			Headers: defaultCPAHeaders(),
			Type:    "xai",
			BaseURL: upstreamURL,
			TokenEP: tokenURL,
			AuthKind: "oauth",
		}
		r.pool.add(acct)
	}

	acct.mu.Lock()
	acct.IDToken = tok.IDToken
	acct.TokenType = tok.TokenType
	if tok.Subject != "" {
		acct.Sub = tok.Subject
	}
	if acct.Headers == nil || len(acct.Headers) == 0 {
		acct.Headers = defaultCPAHeaders()
	}
	if acct.BaseURL == "" {
		acct.BaseURL = upstreamURL
	}
	if acct.TokenEP == "" {
		acct.TokenEP = tokenURL
	}
	if acct.AuthKind == "" {
		acct.AuthKind = "oauth"
	}
	if acct.Type == "" {
		acct.Type = "xai"
	}
	oldPath := acct.filePath
	acct.mu.Unlock()

	acct.applyTokens(tok.AccessToken, tok.RefreshToken, tok.ExpiresIn)

	// clear dead + restore path
	path := ensureCPAPath(r.cpaDir, email, oldPath)
	acct.mu.Lock()
	acct.dead = false
	acct.filePath = path
	acct.cooldownUntil = time.Time{}
	acct.mu.Unlock()

	if err := writeAccountFile(path, acct); err != nil {
		return err
	}
	return nil
}

var errNoSSO = errString("no sso for email")

type errString string

func (e errString) Error() string { return string(e) }
