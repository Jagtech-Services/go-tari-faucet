package ootle

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeRepo is an in-memory Repository test double, the Ootle-path
// analog of internal/faucet's fakeRepo (same shape/convention, see its
// doc comment) -- plain maps/slices with error-injection knobs, no
// mocking framework, safe for concurrent use via a per-key lock
// ordering that mirrors PGRepository.ReserveDispense's advisory-lock
// ordering exactly.
type fakeRepo struct {
	mu        sync.Mutex
	keyLocks  map[string]*sync.Mutex
	lastByKey map[string]time.Time // keyed by "addr:"+address or "ip:"+ip; pre-seedable
	pending   map[int64]bool
	recorded  []recordedDispense
	nextID    int64

	reserveErr  error
	finalizeErr error
	pingErr     error

	// reserveCalls counts ReserveDispense invocations, so tests can
	// assert the rate-limit check was never reached (e.g. the
	// not-configured precondition must short-circuit before it).
	reserveCalls int
}

type recordedDispense struct {
	ID        int64
	Address   string
	IP        string
	Amount    uint64
	Success   bool
	Error     string
	TxID      string
	CreatedAt time.Time
}

func (f *fakeRepo) lockFor(key string) *sync.Mutex {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.keyLocks == nil {
		f.keyLocks = map[string]*sync.Mutex{}
	}
	m, ok := f.keyLocks[key]
	if !ok {
		m = &sync.Mutex{}
		f.keyLocks[key] = m
	}
	return m
}

func (f *fakeRepo) ReserveDispense(_ context.Context, address, ip string, amount uint64, since, now time.Time) (Reservation, error) {
	first, second := address, ip
	if first > second {
		first, second = second, first
	}
	lock1 := f.lockFor(first)
	lock1.Lock()
	defer lock1.Unlock()
	if second != first {
		lock2 := f.lockFor(second)
		lock2.Lock()
		defer lock2.Unlock()
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	f.reserveCalls++
	if f.reserveErr != nil {
		return Reservation{}, f.reserveErr
	}

	var latest time.Time
	var found bool
	consider := func(t time.Time) {
		if t.Before(since) {
			return
		}
		if !found || t.After(latest) {
			latest = t
			found = true
		}
	}
	for _, key := range []string{"addr:" + address, "ip:" + ip} {
		if t, ok := f.lastByKey[key]; ok {
			consider(t)
		}
	}
	for _, rec := range f.recorded {
		if rec.Address != address && rec.IP != ip {
			continue
		}
		if rec.Success || f.pending[rec.ID] {
			consider(rec.CreatedAt)
		}
	}
	if found {
		return Reservation{RateLimited: true, LastSuccessAt: latest}, nil
	}

	f.nextID++
	id := f.nextID
	if f.pending == nil {
		f.pending = map[int64]bool{}
	}
	f.pending[id] = true
	f.recorded = append(f.recorded, recordedDispense{
		ID:        id,
		Address:   address,
		IP:        ip,
		Amount:    amount,
		CreatedAt: now,
	})
	return Reservation{ID: id}, nil
}

func (f *fakeRepo) FinalizeDispense(_ context.Context, id int64, success bool, errMsg string, txID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.finalizeErr != nil {
		return f.finalizeErr
	}
	for i := range f.recorded {
		if f.recorded[i].ID == id {
			f.recorded[i].Success = success
			f.recorded[i].Error = errMsg
			f.recorded[i].TxID = txID
			delete(f.pending, id)
			if success {
				if f.lastByKey == nil {
					f.lastByKey = map[string]time.Time{}
				}
				f.lastByKey["addr:"+f.recorded[i].Address] = f.recorded[i].CreatedAt
				f.lastByKey["ip:"+f.recorded[i].IP] = f.recorded[i].CreatedAt
			}
			return nil
		}
	}
	return errors.New("fakeRepo.FinalizeDispense: no reserved row with that id")
}

func (f *fakeRepo) Ping(_ context.Context) error {
	return f.pingErr
}

// fakeWallet is an in-memory WalletClient test double.
type fakeWallet struct {
	txID string
	err  error

	calls []submitDispenseCall
}

type submitDispenseCall struct {
	faucetComponent string
	dest            string
	amount          uint64
	maxFee          uint64
	waitTimeoutSecs int
}

func (f *fakeWallet) SubmitDispense(_ context.Context, faucetComponent, dest string, amount, maxFee uint64, waitTimeoutSecs int) (string, error) {
	f.calls = append(f.calls, submitDispenseCall{faucetComponent, dest, amount, maxFee, waitTimeoutSecs})
	if f.err != nil {
		return "", f.err
	}
	return f.txID, nil
}

func (f *fakeWallet) Ping(_ context.Context) error {
	return f.err
}

// fakeClock is a fixed Clock test double.
type fakeClock struct {
	now time.Time
}

func (f *fakeClock) Now() time.Time { return f.now }

func testConfig() Config {
	return Config{
		WalletBaseURL:          "http://localhost:12008",
		FaucetComponentAddress: "component_faucet_address",
		DispenseAmount:         1000,
		MaxFee:                 10,
		WaitResultTimeoutSecs:  120,
		RateLimitWindow:        24 * time.Hour,
	}
}

func TestService_Dispense_NotConfiguredWhenFaucetComponentAddressUnset(t *testing.T) {
	repo := &fakeRepo{}
	wallet := &fakeWallet{txID: "tx_abc"}
	cfg := testConfig()
	cfg.FaucetComponentAddress = ""
	svc := &Service{Repo: repo, Wallet: wallet, Clock: &fakeClock{now: time.Now()}, Config: cfg}

	result := svc.Dispense(context.Background(), "component_dest", "1.2.3.4")

	if result.Outcome != OutcomeNotConfigured {
		t.Fatalf("Outcome = %v, want OutcomeNotConfigured", result.Outcome)
	}
	if !errors.Is(result.Err, ErrFaucetComponentNotConfigured) {
		t.Errorf("Err = %v, want ErrFaucetComponentNotConfigured", result.Err)
	}
	// Must fail closed before ever touching the rate-limit table or
	// the wallet -- an unconfigured faucet component can never
	// succeed, so neither should be consulted.
	if repo.reserveCalls != 0 {
		t.Errorf("ReserveDispense called %d times, want 0 (must fail closed before any DB round trip)", repo.reserveCalls)
	}
	if len(wallet.calls) != 0 {
		t.Errorf("wallet.SubmitDispense called %d times, want 0", len(wallet.calls))
	}
}

func TestService_Dispense_InvalidDestination(t *testing.T) {
	tests := []struct {
		name string
		dest string
	}{
		{"empty string", ""},
		{"whitespace only", "   "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := &fakeRepo{}
			wallet := &fakeWallet{txID: "tx_abc"}
			svc := &Service{Repo: repo, Wallet: wallet, Clock: &fakeClock{now: time.Now()}, Config: testConfig()}

			result := svc.Dispense(context.Background(), tt.dest, "1.2.3.4")

			if result.Outcome != OutcomeInvalidAddress {
				t.Fatalf("Outcome = %v, want OutcomeInvalidAddress", result.Outcome)
			}
			if repo.reserveCalls != 0 {
				t.Errorf("ReserveDispense called %d times, want 0", repo.reserveCalls)
			}
		})
	}
}

func TestService_Dispense_Success(t *testing.T) {
	repo := &fakeRepo{}
	wallet := &fakeWallet{txID: "tx_deadbeef"}
	cfg := testConfig()
	svc := &Service{Repo: repo, Wallet: wallet, Clock: &fakeClock{now: time.Now()}, Config: cfg}

	result := svc.Dispense(context.Background(), "  component_dest  ", "1.2.3.4")

	if result.Outcome != OutcomeSuccess {
		t.Fatalf("Outcome = %v, want OutcomeSuccess (err=%v)", result.Outcome, result.Err)
	}
	if result.TxID != "tx_deadbeef" {
		t.Errorf("TxID = %q, want %q", result.TxID, "tx_deadbeef")
	}
	if result.Amount != cfg.DispenseAmount {
		t.Errorf("Amount = %d, want %d", result.Amount, cfg.DispenseAmount)
	}
	if len(wallet.calls) != 1 {
		t.Fatalf("wallet.SubmitDispense called %d times, want 1", len(wallet.calls))
	}
	call := wallet.calls[0]
	if call.faucetComponent != cfg.FaucetComponentAddress {
		t.Errorf("faucetComponent = %q, want %q", call.faucetComponent, cfg.FaucetComponentAddress)
	}
	// Destination must be trimmed before being sent to the wallet.
	if call.dest != "component_dest" {
		t.Errorf("dest = %q, want trimmed %q", call.dest, "component_dest")
	}
	if call.amount != cfg.DispenseAmount {
		t.Errorf("amount = %d, want %d", call.amount, cfg.DispenseAmount)
	}
	if call.maxFee != cfg.MaxFee {
		t.Errorf("maxFee = %d, want %d", call.maxFee, cfg.MaxFee)
	}
	if call.waitTimeoutSecs != cfg.WaitResultTimeoutSecs {
		t.Errorf("waitTimeoutSecs = %d, want %d", call.waitTimeoutSecs, cfg.WaitResultTimeoutSecs)
	}

	// The audit row must be finalized as a success.
	if len(repo.recorded) != 1 {
		t.Fatalf("recorded %d rows, want 1", len(repo.recorded))
	}
	rec := repo.recorded[0]
	if !rec.Success || rec.TxID != "tx_deadbeef" {
		t.Errorf("recorded row = %+v, want Success=true TxID=tx_deadbeef", rec)
	}
}

// TestService_Dispense_RateLimited proves a second Dispense call for
// the same destination address within the rate-limit window is
// rejected without ever touching the wallet again -- the core
// rate-limit behavior this table/Repository exists to enforce,
// independent of (and never colliding with) internal/faucet's L1
// rate-limit table.
func TestService_Dispense_RateLimited(t *testing.T) {
	repo := &fakeRepo{}
	wallet := &fakeWallet{txID: "tx_1"}
	now := time.Now()
	svc := &Service{Repo: repo, Wallet: wallet, Clock: &fakeClock{now: now}, Config: testConfig()}

	first := svc.Dispense(context.Background(), "component_dest", "1.2.3.4")
	if first.Outcome != OutcomeSuccess {
		t.Fatalf("first Dispense Outcome = %v, want OutcomeSuccess (err=%v)", first.Outcome, first.Err)
	}

	second := svc.Dispense(context.Background(), "component_dest", "1.2.3.4")
	if second.Outcome != OutcomeRateLimited {
		t.Fatalf("second Dispense Outcome = %v, want OutcomeRateLimited", second.Outcome)
	}
	wantRetryAfter := now.Add(testConfig().RateLimitWindow)
	if !second.RetryAfter.Equal(wantRetryAfter) {
		t.Errorf("RetryAfter = %v, want %v", second.RetryAfter, wantRetryAfter)
	}
	if len(wallet.calls) != 1 {
		t.Errorf("wallet.SubmitDispense called %d times, want exactly 1 (the rate-limited call must never reach the wallet)", len(wallet.calls))
	}
}

// TestService_Dispense_RateLimited_DifferentAddressSameIP proves the
// rate limit also blocks on IP alone, not just address, same as
// internal/faucet's L1 rate limit -- a second request from the same IP
// but a different destination address must still be blocked.
func TestService_Dispense_RateLimited_DifferentAddressSameIP(t *testing.T) {
	repo := &fakeRepo{}
	wallet := &fakeWallet{txID: "tx_1"}
	now := time.Now()
	svc := &Service{Repo: repo, Wallet: wallet, Clock: &fakeClock{now: now}, Config: testConfig()}

	first := svc.Dispense(context.Background(), "component_dest_a", "1.2.3.4")
	if first.Outcome != OutcomeSuccess {
		t.Fatalf("first Dispense Outcome = %v, want OutcomeSuccess (err=%v)", first.Outcome, first.Err)
	}

	second := svc.Dispense(context.Background(), "component_dest_b", "1.2.3.4")
	if second.Outcome != OutcomeRateLimited {
		t.Fatalf("second Dispense (different address, same IP) Outcome = %v, want OutcomeRateLimited", second.Outcome)
	}
}

// TestService_Dispense_RateLimitWindowExpires proves a dispense is
// allowed again once the rate-limit window has fully elapsed.
func TestService_Dispense_RateLimitWindowExpires(t *testing.T) {
	repo := &fakeRepo{}
	wallet := &fakeWallet{txID: "tx_1"}
	now := time.Now()
	clock := &fakeClock{now: now}
	svc := &Service{Repo: repo, Wallet: wallet, Clock: clock, Config: testConfig()}

	first := svc.Dispense(context.Background(), "component_dest", "1.2.3.4")
	if first.Outcome != OutcomeSuccess {
		t.Fatalf("first Dispense Outcome = %v, want OutcomeSuccess", first.Outcome)
	}

	clock.now = now.Add(testConfig().RateLimitWindow + time.Second)
	second := svc.Dispense(context.Background(), "component_dest", "1.2.3.4")
	if second.Outcome != OutcomeSuccess {
		t.Fatalf("second Dispense (after window elapsed) Outcome = %v, want OutcomeSuccess (err=%v)", second.Outcome, second.Err)
	}
	if len(wallet.calls) != 2 {
		t.Errorf("wallet.SubmitDispense called %d times, want 2", len(wallet.calls))
	}
}

// TestService_Dispense_FailedDispenseAllowsImmediateRetry proves a
// wallet-level failure does NOT rate-limit the caller -- only a
// genuine success should, mirroring internal/faucet's identical
// behavior for the L1 path (see its "or error is null" ReserveDispense
// clause).
func TestService_Dispense_FailedDispenseAllowsImmediateRetry(t *testing.T) {
	repo := &fakeRepo{}
	failingWallet := &fakeWallet{err: errors.New("wallet-daemon: connection refused")}
	svc := &Service{Repo: repo, Wallet: failingWallet, Clock: &fakeClock{now: time.Now()}, Config: testConfig()}

	first := svc.Dispense(context.Background(), "component_dest", "1.2.3.4")
	if first.Outcome != OutcomeError {
		t.Fatalf("first Dispense (failing wallet) Outcome = %v, want OutcomeError", first.Outcome)
	}

	succeedingWallet := &fakeWallet{txID: "tx_ok"}
	svc.Wallet = succeedingWallet
	second := svc.Dispense(context.Background(), "component_dest", "1.2.3.4")
	if second.Outcome != OutcomeSuccess {
		t.Fatalf("second Dispense (same destination/ip, after a FAILED first attempt) Outcome = %v, want OutcomeSuccess", second.Outcome)
	}
}

// TestService_Dispense_WebAuthnUnsupportedSurfacesAsError proves the
// webauthn-unsupported case (see ErrWebAuthnUnsupported) is returned
// as a clean error Outcome, never a panic, and records the failed
// attempt.
func TestService_Dispense_WebAuthnUnsupportedSurfacesAsError(t *testing.T) {
	repo := &fakeRepo{}
	wallet := &fakeWallet{err: ErrWebAuthnUnsupported}
	svc := &Service{Repo: repo, Wallet: wallet, Clock: &fakeClock{now: time.Now()}, Config: testConfig()}

	result := svc.Dispense(context.Background(), "component_dest", "1.2.3.4")

	if result.Outcome != OutcomeError {
		t.Fatalf("Outcome = %v, want OutcomeError", result.Outcome)
	}
	if !errors.Is(result.Err, ErrWebAuthnUnsupported) {
		t.Errorf("Err = %v, want ErrWebAuthnUnsupported", result.Err)
	}
	if len(repo.recorded) != 1 || repo.recorded[0].Success {
		t.Errorf("recorded = %+v, want exactly one row with Success=false", repo.recorded)
	}
}

// TestService_Dispense_ReserveDispenseErrorSurfacesAsError proves a
// Repository-level error (e.g. a DB outage) is returned as a clean
// error Outcome rather than panicking the request.
func TestService_Dispense_ReserveDispenseErrorSurfacesAsError(t *testing.T) {
	repo := &fakeRepo{reserveErr: errors.New("db: connection reset")}
	wallet := &fakeWallet{txID: "tx_1"}
	svc := &Service{Repo: repo, Wallet: wallet, Clock: &fakeClock{now: time.Now()}, Config: testConfig()}

	result := svc.Dispense(context.Background(), "component_dest", "1.2.3.4")

	if result.Outcome != OutcomeError {
		t.Fatalf("Outcome = %v, want OutcomeError", result.Outcome)
	}
	if len(wallet.calls) != 0 {
		t.Errorf("wallet.SubmitDispense called %d times, want 0 (must never be reached when the reservation itself fails)", len(wallet.calls))
	}
}

// TestService_Dispense_ConcurrentSameDestinationAndIP is the
// in-memory-fake approximation (see fakeRepo's doc comment, and
// internal/faucet's TestService_Dispense_ConcurrentSameAddressAndIP /
// TestIntegrationReserveDispense_ConcurrentSameAddressAndIP for the
// authoritative real-Postgres version of this same property) of the
// race this rate-limit discipline exists to close: N concurrent
// Dispense calls for the exact same destination/ip must result in
// exactly one success.
func TestService_Dispense_ConcurrentSameDestinationAndIP(t *testing.T) {
	repo := &fakeRepo{}
	wallet := &fakeWallet{txID: "tx_1"}
	svc := &Service{Repo: repo, Wallet: wallet, Clock: &fakeClock{now: time.Now()}, Config: testConfig()}

	const n = 20
	results := make([]Result, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			results[i] = svc.Dispense(context.Background(), "component_dest", "203.0.113.42")
		}(i)
	}
	wg.Wait()

	var successes, rateLimited, other int
	for _, r := range results {
		switch r.Outcome {
		case OutcomeSuccess:
			successes++
		case OutcomeRateLimited:
			rateLimited++
		default:
			other++
		}
	}
	if successes != 1 {
		t.Errorf("got %d OutcomeSuccess results across %d concurrent calls, want exactly 1", successes, n)
	}
	if rateLimited != n-1 {
		t.Errorf("got %d OutcomeRateLimited results, want exactly %d", rateLimited, n-1)
	}
	if other != 0 {
		t.Errorf("got %d unexpected outcomes, want 0", other)
	}
}
