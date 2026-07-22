package main

import (
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"time"
)

// Reviver re-mints CPA via SSO when refresh_token dies.
//
// Policy (user-facing):
//  1. Load only non-*.dead CPA files.
//  2. RT fails → soft-dead + enqueue SSO (file still *.json).
//  3. SSO ok → write new tokens, back in pool.
//  4. SSO permanent fail / no SSO → rename *.json.dead (next boot skips forever).
//  5. SSO rate-limit → requeue later (do NOT rename).
type Reviver struct {
	pool    *Pool
	sso     *SSOStore
	oauth   *SSOOAuth
	cpaDir  string
	workers int

	mu     sync.Mutex
	queue  map[string]bool
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
	if r == nil || email == "" {
		return
	}
	r.mu.Lock()
	if !r.inProg[email] {
		r.queue[email] = true
	}
	r.mu.Unlock()
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
	}
	return out
}

func (r *Reviver) take(email string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.inProg[email] || !r.queue[email] {
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

func (r *Reviver) queueSize() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.queue) + len(r.inProg)
}

// Loop drains on wake (soft-dead) and periodically retries rate-limited ones.
func (r *Reviver) Loop(ctx context.Context, interval time.Duration) {
	if r == nil || r.oauth == nil {
		return
	}
	if interval <= 0 {
		interval = 10 * time.Minute
	}
	r.drain(ctx)

	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.wake:
			time.Sleep(200 * time.Millisecond)
			r.drain(ctx)
		case <-t.C:
			r.drain(ctx)
		}
	}
}

func (r *Reviver) drain(ctx context.Context) {
	// Serialize through oauth gate: still allow workers, but after a rate-limit
	// hit pause the whole drain so we don't burn the queue with 429 spam.
	sem := make(chan struct{}, r.workers)
	var wg sync.WaitGroup
	var stopOnce sync.Once
	stop := false
	var stopMu sync.Mutex

	for _, email := range r.pending() {
		if ctx.Err() != nil {
			break
		}
		stopMu.Lock()
		halted := stop
		stopMu.Unlock()
		if halted {
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
				switch {
				case isRateLimitErr(err):
					// keep soft-dead, retry later — do NOT rename
					requeue = true
					// halt remaining drain this round; oauth.trip already set gate
					stopOnce.Do(func() {
						stopMu.Lock()
						stop = true
						stopMu.Unlock()
						log.Printf("[revive] rate-limited — pause drain, remaining stay queued")
					})
				case isTransientErr(err):
					// network/timeout — soft-dead only, retry later
					requeue = true
				default:
					// permanent: no SSO / SSO rejected / other hard fail → .json.dead
					if acct := r.pool.findByEmail(em); acct != nil {
						acct.finalizeDead(err.Error())
					}
					if r.sso != nil && isHardSSOErr(err) {
						r.sso.MarkBad(em)
					}
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
	if r.sso == nil {
		return errNoSSO
	}
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

	acct := r.pool.findByEmail(email)
	if acct == nil {
		acct = &Account{
			Email:    email,
			Headers:  defaultCPAHeaders(),
			Type:     "xai",
			BaseURL:  upstreamURL,
			TokenEP:  tokenURL,
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
	if len(acct.Headers) == 0 {
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

	path := ensureCPAPath(r.cpaDir, email, oldPath)
	acct.mu.Lock()
	acct.dead = false
	acct.filePath = path
	acct.cooldownUntil = time.Time{}
	acct.mu.Unlock()

	return writeAccountFile(path, acct)
}

func isRateLimitErr(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	// Real device OAuth throttle (verified):
	// HTTP 429 {"error":"slow_down","error_description":"Too many device code requests..."}
	// confirm path may return Location error=rate_limited
	return strings.Contains(s, "429") ||
		strings.Contains(s, "slow_down") ||
		strings.Contains(s, "rate_limited") ||
		strings.Contains(s, "rate limit") ||
		strings.Contains(s, "too many")
}

// isTransientErr: network blips / timeouts — requeue, never .dead.
func isTransientErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "timeout") ||
		strings.Contains(s, "deadline exceeded") ||
		strings.Contains(s, "connection reset") ||
		strings.Contains(s, "connection refused") ||
		strings.Contains(s, "temporary failure") ||
		strings.Contains(s, "i/o timeout") ||
		strings.Contains(s, "tls handshake timeout") ||
		strings.Contains(s, "no such host") ||
		strings.Contains(s, "network is unreachable") ||
		strings.Contains(s, "eof") ||
		strings.Contains(s, "broken pipe")
}

func isHardSSOErr(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "sso challenge") ||
		strings.Contains(s, "oauth_denied") ||
		strings.Contains(s, "oauth_rejected") ||
		(strings.Contains(s, "invalid") && strings.Contains(s, "session")) ||
		err == errNoSSO
}

var errNoSSO = errString("no sso for email")

type errString string

func (e errString) Error() string { return string(e) }
