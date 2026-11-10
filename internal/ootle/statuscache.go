package ootle

import (
	"context"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

// StatusCache holds the last-known Ootle wallet-daemon reachability,
// refreshed on a fixed interval by a single background goroutine
// (StartPolling) and read instantly by many concurrent HTTP handlers
// via Get -- the Ootle-path analog of internal/faucet.StatusCache,
// duplicated here (rather than imported) so this package stays fully
// self-contained. It exists so no HTTP handler (Index, Healthz) ever
// blocks a request on a live wallet-daemon JSON-RPC call.
//
// Unlike faucet.StatusCache, there is no balance to poll here -- the
// Ootle wallet-daemon's balance concept isn't exposed by this faucet
// today -- so the cached snapshot is purely a reachability bit, via a
// bare auth.method call (WalletClient.Ping).
type StatusCache struct {
	mu sync.RWMutex
	up bool
	ok bool // true once the last poll itself completed (success or failure); false only before the very first poll

	// Logger is used to note poll failures. Optional; falls back to
	// logrus.StandardLogger().
	Logger *logrus.Logger
}

// NewStatusCache builds an empty StatusCache -- Get returns ok == false
// until the first poll completes.
func NewStatusCache() *StatusCache {
	return &StatusCache{}
}

// Get returns the last-known reachability (up) paired with whether that
// value came from a completed poll (ok). Callers must treat ok == false
// as "unknown," not as "down".
func (c *StatusCache) Get() (up bool, ok bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.up, c.ok
}

func (c *StatusCache) logger() *logrus.Logger {
	if c.Logger != nil {
		return c.Logger
	}
	return logrus.StandardLogger()
}

// poll calls WalletClient.Ping once and atomically updates the cached
// snapshot with the result.
func (c *StatusCache) poll(ctx context.Context, wallet WalletClient) {
	err := wallet.Ping(ctx)
	up := err == nil
	if err != nil {
		c.logger().WithField("error", err).Warn("ootle statuscache: wallet-daemon reachability poll failed")
	}

	c.mu.Lock()
	c.up = up
	c.ok = true
	c.mu.Unlock()
}

// initialPollTimeout bounds StartPolling's first, synchronous poll so a
// slow/dead wallet-daemon at startup does not hang the caller -- see
// faucet.StatusCache's identically-named constant for the same
// reasoning. Every subsequent poll is unbounded and runs purely in the
// background.
const initialPollTimeout = 5 * time.Second

// StartPolling runs a background poll loop against wallet every
// interval, until ctx is cancelled. It does ONE immediate poll
// (bounded by initialPollTimeout) before returning, so the cache is
// never empty for the first interval after startup; callers are
// expected to invoke StartPolling itself in a goroutine (e.g. `go
// cache.StartPolling(ctx, wallet, interval)`).
func (c *StatusCache) StartPolling(ctx context.Context, wallet WalletClient, interval time.Duration) {
	initCtx, cancel := context.WithTimeout(ctx, initialPollTimeout)
	c.poll(initCtx, wallet)
	cancel()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.poll(ctx, wallet)
		}
	}
}
