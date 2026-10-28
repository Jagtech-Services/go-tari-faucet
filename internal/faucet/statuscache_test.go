package faucet

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Jagtech-Services/go-tari-grpc-lib/v3/tari_generated"
)

func TestStatusCache_Get_ZeroValueIsUnavailable(t *testing.T) {
	cache := NewStatusCache()
	balance, balanceOK, walletUp, walletUpOK := cache.Get()
	if balanceOK || walletUpOK {
		t.Fatalf("a never-polled StatusCache must report both values unavailable, got balanceOK=%v walletUpOK=%v", balanceOK, walletUpOK)
	}
	if balance != 0 || walletUp {
		t.Fatalf("a never-polled StatusCache should have zero balance and walletUp=false, got balance=%d walletUp=%v", balance, walletUp)
	}
}

func TestStatusCache_Poll_SeedsExpectedValuesOnSuccess(t *testing.T) {
	wallet := &fakeWallet{
		balanceResp: &tari_generated.GetBalanceResponse{AvailableBalance: 42},
		connectivity: &tari_generated.CheckConnectivityResponse{
			Status: tari_generated.CheckConnectivityResponse_Online,
		},
	}
	cache := NewStatusCache()

	cache.poll(wallet)

	balance, balanceOK, walletUp, walletUpOK := cache.Get()
	if !balanceOK {
		t.Fatal("balanceOK = false, want true after a successful poll")
	}
	if balance != 42 {
		t.Fatalf("balance = %d, want 42", balance)
	}
	if !walletUpOK {
		t.Fatal("walletUpOK = false, want true after a successful poll")
	}
	if !walletUp {
		t.Fatal("walletUp = false, want true when the wallet reports Online")
	}
}

func TestStatusCache_Poll_FailedPollMarksUnavailableWithoutPanicking(t *testing.T) {
	wallet := &fakeWallet{
		balanceErr:      errors.New("grpc: unavailable"),
		connectivityErr: errors.New("grpc: no connection"),
	}
	cache := NewStatusCache()

	// Must not panic or block, even though both calls fail.
	cache.poll(wallet)

	balance, balanceOK, walletUp, walletUpOK := cache.Get()
	if balanceOK {
		t.Fatal("balanceOK = true, want false after a failed balance poll")
	}
	if balance != 0 {
		t.Fatalf("balance = %d, want 0 on a failed poll", balance)
	}
	if walletUpOK {
		t.Fatal("walletUpOK = true, want false after a failed connectivity poll")
	}
	if walletUp {
		t.Fatal("walletUp = true, want false on a failed poll")
	}
}

func TestStatusCache_Poll_OfflineConnectivityIsNotWalletUp(t *testing.T) {
	wallet := &fakeWallet{
		balanceResp: &tari_generated.GetBalanceResponse{AvailableBalance: 7},
		connectivity: &tari_generated.CheckConnectivityResponse{
			Status: tari_generated.CheckConnectivityResponse_Offline,
		},
	}
	cache := NewStatusCache()

	cache.poll(wallet)

	_, _, walletUp, walletUpOK := cache.Get()
	if !walletUpOK {
		t.Fatal("walletUpOK = false, want true -- the connectivity call itself succeeded")
	}
	if walletUp {
		t.Fatal("walletUp = true, want false when the wallet reports Offline")
	}
}

func TestStatusCache_Poll_PartialFailureOnlyMarksThatHalfUnavailable(t *testing.T) {
	wallet := &fakeWallet{
		balanceErr: errors.New("grpc: unavailable"),
		connectivity: &tari_generated.CheckConnectivityResponse{
			Status: tari_generated.CheckConnectivityResponse_Online,
		},
	}
	cache := NewStatusCache()

	cache.poll(wallet)

	_, balanceOK, walletUp, walletUpOK := cache.Get()
	if balanceOK {
		t.Fatal("balanceOK = true, want false when the balance call failed")
	}
	if !walletUpOK || !walletUp {
		t.Fatal("connectivity should still be reported as up, independently of the failed balance call")
	}
}

// blockingWallet's GetBalance blocks until release is closed, so tests can
// deterministically exercise pollWithTimeout's bound without relying on a
// real sleep race.
type blockingWallet struct {
	*fakeWallet
	release chan struct{}
}

func (b *blockingWallet) GetBalance() (*tari_generated.GetBalanceResponse, error) {
	<-b.release
	return b.fakeWallet.GetBalance()
}

func TestStatusCache_PollWithTimeout_DoesNotBlockPastTimeout(t *testing.T) {
	wallet := &blockingWallet{
		fakeWallet: &fakeWallet{
			balanceResp: &tari_generated.GetBalanceResponse{AvailableBalance: 99},
			connectivity: &tari_generated.CheckConnectivityResponse{
				Status: tari_generated.CheckConnectivityResponse_Online,
			},
		},
		release: make(chan struct{}),
	}
	cache := NewStatusCache()

	start := time.Now()
	cache.pollWithTimeout(context.Background(), wallet, 20*time.Millisecond)
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("pollWithTimeout took %s, want it to give up around its 20ms timeout", elapsed)
	}

	// The wallet call is still hanging in the background, so the cache
	// should still be at its zero/unavailable state.
	_, balanceOK, _, _ := cache.Get()
	if balanceOK {
		t.Fatal("balanceOK = true, want false -- the poll goroutine hasn't completed yet")
	}

	// Let the background poll goroutine finish so it doesn't leak past
	// the test, and confirm it eventually does update the cache.
	close(wallet.release)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok, _, _ := cache.Get(); ok {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("background poll never completed after releasing the blocked wallet call")
}

func TestStatusCache_StartPolling_StopsOnContextCancel(t *testing.T) {
	wallet := &fakeWallet{
		balanceResp: &tari_generated.GetBalanceResponse{AvailableBalance: 1},
		connectivity: &tari_generated.CheckConnectivityResponse{
			Status: tari_generated.CheckConnectivityResponse_Online,
		},
	}
	cache := NewStatusCache()
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		cache.StartPolling(ctx, wallet, 5*time.Millisecond)
		close(done)
	}()

	// Let it poll at least once, then cancel and confirm StartPolling
	// returns instead of looping forever.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok, _, _ := cache.Get(); ok {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("StartPolling did not return after ctx was cancelled")
	}
}

// TestStatusCache_ConcurrentReadsAndWrites exercises many concurrent Get
// calls against a single background poller, so `go test -race` can catch
// any unsynchronized access to the cached snapshot.
func TestStatusCache_ConcurrentReadsAndWrites(t *testing.T) {
	wallet := &fakeWallet{
		balanceResp: &tari_generated.GetBalanceResponse{AvailableBalance: 123},
		connectivity: &tari_generated.CheckConnectivityResponse{
			Status: tari_generated.CheckConnectivityResponse_Online,
		},
	}
	cache := NewStatusCache()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go cache.StartPolling(ctx, wallet, time.Millisecond)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				cache.Get()
			}
		}()
	}
	wg.Wait()
}
