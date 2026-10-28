package faucet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/Jagtech-Services/go-tari-lib/v3/address"
	"github.com/sirupsen/logrus"
)

// turnstileSiteverifyURL is Cloudflare's Turnstile siteverify endpoint.
// verifyTurnstile always POSTs here -- never mocked/overridden in
// production, only via the httpDoer seam in tests.
const turnstileSiteverifyURL = "https://challenges.cloudflare.com/turnstile/v0/siteverify"

// httpDoer is the minimal subset of *http.Client used by verifyTurnstile,
// small enough that tests can implement it with a fake instead of standing
// up a real HTTP server or a full http.RoundTripper.
type httpDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// genericRejectionMessage builds the message rendered for any request
// rejection that shouldn't reveal its real cause to the caller --
// currently just the honeypot trip in Request. Kept as a shared helper
// so the wording stays identical to the "something went wrong" default
// outcome path, parameterized by ticker (e.g. "tXTM"/"XTM") rather than
// a hardcoded package-level constant.
func genericRejectionMessage(ticker string) string {
	return fmt.Sprintf("Something went wrong dispensing %s. Please try again shortly.", ticker)
}

// Handler wires Service into net/http handlers for the faucet's three
// routes: GET / (the form), POST /request (submit an address), and GET
// /healthz (liveness probe).
type Handler struct {
	Service *Service

	// StatusCache holds the last-known wallet balance/connectivity,
	// refreshed in the background (see StatusCache.StartPolling).
	// Index and Healthz read it directly instead of calling
	// Service.Wallet per request, so no HTTP request ever blocks on a
	// live wallet GRPC call. Must be non-nil -- NewHandler always
	// supplies one.
	StatusCache *StatusCache

	// HTTPClient is used for outbound HTTP calls (currently just Cloudflare
	// Turnstile siteverify). Defaults to http.DefaultClient when nil -- tests
	// inject a mock via httpClient interface / http.Client{Transport: ...}.
	HTTPClient httpDoer
}

// NewHandler builds a Handler around svc, reading wallet balance/
// connectivity from cache instead of svc's WalletClient directly (see
// StatusCache).
func NewHandler(svc *Service, cache *StatusCache) *Handler {
	return &Handler{Service: svc, StatusCache: cache}
}

// Routes registers this Handler's routes on mux.
func (h *Handler) Routes(mux *http.ServeMux) {
	mux.HandleFunc("/", h.Index)
	mux.HandleFunc("/request", h.Request)
	mux.HandleFunc("/healthz", h.Healthz)
}

// Index renders the plain HTML request form, including the wallet's
// current spendable balance, read from StatusCache -- an in-memory,
// zero-network-I/O read, never a live wallet GRPC call per request. A
// stale/failed cache entry degrades gracefully -- the form still
// renders, with "balance unavailable" in place of the figure, rather
// than erroring the whole page (same principle Healthz's dependency
// checks already follow).
func (h *Handler) Index(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	h.renderIndex(w, indexData{})
}

// Request handles the form submission: rejects honeypot-tripped spam,
// validates the address, applies the rate limit, and dispenses test Tari
// on success. Every outcome -- honeypot, malformed address, rate limited,
// or a wallet/DB error -- is rendered back into the same form with a
// clear message instead of a 500.
func (h *Handler) Request(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		h.renderIndex(w, indexData{Message: "Could not parse form submission.", IsError: true})
		return
	}
	rawAddress := r.FormValue("address")
	ip := ClientIP(r)

	if honeypot := r.FormValue(honeypotFieldName); honeypot != "" {
		logger := h.Service.Logger
		if logger == nil {
			logger = logrus.StandardLogger()
		}
		logger.WithFields(logrus.Fields{
			"ip": ip,
		}).Info("faucet: honeypot field populated, rejecting as spam")
		h.renderIndex(w, indexData{
			Address: rawAddress,
			Message: genericRejectionMessage(h.Service.Config.Ticker),
			IsError: true,
		})
		return
	}

	if h.Service.Config.TurnstileEnabled {
		token := r.FormValue("cf-turnstile-response")
		logger := h.Service.Logger
		if logger == nil {
			logger = logrus.StandardLogger()
		}
		if token == "" {
			logger.WithFields(logrus.Fields{
				"ip": ip,
			}).Warn("faucet: turnstile token missing, rejecting")
			h.renderIndex(w, indexData{
				Address: rawAddress,
				Message: genericRejectionMessage(h.Service.Config.Ticker),
				IsError: true,
			})
			return
		}
		ok, err := h.verifyTurnstile(r.Context(), h.Service.Config.TurnstileSecretKey, token, ip)
		if err != nil {
			logger.WithFields(logrus.Fields{
				"ip":    ip,
				"error": err,
			}).Warn("faucet: turnstile verification request failed, rejecting")
			h.renderIndex(w, indexData{
				Address: rawAddress,
				Message: genericRejectionMessage(h.Service.Config.Ticker),
				IsError: true,
			})
			return
		}
		if !ok {
			logger.WithFields(logrus.Fields{
				"ip": ip,
			}).Warn("faucet: turnstile verification failed, rejecting as spam")
			h.renderIndex(w, indexData{
				Address: rawAddress,
				Message: genericRejectionMessage(h.Service.Config.Ticker),
				IsError: true,
			})
			return
		}
	}

	result := h.Service.Dispense(r.Context(), rawAddress, ip)
	switch result.Outcome {
	case OutcomeSuccess:
		h.renderIndex(w, indexData{
			Message: fmt.Sprintf("Success! Sent %s (tx id %d).", formatXTM(result.Amount), result.TxID),
		})
	case OutcomeInvalidAddress:
		h.renderIndex(w, indexData{
			Address: rawAddress,
			Message: fmt.Sprintf("That doesn't look like a valid Tari address: %v", describeAddressError(result.Err)),
			IsError: true,
		})
	case OutcomeRateLimited:
		h.renderIndex(w, indexData{
			Address:    rawAddress,
			Message:    fmt.Sprintf("This address or IP has already received %s recently. Try again after %s.", h.Service.Config.Ticker, result.RetryAfter.Format("2006-01-02 15:04:05 MST")),
			IsError:    true,
			StatusCode: http.StatusTooManyRequests,
		})
	default:
		h.renderIndex(w, indexData{
			Address: rawAddress,
			Message: genericRejectionMessage(h.Service.Config.Ticker),
			IsError: true,
		})
	}
}

// Healthz reports 200 if both Postgres and the wallet GRPC connection are
// reachable, 503 otherwise. Postgres is checked live (Repository.Ping)
// on every call -- that's already fast and unaffected by this change.
// The wallet check reads StatusCache's last-known connectivity poll
// instead of dialing the wallet per request, so a slow/hung wallet GRPC
// call can never delay this response.
func (h *Handler) Healthz(w http.ResponseWriter, r *http.Request) {
	if err := h.Service.Repo.Ping(r.Context()); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = fmt.Fprintf(w, "unhealthy: %v\n", err)
		return
	}
	if _, _, walletUp, walletUpOK := h.StatusCache.Get(); !walletUpOK || !walletUp {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = fmt.Fprintln(w, "unhealthy: wallet GRPC connectivity is not online")
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintln(w, "ok")
}

// describeAddressError renders go-tari-lib/address's parse errors as a
// user-facing string; unknown errors fall back to err.Error().
func describeAddressError(err error) string {
	if err == nil {
		return "unknown error"
	}
	if errors.Is(err, address.ErrInvalidAddressString) {
		return "could not recognize the address format"
	}
	if errors.Is(err, ErrPaymentIDNotAllowed) {
		return "addresses containing a payment id are not accepted by this faucet — please submit a plain address without a payment id"
	}
	return err.Error()
}

// renderIndex renders data through indexTemplate, filling in Ticker,
// NetworkLabel, and NetworkNickname from h.Service.Config so every call site
// gets the configured branding words without having to set them on each
// indexData literal itself. It also populates FaucetBalance/FaucetBalanceErr
// from h.StatusCache here -- centrally, for every call site, present and
// future -- rather than relying on each caller (Index, Request's many
// outcome branches, etc.) to remember to set it individually. That used to
// be Index's job alone, which meant every indexData{} literal built inside
// Request silently rendered a zero/zero-value balance ("0 XTM (spendable)")
// on every outcome, regardless of the real cached balance.
func (h *Handler) renderIndex(w http.ResponseWriter, data indexData) {
	data.Ticker = h.Service.Config.Ticker
	data.NetworkLabel = h.Service.Config.NetworkLabel
	data.NetworkNickname = h.Service.Config.NetworkNickname
	data.TurnstileEnabled = h.Service.Config.TurnstileEnabled
	data.TurnstileSiteKey = h.Service.Config.TurnstileSiteKey
	data.DispenseAmount = h.Service.Config.DispenseAmount
	data.MaxDispenseAmount = h.Service.Config.MaxDispenseAmount
	balance, balanceOK, _, _ := h.StatusCache.Get()
	if !balanceOK {
		data.FaucetBalanceErr = true
	} else {
		data.FaucetBalance = balance
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	switch {
	case data.StatusCode != 0:
		w.WriteHeader(data.StatusCode)
	case data.IsError:
		w.WriteHeader(http.StatusBadRequest)
	}
	_ = indexTemplate.Execute(w, data)
}

// verifyTurnstile POSTs form-encoded {secret, response, remoteip} to
// Cloudflare's Turnstile siteverify endpoint and returns whether it reports
// success. Any HTTP/network/decode error is treated as a FAILED verification
// (fail closed, not open) -- a Cloudflare outage must never accidentally let
// spam through. The returned error (non-nil only on the fail-closed path) lets
// the caller log the underlying transport/decode failure distinctly from a
// plain "verification said no" rejection, so on-call can tell a Cloudflare
// outage from a real bot rejection.
func (h *Handler) verifyTurnstile(ctx context.Context, secretKey, token, remoteIP string) (bool, error) {
	form := url.Values{
		"secret":   {secretKey},
		"response": {token},
		"remoteip": {remoteIP},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, turnstileSiteverifyURL, strings.NewReader(form.Encode()))
	if err != nil {
		return false, fmt.Errorf("faucet: building turnstile siteverify request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := h.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, fmt.Errorf("faucet: turnstile siteverify request failed: %w", err)
	}
	defer resp.Body.Close()

	var body struct {
		Success     bool     `json:"success"`
		ErrorCodes  []string `json:"error-codes"`
		ChallengeTS string   `json:"challenge_ts"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return false, fmt.Errorf("faucet: decoding turnstile siteverify response: %w", err)
	}
	return body.Success, nil
}
