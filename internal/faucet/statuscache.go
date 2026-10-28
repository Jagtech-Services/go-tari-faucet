package faucet

import (
	"context"
	"sync"
	"time"

	"github.com/Jagtech-Services/go-tari-grpc-lib/v3/tari_generated"
	"github.com/sirupsen/logrus"
)

// statusSnapshot is the immutable value StatusCache holds at any point in
// time -- everything an HTTP handler needs to render the balance display
// or answer /healthz's wallet-side question, without ever touching the
// network. A new snapshot fully replaces the old one on every poll, so
// readers never observe a half-updated state.
type statusSnapshot struct {
	balance    uint64
	balanceOK  bool // true if the last balance poll succeeded
	walletUp   bool // true if the last connectivity poll reported Online
	walletUpOK bool // true if the last connectivity poll itself succeeded
	polledAt   time.Time
}

// StatusCache holds the last-known wallet balance and connectivity status,
// refreshed on a fixed interval by a single background goroutine
// (StartPolling) and read instantly (no GRPC call, no blocking) by many
// concurrent HTTP handlers via Get. This exists so Handler.Index and
// Handler.Healthz never block a request on a live wallet GRPC call --
// WalletClient.GetBalance/GetWalletConnectivity have no timeout of their
// own and have been observed in production to occasionally take 10+
// seconds to resolve over the Tailscale link to the wallet daemon.
//
// It deliberately does NOT sit in front of Dispense/SendTransactions --
// that's a real payout and must always be a live, synchronous call. This
// cache is exclusively for the read-only balance-display/connectivity-
// check paths.
type StatusCache struct {
	mu       sync.RWMutex
	snapshot statusSnapshot

	// Logger is used to note poll failures. Optional; falls back to
	// logrus.StandardLogger() like Service does.
	Logger *logrus.Logger
}

// NewStatusCache builds an empty StatusCache -- Get returns balanceOK ==
// false and walletUpOK == false until the first poll completes.
func NewStatusCache() *StatusCache {
	return &StatusCache{}
}

// Get returns the last-known balance and wallet connectivity, each paired
// with whether that specific value came from a successful poll. Callers
// must treat balanceOK == false / walletUpOK == false as "unknown," not
// as zero/down -- e.g. Handler.Index renders "balance unavailable" rather
// than a balance of 0 when balanceOK is false.
func (c *StatusCache) Get() (balance uint64, balanceOK bool, walletUp bool, walletUpOK bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	s := c.snapshot
	return s.balance, s.balanceOK, s.walletUp, s.walletUpOK
}

func (c *StatusCache) logger() *logrus.Logger {
	if c.Logger != nil {
		return c.Logger
	}
	return logrus.StandardLogger()
}

// poll calls the real WalletClient.GetBalance and
// WalletClient.GetWalletConnectivity once and atomically updates the
// cached snapshot with the results. A failure on either call marks just
// that half of the snapshot unavailable (balanceOK/walletUpOK false)
// rather than discarding the other half or panicking.
func (c *StatusCache) poll(wallet WalletClient) {
	next := statusSnapshot{polledAt: time.Now()}

	balResp, balErr := wallet.GetBalance()
	if balErr != nil || balResp == nil {
		next.balanceOK = false
		if balErr != nil {
			c.logger().WithField("error", balErr).Warn("statuscache: wallet balance poll failed")
		} else {
			c.logger().Warn("statuscache: wallet balance poll returned no response")
		}
	} else {
		next.balance = balResp.GetAvailableBalance()
		next.balanceOK = true
	}

	connResp, connErr := wallet.GetWalletConnectivity()
	if connErr != nil {
		next.walletUpOK = false
		c.logger().WithField("error", connErr).Warn("statuscache: wallet connectivity poll failed")
	} else {
		next.walletUpOK = true
		next.walletUp = connResp != nil && connResp.GetStatus() == tari_generated.CheckConnectivityResponse_Online
	}

	c.mu.Lock()
	c.snapshot = next
	c.mu.Unlock()
}

// StartPolling runs a background poll loop against wallet every interval,
// until ctx is cancelled. It does ONE immediate poll synchronously before
// returning -- so the cache is never empty for the first interval after
// startup -- but that first poll is bounded by initialPollTimeout so a
// slow/dead wallet at process startup cannot hang the caller (typically
// main()). Every poll after that first one runs purely in this goroutine
// and is unbounded: callers are expected to invoke StartPolling itself in
// a goroutine (e.g. `go cache.StartPolling(ctx, wallet, interval)`) so a
// slow later poll never blocks anything either.
func (c *StatusCache) StartPolling(ctx context.Context, wallet WalletClient, interval time.Duration) {
	c.pollWithTimeout(ctx, wallet, initialPollTimeout)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.poll(wallet)
		}
	}
}

// initialPollTimeout bounds StartPolling's first, synchronous poll so a
// slow/dead wallet at startup does not hang the caller (see
// StartPolling's doc comment). Every subsequent poll is unbounded and
// runs purely in the background.
const initialPollTimeout = 5 * time.Second

// pollWithTimeout runs poll but gives up waiting after timeout, leaving
// the cache at its prior (likely empty/zero) state if the wallet doesn't
// answer in time -- the poll goroutine itself is left running in the
// background and will still update the cache whenever it does eventually
// complete, since WalletClient's methods don't accept a context to
// cancel them by. This is only used for StartPolling's initial poll;
// every later tick calls poll directly with no timeout.
func (c *StatusCache) pollWithTimeout(ctx context.Context, wallet WalletClient, timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		c.poll(wallet)
		close(done)
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		c.logger().Warn("statuscache: initial wallet poll did not complete within timeout, continuing in background")
	case <-ctx.Done():
	}
}
