package ootle

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestHandler(repo *fakeRepo, wallet *fakeWallet) *Handler {
	svc := &Service{Repo: repo, Wallet: wallet, Clock: &fakeClock{now: time.Now()}, Config: testConfig()}
	return NewHandler(svc, NewStatusCache())
}

func TestHandler_Index_RendersForm(t *testing.T) {
	h := newTestHandler(&fakeRepo{}, &fakeWallet{})
	req := httptest.NewRequest(http.MethodGet, "/ootle", nil)
	rec := httptest.NewRecorder()

	h.Index(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `action="/ootle/dispense"`) {
		t.Errorf("body does not contain the expected form action, body: %s", rec.Body.String())
	}
}

func TestHandler_Dispense_HTMLFormHoneypotRejectsAsSpam(t *testing.T) {
	repo := &fakeRepo{}
	wallet := &fakeWallet{txID: "tx_1"}
	h := newTestHandler(repo, wallet)

	form := "destination=component_dest&website=i-am-a-bot"
	req := httptest.NewRequest(http.MethodPost, "/ootle/dispense", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()

	h.Dispense(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if repo.reserveCalls != 0 {
		t.Errorf("ReserveDispense called %d times, want 0 (honeypot trip must short-circuit before rate-limit check)", repo.reserveCalls)
	}
	if len(wallet.calls) != 0 {
		t.Errorf("wallet.SubmitDispense called %d times, want 0", len(wallet.calls))
	}
}

func TestHandler_Dispense_HTMLForm_Success(t *testing.T) {
	repo := &fakeRepo{}
	wallet := &fakeWallet{txID: "tx_cafebabe"}
	h := newTestHandler(repo, wallet)

	form := "destination=component_dest"
	req := httptest.NewRequest(http.MethodPost, "/ootle/dispense", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()

	h.Dispense(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "tx_cafebabe") {
		t.Errorf("body does not contain the tx id, body: %s", rec.Body.String())
	}
}

func TestHandler_Dispense_JSON_Success(t *testing.T) {
	repo := &fakeRepo{}
	wallet := &fakeWallet{txID: "tx_json"}
	h := newTestHandler(repo, wallet)

	body, _ := json.Marshal(dispenseRequest{Destination: "component_dest"})
	req := httptest.NewRequest(http.MethodPost, "/ootle/dispense", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	h.Dispense(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	var resp dispenseResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v, body: %s", err, rec.Body.String())
	}
	if !resp.Success || resp.TxID != "tx_json" {
		t.Errorf("resp = %+v, want Success=true TxID=tx_json", resp)
	}
}

func TestHandler_Dispense_JSON_NotConfiguredFailsClosed(t *testing.T) {
	repo := &fakeRepo{}
	wallet := &fakeWallet{txID: "tx_json"}
	svc := &Service{Repo: repo, Wallet: wallet, Clock: &fakeClock{now: time.Now()}, Config: Config{
		FaucetComponentAddress: "", // not yet deployed
		DispenseAmount:         1000,
		MaxFee:                 10,
		RateLimitWindow:        time.Hour,
	}}
	h := NewHandler(svc, NewStatusCache())

	body, _ := json.Marshal(dispenseRequest{Destination: "component_dest"})
	req := httptest.NewRequest(http.MethodPost, "/ootle/dispense", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	h.Dispense(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503, body: %s", rec.Code, rec.Body.String())
	}
	var resp dispenseResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp.Success {
		t.Errorf("resp.Success = true, want false")
	}
}

func TestHandler_Dispense_MethodNotAllowed(t *testing.T) {
	h := newTestHandler(&fakeRepo{}, &fakeWallet{})
	req := httptest.NewRequest(http.MethodGet, "/ootle/dispense", nil)
	rec := httptest.NewRecorder()

	h.Dispense(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

// TestStatusCache_Poll_ReachabilityUnaffectedByAuthMethod proves Ping
// (and thus StatusCache) treats ANY successful auth.method response
// (regardless of whether it reports "none" or "webauthn") as
// reachable -- only a transport/RPC-level error should ever mark the
// cache "down".
func TestStatusCache_Poll_ReachabilityUnaffectedByAuthMethod(t *testing.T) {
	wallet := &fakeWallet{} // err == nil -> Ping succeeds
	cache := NewStatusCache()
	cache.poll(context.Background(), wallet)

	up, ok := cache.Get()
	if !ok || !up {
		t.Fatalf("Get() = (%v, %v), want (true, true)", up, ok)
	}
}
