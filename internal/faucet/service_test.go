package faucet

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Jagtech-Services/go-tari-grpc-lib/v3/tari_generated"
)

// fakeRepo is an in-memory Repository test double, following the same
// fakeXxx convention (and shape) as go-crypto-pool/internal/backend/
// unlocker's fakeRepo test double: plain maps/slices, no mocking
// framework, with error-injection knobs and a call log for assertions.
//
// ReserveDispense below deliberately models Postgres's
// pg_advisory_xact_lock semantics faithfully -- a real per-key
// sync.Mutex acquired in the same sorted order PGRepository.
// ReserveDispense uses, held across the whole check-and-reserve
// section -- rather than just being a sequential, single-threaded
// mock. That makes fakeRepo safe to drive from many concurrent
// goroutines (see TestService_Dispense_ConcurrentSameAddressAndIP),
// not just from single-threaded table-driven tests.
type fakeRepo struct {
	mu        sync.Mutex             // guards everything below, including keyLocks itself
	keyLocks  map[string]*sync.Mutex // one lock per distinct address/ip string, created on demand
	lastByKey map[string]time.Time   // keyed by "addr:"+address or "ip:"+ip; pre-seedable directly by tests
	pending   map[int64]bool         // reservation IDs not yet finalized -- see ReserveDispense
	lastErr   error
	pingErr   error
	recorded  []DispenseRecord
	recordErr error
	nextID    int64
	// lookupCalls counts ReserveDispense invocations, so tests can
	// assert the rate-limit check was never reached (e.g. a
	// honeypot-tripped request must short-circuit before it).
	lookupCalls int
}

// lockFor returns the shared *sync.Mutex for key, creating it on first
// use. This helper's own f.mu hold is intentionally short-lived (just
// long enough to read/populate the map) -- callers lock the returned
// mutex themselves afterward, exactly mirroring how a real
// pg_advisory_xact_lock(hashtext(key)) call works: the "which lock"
// lookup and the actual locking are two separate steps.
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

// ReserveDispense is fakeRepo's analog of PGRepository.ReserveDispense:
// it takes the two per-key locks in the same string-sorted order (so
// swapped address/ip roles between two concurrent callers can't
// deadlock here either -- see PGRepository.ReserveDispense's doc
// comment for the full reasoning), then re-checks the rolling-window
// rate limit and reserves a placeholder row, all while holding those
// locks -- so a second concurrent call for the same address or ip is
// guaranteed to observe the first call's reservation before it can
// itself decide "not rate limited".
//
// The rate-limit check itself considers two signals, matching
// PGRepository.ReserveDispense's "success or error is null" SQL
// exactly: (1) lastByKey, a directly pre-seedable map some tests
// construct fakeRepo with to simulate a prior successful dispense
// without going through a real ReserveDispense/FinalizeDispense round
// trip, and (2) recorded/pending, populated by real ReserveDispense/
// FinalizeDispense calls, which is what makes a still-in-flight (not
// yet finalized) reservation from another concurrent goroutine block
// too -- the exact mechanism the real bug needed (a placeholder alone
// with success=false wouldn't have blocked anything).
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

	f.lookupCalls++
	if f.lastErr != nil {
		return Reservation{}, f.lastErr
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
	f.recorded = append(f.recorded, DispenseRecord{
		ID:        id,
		Address:   address,
		IP:        ip,
		Amount:    amount,
		CreatedAt: now,
	})
	return Reservation{ID: id}, nil
}

// FinalizeDispense updates the placeholder row ReserveDispense reserved
// (by id) with the wallet call's real outcome, mirroring
// PGRepository.FinalizeDispense -- deliberately taking no per-key lock
// (the real UPDATE doesn't either; see its doc comment for why that's
// safe). Clearing f.pending[id] here is what lets a finalized *failure*
// stop blocking future requests immediately, while a finalized success
// keeps blocking via rec.Success for the rest of the window -- same
// distinction PGRepository.FinalizeDispense's non-NULL error column
// draws.
func (f *fakeRepo) FinalizeDispense(_ context.Context, id int64, success bool, errMsg string, txID uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.recordErr != nil {
		return f.recordErr
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
	return fmt.Errorf("fakeRepo.FinalizeDispense: no reserved row with id %d", id)
}

func (f *fakeRepo) Ping(_ context.Context) error {
	return f.pingErr
}

// fakeWallet is an in-memory WalletClient test double.
type fakeWallet struct {
	sendResp        *tari_generated.TransferResponse
	sendErr         error
	connectivity    *tari_generated.CheckConnectivityResponse
	connectivityErr error
	sentRecipients  [][]*tari_generated.PaymentRecipient
	sentSingleTx    []bool
	balanceResp     *tari_generated.GetBalanceResponse
	balanceErr      error
}

func (f *fakeWallet) SendTransactions(transactions []*tari_generated.PaymentRecipient, singleTx bool) (*tari_generated.TransferResponse, error) {
	f.sentRecipients = append(f.sentRecipients, transactions)
	f.sentSingleTx = append(f.sentSingleTx, singleTx)
	if f.sendErr != nil {
		return nil, f.sendErr
	}
	return f.sendResp, nil
}

func (f *fakeWallet) GetWalletConnectivity() (*tari_generated.CheckConnectivityResponse, error) {
	if f.connectivityErr != nil {
		return nil, f.connectivityErr
	}
	return f.connectivity, nil
}

func (f *fakeWallet) GetBalance() (*tari_generated.GetBalanceResponse, error) {
	if f.balanceErr != nil {
		return nil, f.balanceErr
	}
	return f.balanceResp, nil
}

// fakeClock is a fixed/steppable Clock test double.
type fakeClock struct {
	now time.Time
}

func (f *fakeClock) Now() time.Time { return f.now }

// fakeRandomizer is an injectable AmountRandomizer test double. Two
// modes, matched to what different tests in this file need:
//
//   - values, if non-empty, is consumed sequentially (one value per
//     IntRange call, past the end of the slice it repeats the last
//     value) -- used by the random-path-threads-through test to prove
//     distinct injected values genuinely flow through to Result.Amount,
//     the wallet call, and the recorded audit row on separate calls.
//   - panicOnCall, if true, makes IntRange panic instead of returning a
//     value -- used by the off-path test to prove chooseDispenseAmount
//     never calls the randomizer at all when MaxDispenseAmount <=
//     DispenseAmount, an early return rather than a zero-width-range
//     call that coincidentally never panics.
//
// calls counts every IntRange invocation regardless of mode, so tests
// can also assert an exact call count directly.
type fakeRandomizer struct {
	values      []int64
	panicOnCall bool
	calls       int
}

func (f *fakeRandomizer) IntRange(min, max int64) int64 {
	f.calls++
	if f.panicOnCall {
		panic("fakeRandomizer.IntRange: must not be called on the fixed-amount path")
	}
	if len(f.values) == 0 {
		return min
	}
	idx := f.calls - 1
	if idx >= len(f.values) {
		idx = len(f.values) - 1
	}
	return f.values[idx]
}

func successResponse(txID uint64) *tari_generated.TransferResponse {
	return &tari_generated.TransferResponse{
		Results: []*tari_generated.TransferResult{
			{IsSuccess: true, TransactionId: txID},
		},
	}
}

func TestService_Dispense_RejectsInvalidAddress(t *testing.T) {
	repo := &fakeRepo{}
	wallet := &fakeWallet{sendResp: successResponse(1)}
	svc := &Service{
		Repo:   repo,
		Wallet: wallet,
		Clock:  &fakeClock{now: time.Now()},
		Config: Config{DispenseAmount: 1000000, RateLimitWindow: time.Hour},
	}

	result := svc.Dispense(context.Background(), "not-a-real-address", "10.0.0.1")
	if result.Outcome != OutcomeInvalidAddress {
		t.Fatalf("Outcome = %v, want OutcomeInvalidAddress", result.Outcome)
	}
	if result.Err == nil {
		t.Fatal("expected a non-nil error for an invalid address")
	}
	if len(wallet.sentRecipients) != 0 {
		t.Fatal("wallet should never be called for an invalid address")
	}
	if len(repo.recorded) != 0 {
		t.Fatal("no audit row should be recorded for an invalid address (it never reached dispensing)")
	}
}

func TestService_Dispense_SucceedsAndRecordsAuditRow(t *testing.T) {
	validAddr := validTestnetAddress(t)
	repo := &fakeRepo{}
	wallet := &fakeWallet{sendResp: successResponse(42)}
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	svc := &Service{
		Repo:   repo,
		Wallet: wallet,
		Clock:  &fakeClock{now: now},
		Config: Config{DispenseAmount: 1000000, RateLimitWindow: time.Hour},
	}

	result := svc.Dispense(context.Background(), validAddr, "10.0.0.1")
	if result.Outcome != OutcomeSuccess {
		t.Fatalf("Outcome = %v, want OutcomeSuccess (err=%v)", result.Outcome, result.Err)
	}
	if result.TxID != 42 {
		t.Fatalf("TxID = %d, want 42", result.TxID)
	}
	if len(repo.recorded) != 1 {
		t.Fatalf("expected exactly one recorded dispense, got %d", len(repo.recorded))
	}
	rec := repo.recorded[0]
	if !rec.Success || rec.Amount != 1000000 || rec.IP != "10.0.0.1" {
		t.Fatalf("recorded dispense = %+v, unexpected values", rec)
	}
	if len(wallet.sentSingleTx) != 1 || wallet.sentSingleTx[0] != false {
		t.Fatalf("wallet.SendTransactions singleTx = %v, want [false] (faucet dispenses one recipient per call, so single_tx must be passed false)", wallet.sentSingleTx)
	}
}

func TestService_Dispense_RateLimitsSameAddressWithinWindow(t *testing.T) {
	validAddr := validTestnetAddress(t)
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	repo := &fakeRepo{}
	wallet := &fakeWallet{sendResp: successResponse(1)}
	clock := &fakeClock{now: base}
	svc := &Service{
		Repo:   repo,
		Wallet: wallet,
		Clock:  clock,
		Config: Config{DispenseAmount: 1000000, RateLimitWindow: 24 * time.Hour},
	}

	// First request from this address/IP succeeds.
	first := svc.Dispense(context.Background(), validAddr, "10.0.0.1")
	if first.Outcome != OutcomeSuccess {
		t.Fatalf("first Dispense Outcome = %v, want OutcomeSuccess", first.Outcome)
	}

	// A second request for the same address, a few minutes later but well
	// inside the 24h window, from a *different* IP must still be blocked
	// (rate limit checks address OR ip).
	clock.now = base.Add(10 * time.Minute)
	second := svc.Dispense(context.Background(), validAddr, "10.0.0.2")
	if second.Outcome != OutcomeRateLimited {
		t.Fatalf("second Dispense Outcome = %v, want OutcomeRateLimited", second.Outcome)
	}
	wantRetry := base.Add(24 * time.Hour)
	if !second.RetryAfter.Equal(wantRetry) {
		t.Fatalf("RetryAfter = %v, want %v", second.RetryAfter, wantRetry)
	}
	if len(wallet.sentRecipients) != 1 {
		t.Fatalf("wallet.SendTransactions should have been called exactly once (not on the rate-limited attempt), got %d calls", len(wallet.sentRecipients))
	}
}

func TestService_Dispense_RateLimitsSameIPDifferentAddress(t *testing.T) {
	addrOne := validTestnetAddress(t)
	addrTwo := validTestnetAddress(t)
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	repo := &fakeRepo{}
	wallet := &fakeWallet{sendResp: successResponse(1)}
	clock := &fakeClock{now: base}
	svc := &Service{
		Repo:   repo,
		Wallet: wallet,
		Clock:  clock,
		Config: Config{DispenseAmount: 1000000, RateLimitWindow: time.Hour},
	}

	first := svc.Dispense(context.Background(), addrOne, "10.0.0.5")
	if first.Outcome != OutcomeSuccess {
		t.Fatalf("first Dispense Outcome = %v, want OutcomeSuccess", first.Outcome)
	}

	// Different address, same IP, still within the window -> blocked.
	clock.now = base.Add(time.Minute)
	second := svc.Dispense(context.Background(), addrTwo, "10.0.0.5")
	if second.Outcome != OutcomeRateLimited {
		t.Fatalf("second Dispense Outcome = %v, want OutcomeRateLimited", second.Outcome)
	}
}

func TestService_Dispense_AllowsAfterWindowExpires(t *testing.T) {
	validAddr := validTestnetAddress(t)
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	repo := &fakeRepo{}
	wallet := &fakeWallet{sendResp: successResponse(1)}
	clock := &fakeClock{now: base}
	svc := &Service{
		Repo:   repo,
		Wallet: wallet,
		Clock:  clock,
		Config: Config{DispenseAmount: 1000000, RateLimitWindow: time.Hour},
	}

	first := svc.Dispense(context.Background(), validAddr, "10.0.0.1")
	if first.Outcome != OutcomeSuccess {
		t.Fatalf("first Dispense Outcome = %v, want OutcomeSuccess", first.Outcome)
	}

	// Exactly at window expiry (base + window) the request should once
	// again be allowed: LastSuccessfulDispense's "since" boundary is
	// exclusive going forward in time from the perspective of a fresh
	// request one full window later.
	clock.now = base.Add(time.Hour + time.Second)
	third := svc.Dispense(context.Background(), validAddr, "10.0.0.1")
	if third.Outcome != OutcomeSuccess {
		t.Fatalf("Dispense after window expiry Outcome = %v, want OutcomeSuccess (err=%v)", third.Outcome, third.Err)
	}
}

func TestService_Dispense_WalletFailureIsRecordedAndReturnsError(t *testing.T) {
	validAddr := validTestnetAddress(t)
	repo := &fakeRepo{}
	wallet := &fakeWallet{sendErr: errors.New("grpc: connection refused")}
	svc := &Service{
		Repo:   repo,
		Wallet: wallet,
		Clock:  &fakeClock{now: time.Now()},
		Config: Config{DispenseAmount: 1000000, RateLimitWindow: time.Hour},
	}

	result := svc.Dispense(context.Background(), validAddr, "10.0.0.1")
	if result.Outcome != OutcomeError {
		t.Fatalf("Outcome = %v, want OutcomeError", result.Outcome)
	}
	if len(repo.recorded) != 1 || repo.recorded[0].Success {
		t.Fatalf("expected exactly one failed audit row, got %+v", repo.recorded)
	}
}

func TestService_Dispense_WalletRejectionIsRecordedAndReturnsError(t *testing.T) {
	validAddr := validTestnetAddress(t)
	repo := &fakeRepo{}
	wallet := &fakeWallet{sendResp: &tari_generated.TransferResponse{
		Results: []*tari_generated.TransferResult{
			{IsSuccess: false, FailureMessage: "insufficient funds"},
		},
	}}
	svc := &Service{
		Repo:   repo,
		Wallet: wallet,
		Clock:  &fakeClock{now: time.Now()},
		Config: Config{DispenseAmount: 1000000, RateLimitWindow: time.Hour},
	}

	result := svc.Dispense(context.Background(), validAddr, "10.0.0.1")
	if result.Outcome != OutcomeError {
		t.Fatalf("Outcome = %v, want OutcomeError", result.Outcome)
	}
	if repo.recorded[0].Error != "insufficient funds" {
		t.Fatalf("recorded error = %q, want %q", repo.recorded[0].Error, "insufficient funds")
	}
}

func TestService_Dispense_RepositoryLookupFailureDoesNotDispense(t *testing.T) {
	validAddr := validTestnetAddress(t)
	repo := &fakeRepo{lastErr: errors.New("connection reset")}
	wallet := &fakeWallet{sendResp: successResponse(1)}
	svc := &Service{
		Repo:   repo,
		Wallet: wallet,
		Clock:  &fakeClock{now: time.Now()},
		Config: Config{DispenseAmount: 1000000, RateLimitWindow: time.Hour},
	}

	result := svc.Dispense(context.Background(), validAddr, "10.0.0.1")
	if result.Outcome != OutcomeError {
		t.Fatalf("Outcome = %v, want OutcomeError", result.Outcome)
	}
	if len(wallet.sentRecipients) != 0 {
		t.Fatal("wallet must not be called when the rate-limit lookup itself fails")
	}
}

// TestService_Dispense_ConcurrentSameAddressAndIP is the direct
// regression test for the TOCTOU race ReserveDispense fixes: N
// concurrent Service.Dispense calls for the exact same address and ip
// must yield exactly one OutcomeSuccess and N-1 OutcomeRateLimited --
// never more than one success (the live-incident bug: 5 concurrent
// requests all dispensing), and never an OutcomeError as a side effect
// of the race itself. It drives fakeRepo through real concurrent
// goroutines (not a sequential call sequence), so it genuinely
// exercises fakeRepo's per-key mutex locking (see fakeRepo's doc
// comment) rather than just asserting on a mocked call count.
//
// This is a fast, always-on unit-level companion to the authoritative
// real-Postgres proof in repository_integration_test.go
// (TestIntegrationReserveDispense_ConcurrentSameAddressAndIP), which
// is what actually proves pg_advisory_xact_lock works as intended;
// this one covers the same regression even where Postgres isn't
// available (see that file's package doc for why it's gated).
func TestService_Dispense_ConcurrentSameAddressAndIP(t *testing.T) {
	validAddr := validTestnetAddress(t)
	repo := &fakeRepo{}
	wallet := &fakeWallet{sendResp: successResponse(7)}
	svc := &Service{
		Repo:   repo,
		Wallet: wallet,
		Clock:  &fakeClock{now: time.Now()},
		Config: Config{DispenseAmount: 1000000, RateLimitWindow: time.Hour},
	}

	const n = 8
	results := make([]Result, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			results[i] = svc.Dispense(context.Background(), validAddr, "10.0.0.1")
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
			t.Errorf("unexpected Outcome %v (err=%v) among concurrent results", r.Outcome, r.Err)
		}
	}
	if successes != 1 {
		t.Errorf("got %d OutcomeSuccess results across %d concurrent calls, want exactly 1", successes, n)
	}
	if rateLimited != n-1 {
		t.Errorf("got %d OutcomeRateLimited results, want exactly %d", rateLimited, n-1)
	}
	if other != 0 {
		t.Errorf("got %d unexpected (non-success/non-rate-limited) outcomes, want 0", other)
	}
	if len(wallet.sentRecipients) != 1 {
		t.Errorf("wallet.SendTransactions called %d times, want exactly 1 (only the winning request should ever reach the wallet)", len(wallet.sentRecipients))
	}
}

// TestService_Dispense_FixedAmountRegression_MaxDispenseAmountUnset covers
// the exact code path production testnet traffic hits today: Config.Rand
// is left nil (as every existing Config{DispenseAmount: ...} literal in
// this file does) and Config.MaxDispenseAmount is left at its zero
// value, exactly mirroring today's -dispense-amount-only flag set. Every
// one of several calls, across distinct addresses, must dispense exactly
// DispenseAmount -- same Result.Amount, same wallet-sent
// PaymentRecipient.Amount, same ReserveDispense-recorded audit amount,
// every time -- proving chooseDispenseAmount's "off" path is genuinely
// unchanged/no-randomness-touched.
func TestService_Dispense_FixedAmountRegression_MaxDispenseAmountUnset(t *testing.T) {
	const fixedAmount = 1_000_000
	repo := &fakeRepo{}
	wallet := &fakeWallet{sendResp: successResponse(1)}
	svc := &Service{
		Repo:   repo,
		Wallet: wallet,
		Clock:  &fakeClock{now: time.Now()},
		Config: Config{DispenseAmount: fixedAmount, RateLimitWindow: time.Hour},
	}

	const n = 5
	for i := 0; i < n; i++ {
		addr := validTestnetAddress(t)
		result := svc.Dispense(context.Background(), addr, fmt.Sprintf("10.0.0.%d", i+1))
		if result.Outcome != OutcomeSuccess {
			t.Fatalf("iteration %d: Outcome = %v, want OutcomeSuccess (err=%v)", i, result.Outcome, result.Err)
		}
		if result.Amount != fixedAmount {
			t.Fatalf("iteration %d: Result.Amount = %d, want %d", i, result.Amount, fixedAmount)
		}
	}

	if len(wallet.sentRecipients) != n {
		t.Fatalf("wallet.SendTransactions called %d times, want %d", len(wallet.sentRecipients), n)
	}
	for i, recipients := range wallet.sentRecipients {
		if len(recipients) != 1 || recipients[0].Amount != fixedAmount {
			t.Fatalf("call %d: sent recipients = %+v, want a single recipient with Amount=%d", i, recipients, fixedAmount)
		}
	}
	if len(repo.recorded) != n {
		t.Fatalf("expected %d recorded dispenses, got %d", n, len(repo.recorded))
	}
	for i, rec := range repo.recorded {
		if rec.Amount != fixedAmount {
			t.Fatalf("recorded[%d].Amount = %d, want %d", i, rec.Amount, fixedAmount)
		}
	}
}

// TestService_Dispense_RandomAmountThreadsThroughEndToEnd covers that,
// with MaxDispenseAmount > DispenseAmount and an injected fake
// AmountRandomizer, the token count the fake returns genuinely flows
// through to Result.Amount, the wallet-sent PaymentRecipient.Amount, and
// the ReserveDispense-recorded audit amount -- run for two different
// fake-returned values (on two separate Service instances) to prove
// it's actually threaded per-call, not coincidentally matching
// DispenseAmount as some fallback.
func TestService_Dispense_RandomAmountThreadsThroughEndToEnd(t *testing.T) {
	const dispenseAmount = 1_000_000     // 1 token
	const maxDispenseAmount = 10_000_000 // 10 tokens

	cases := []struct {
		name       string
		tokens     int64  // what the fake randomizer returns from IntRange
		wantAmount uint64 // tokens * 1_000_000
	}{
		{name: "low end of range", tokens: 1, wantAmount: 1_000_000},
		{name: "distinct higher value", tokens: 7, wantAmount: 7_000_000},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			validAddr := validTestnetAddress(t)
			repo := &fakeRepo{}
			wallet := &fakeWallet{sendResp: successResponse(1)}
			fakeRand := &fakeRandomizer{values: []int64{tc.tokens}}
			svc := &Service{
				Repo:   repo,
				Wallet: wallet,
				Clock:  &fakeClock{now: time.Now()},
				Rand:   fakeRand,
				Config: Config{DispenseAmount: dispenseAmount, MaxDispenseAmount: maxDispenseAmount, RateLimitWindow: time.Hour},
			}

			result := svc.Dispense(context.Background(), validAddr, "10.0.0.1")
			if result.Outcome != OutcomeSuccess {
				t.Fatalf("Outcome = %v, want OutcomeSuccess (err=%v)", result.Outcome, result.Err)
			}
			if result.Amount != tc.wantAmount {
				t.Fatalf("Result.Amount = %d, want %d", result.Amount, tc.wantAmount)
			}
			if fakeRand.calls != 1 {
				t.Fatalf("fakeRandomizer.IntRange called %d times, want exactly 1", fakeRand.calls)
			}
			if len(wallet.sentRecipients) != 1 || len(wallet.sentRecipients[0]) != 1 || wallet.sentRecipients[0][0].Amount != tc.wantAmount {
				t.Fatalf("wallet-sent amount = %+v, want a single recipient with Amount=%d", wallet.sentRecipients, tc.wantAmount)
			}
			if len(repo.recorded) != 1 || repo.recorded[0].Amount != tc.wantAmount {
				t.Fatalf("repo.recorded = %+v, want a single record with Amount=%d", repo.recorded, tc.wantAmount)
			}
		})
	}
}

// TestService_Dispense_OffPathNeverTouchesRandomizer covers the boundary
// (MaxDispenseAmount == DispenseAmount, not just the zero-value "unset"
// case) and proves chooseDispenseAmount's fixed-amount path is a genuine
// early return: the injected fakeRandomizer panics if IntRange is ever
// called, and a successful OutcomeSuccess dispense with call count 0
// proves it never was.
func TestService_Dispense_OffPathNeverTouchesRandomizer(t *testing.T) {
	const amount = 1_000_000

	tests := []struct {
		name              string
		maxDispenseAmount uint64
	}{
		{name: "MaxDispenseAmount unset (0)", maxDispenseAmount: 0},
		{name: "MaxDispenseAmount == DispenseAmount boundary", maxDispenseAmount: amount},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			validAddr := validTestnetAddress(t)
			repo := &fakeRepo{}
			wallet := &fakeWallet{sendResp: successResponse(1)}
			fakeRand := &fakeRandomizer{panicOnCall: true}
			svc := &Service{
				Repo:   repo,
				Wallet: wallet,
				Clock:  &fakeClock{now: time.Now()},
				Rand:   fakeRand,
				Config: Config{DispenseAmount: amount, MaxDispenseAmount: tt.maxDispenseAmount, RateLimitWindow: time.Hour},
			}

			result := svc.Dispense(context.Background(), validAddr, "10.0.0.1")
			if result.Outcome != OutcomeSuccess {
				t.Fatalf("Outcome = %v, want OutcomeSuccess (err=%v)", result.Outcome, result.Err)
			}
			if result.Amount != amount {
				t.Fatalf("Result.Amount = %d, want %d", result.Amount, amount)
			}
			if fakeRand.calls != 0 {
				t.Fatalf("fakeRandomizer.IntRange called %d times, want exactly 0", fakeRand.calls)
			}
		})
	}
}

func TestService_HealthCheck(t *testing.T) {
	t.Run("healthy when both dependencies are up", func(t *testing.T) {
		svc := &Service{
			Repo: &fakeRepo{},
			Wallet: &fakeWallet{connectivity: &tari_generated.CheckConnectivityResponse{
				Status: tari_generated.CheckConnectivityResponse_Online,
			}},
		}
		if err := svc.HealthCheck(context.Background()); err != nil {
			t.Fatalf("HealthCheck() = %v, want nil", err)
		}
	})

	t.Run("unhealthy when postgres ping fails", func(t *testing.T) {
		svc := &Service{
			Repo: &fakeRepo{pingErr: errors.New("db down")},
			Wallet: &fakeWallet{connectivity: &tari_generated.CheckConnectivityResponse{
				Status: tari_generated.CheckConnectivityResponse_Online,
			}},
		}
		if err := svc.HealthCheck(context.Background()); err == nil {
			t.Fatal("HealthCheck() = nil, want an error when postgres is down")
		}
	})

	t.Run("unhealthy when wallet GRPC is unreachable", func(t *testing.T) {
		svc := &Service{
			Repo:   &fakeRepo{},
			Wallet: &fakeWallet{connectivityErr: errors.New("grpc: no connection")},
		}
		if err := svc.HealthCheck(context.Background()); err == nil {
			t.Fatal("HealthCheck() = nil, want an error when the wallet GRPC is down")
		}
	})

	t.Run("unhealthy when wallet reports offline", func(t *testing.T) {
		svc := &Service{
			Repo: &fakeRepo{},
			Wallet: &fakeWallet{connectivity: &tari_generated.CheckConnectivityResponse{
				Status: tari_generated.CheckConnectivityResponse_Offline,
			}},
		}
		if err := svc.HealthCheck(context.Background()); err == nil {
			t.Fatal("HealthCheck() = nil, want an error when the wallet reports Offline")
		}
	})
}

func TestService_CurrentBalance_ReturnsAvailableBalance(t *testing.T) {
	svc := &Service{
		Repo: &fakeRepo{},
		Wallet: &fakeWallet{balanceResp: &tari_generated.GetBalanceResponse{
			AvailableBalance: 1234567,
		}},
	}

	got, err := svc.CurrentBalance(context.Background())
	if err != nil {
		t.Fatalf("CurrentBalance() error = %v, want nil", err)
	}
	if got != 1234567 {
		t.Fatalf("CurrentBalance() = %d, want 1234567", got)
	}
}

func TestService_CurrentBalance_PropagatesWalletError(t *testing.T) {
	svc := &Service{
		Repo:   &fakeRepo{},
		Wallet: &fakeWallet{balanceErr: errors.New("grpc: unavailable")},
	}

	_, err := svc.CurrentBalance(context.Background())
	if err == nil {
		t.Fatal("CurrentBalance() error = nil, want an error when the wallet call fails")
	}
}

func TestService_CurrentBalance_ErrorsOnNilResponse(t *testing.T) {
	svc := &Service{
		Repo:   &fakeRepo{},
		Wallet: &fakeWallet{},
	}

	_, err := svc.CurrentBalance(context.Background())
	if err == nil {
		t.Fatal("CurrentBalance() error = nil, want an error when the wallet returns a nil response")
	}
}
