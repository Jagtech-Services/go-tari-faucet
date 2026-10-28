package faucet

import (
	"context"
	"crypto/rand"
	"errors"
	"math/big"
	"time"

	"github.com/Jagtech-Services/go-tari-grpc-lib/v3/tari_generated"
	"github.com/sirupsen/logrus"
)

// Outcome classifies the result of a Dispense call so handlers can render
// an appropriate response without inspecting error strings.
type Outcome int

const (
	// OutcomeSuccess means the wallet accepted the transfer.
	OutcomeSuccess Outcome = iota
	// OutcomeInvalidAddress means the submitted address failed
	// address.Parse validation.
	OutcomeInvalidAddress
	// OutcomeRateLimited means the address or IP has dispensed
	// successfully within the configured rate-limit window.
	OutcomeRateLimited
	// OutcomeError means something else went wrong (DB or wallet GRPC
	// failure) -- the request should still get a clean error, not a 500.
	OutcomeError
)

// Result is the outcome of a single Dispense call.
type Result struct {
	Outcome    Outcome
	RetryAfter time.Time // valid only when Outcome == OutcomeRateLimited
	Err        error     // non-nil for OutcomeInvalidAddress/OutcomeError
	TxID       uint64    // valid only when Outcome == OutcomeSuccess
	// Amount is the actual chosen dispense amount (in microMinotari) for
	// this call, from Service.chooseDispenseAmount -- always the fixed
	// Config.DispenseAmount when the random range is off, otherwise the
	// per-call random amount that was actually sent to the wallet. Set
	// on OutcomeSuccess; also set on OutcomeError to reflect the amount
	// that was attempted, for logging/debugging purposes.
	Amount uint64
}

// Config holds Service's tunables, all of which are wired from main.go's
// flags.
type Config struct {
	DispenseAmount  uint64
	RateLimitWindow time.Duration

	// MaxDispenseAmount, when set greater than DispenseAmount, opts the
	// deployment into dispensing a random amount in the inclusive
	// range [DispenseAmount, MaxDispenseAmount] (quantized to whole
	// tokens -- see chooseDispenseAmount) instead of always the exact
	// DispenseAmount. Zero (the default) or any value <=
	// DispenseAmount means the feature is off -- DispenseAmount's
	// fixed-amount behavior is unchanged, byte-for-byte, and the
	// randomizer is never invoked. This is what today's testnet
	// deployment exercises, since it only ever sets -dispense-amount.
	MaxDispenseAmount uint64

	// Ticker is the exact display word used for user-facing branding
	// text (e.g. "tXTM" on testnet, "XTM" on mainnet). It's taken
	// verbatim from -ticker with no validation against a fixed set of
	// known values -- the same binary must be able to display whatever
	// ticker string a future network needs without a code change.
	Ticker string

	// NetworkLabel is the exact display word used alongside Ticker in
	// user-facing branding text (e.g. "Testnet" or "Mainnet"). Like
	// Ticker, it's taken verbatim from -network-label with no
	// validation against a fixed set of known values -- there's no
	// hardcoded testnet/mainnet enum, just whatever label string is
	// configured.
	NetworkLabel string

	// NetworkNickname is an optional display word used in parentheses
	// after NetworkLabel in the intro paragraph (e.g. "Esme" for
	// Esmeralda testnet). Taken verbatim from -network-nickname with no
	// validation against a fixed set of known values. Empty string means
	// no nickname display -- the parenthetical is omitted entirely from
	// the intro paragraph, allowing the same binary to serve networks
	// with and without a nickname.
	NetworkNickname string

	// TurnstileEnabled turns on server-side Cloudflare Turnstile CAPTCHA
	// verification for /request. Off by default (zero value) so existing
	// deployments are unaffected unless explicitly enabled via -turnstile-enabled.
	TurnstileEnabled bool

	// TurnstileSiteKey is the Cloudflare Turnstile site key rendered into the
	// request form's widget when TurnstileEnabled is true. Only meaningful when
	// TurnstileEnabled is true.
	TurnstileSiteKey string

	// TurnstileSecretKey is the Cloudflare Turnstile secret key used to verify
	// submitted tokens against Cloudflare's siteverify endpoint. Only meaningful
	// when TurnstileEnabled is true. Never logged.
	TurnstileSecretKey string
}

// AmountRandomizer is the narrow randomness source chooseDispenseAmount
// depends on, so tests can inject deterministic values instead of racing
// real randomness.
type AmountRandomizer interface {
	// IntRange returns a value uniformly distributed over the inclusive
	// range [min, max]. Implementations must handle min == max (only one
	// possible value) without panicking.
	IntRange(min, max int64) int64
}

// cryptoRandAmountRandomizer is the production AmountRandomizer, backed
// by crypto/rand rather than math/rand -- dispense amounts are a
// user-facing, financial value, so there's no reason to accept even the
// small predictability risk of a PRNG seeded from wall-clock time.
type cryptoRandAmountRandomizer struct{}

// IntRange implements AmountRandomizer via crypto/rand.Int. min == max
// returns min directly without consulting crypto/rand at all, since
// big.NewInt(0) as an upper bound would be a degenerate (and, per
// crypto/rand.Int's docs, invalid/panicking) call.
func (cryptoRandAmountRandomizer) IntRange(min, max int64) int64 {
	if min == max {
		return min
	}
	span := big.NewInt(max - min + 1)
	n, err := rand.Int(rand.Reader, span)
	if err != nil {
		// crypto/rand.Reader failing is a fatal platform-level problem
		// (entropy source unavailable) that every other crypto/rand
		// caller in this ecosystem would also be unable to recover
		// from meaningfully; falling back to the lower bound keeps
		// Dispense from ever sending more than the requested range
		// permits rather than panicking a live request.
		return min
	}
	return min + n.Int64()
}

// Service is the faucet's core business logic: address validation, rate
// limiting, and wallet dispensing. It depends only on the narrow
// Repository/WalletClient/Clock interfaces, so it can be fully unit tested
// without a live Postgres or wallet GRPC daemon.
type Service struct {
	Repo   Repository
	Wallet WalletClient
	Clock  Clock
	// Rand is the randomness source chooseDispenseAmount uses when the
	// random-range feature is on (see Config.MaxDispenseAmount). Nil
	// falls back to cryptoRandAmountRandomizer, mirroring how a nil
	// Clock falls back to time.Now() in now().
	Rand   AmountRandomizer
	Config Config
	Logger *logrus.Logger
}

// Dispense validates rawAddress, checks the rate limit for both the address
// and ip, and -- if allowed -- sends Config.DispenseAmount to the address
// via the wallet, recording the attempt in Repository regardless of
// outcome.
func (s *Service) Dispense(ctx context.Context, rawAddress, ip string) Result {
	logger := s.Logger
	if logger == nil {
		logger = logrus.StandardLogger()
	}

	_, base58Addr, err := ValidateAddress(rawAddress)
	if err != nil {
		logger.WithFields(logrus.Fields{
			"address": rawAddress,
			"ip":      ip,
		}).Warn("faucet: rejected malformed address")
		return Result{Outcome: OutcomeInvalidAddress, Err: err}
	}

	now := s.now()
	since := now.Add(-s.Config.RateLimitWindow)
	// chooseDispenseAmount is called exactly once per Dispense call, up
	// front, so every downstream use -- the rate-limit reservation, the
	// wallet send, the audit finalize, and the returned Result -- all
	// agree on the exact same amount for this request.
	amount := s.chooseDispenseAmount()
	// ReserveDispense does the rate-limit check AND reserves a
	// placeholder audit row atomically (see its doc comment) -- this is
	// the fix for the race that let concurrent requests for the same
	// address/ip all pass a plain "check" SELECT before any of their
	// "record" INSERTs landed. Nothing below this point may re-derive
	// the rate-limit decision from a separate, unsynchronized read.
	reservation, err := s.Repo.ReserveDispense(ctx, base58Addr, ip, amount, since, now)
	if err != nil {
		logger.WithFields(logrus.Fields{
			"address": base58Addr,
			"ip":      ip,
			"error":   err,
		}).Warn("faucet: rate-limit reservation failed")
		return Result{Outcome: OutcomeError, Err: err, Amount: amount}
	}
	if reservation.RateLimited {
		retryAfter := reservation.LastSuccessAt.Add(s.Config.RateLimitWindow)
		logger.WithFields(logrus.Fields{
			"address":     base58Addr,
			"ip":          ip,
			"retry_after": retryAfter,
		}).Info("faucet: rate limited")
		return Result{Outcome: OutcomeRateLimited, RetryAfter: retryAfter}
	}

	// the faucet only ever sends a single recipient per dispense call, so
	// single_tx is semantically a no-op here — pass false deliberately
	// rather than leaving it implicit.
	resp, sendErr := s.Wallet.SendTransactions([]*tari_generated.PaymentRecipient{
		{
			Address:     base58Addr,
			Amount:      amount,
			FeePerGram:  5,
			PaymentType: tari_generated.PaymentRecipient_ONE_SIDED,
		},
	}, false)

	success, errMsg, txID, result := s.buildResult(resp, sendErr)
	result.Amount = amount

	// Deliberately outside of ReserveDispense's advisory-lock
	// transaction -- see FinalizeDispense's doc comment for why this
	// (potentially slow, wallet-RPC-dependent) write must never hold
	// that lock.
	if finalizeErr := s.Repo.FinalizeDispense(ctx, reservation.ID, success, errMsg, txID); finalizeErr != nil {
		logger.WithFields(logrus.Fields{
			"address": base58Addr,
			"ip":      ip,
			"error":   finalizeErr,
		}).Error("faucet: failed to finalize dispense audit row")
	}

	logFields := logrus.Fields{
		"address": base58Addr,
		"ip":      ip,
		"amount":  amount,
		"success": success,
	}
	if errMsg != "" {
		logFields["error"] = errMsg
	}
	if success {
		logger.WithFields(logFields).Info("faucet: dispense attempt")
	} else {
		logger.WithFields(logFields).Warn("faucet: dispense attempt")
	}

	return result
}

// buildResult interprets the wallet's SendTransactions response, returning
// the success/error/txID values the caller records via FinalizeDispense
// alongside the Result to send back to the handler.
func (s *Service) buildResult(resp *tari_generated.TransferResponse, sendErr error) (success bool, errMsg string, txID uint64, result Result) {
	if sendErr != nil {
		return false, sendErr.Error(), 0, Result{Outcome: OutcomeError, Err: sendErr}
	}
	if resp == nil || len(resp.GetResults()) == 0 {
		err := errors.New("wallet returned no transfer result")
		return false, err.Error(), 0, Result{Outcome: OutcomeError, Err: err}
	}
	txResult := resp.GetResults()[0]
	if !txResult.GetIsSuccess() {
		err := errors.New(txResult.GetFailureMessage())
		return false, txResult.GetFailureMessage(), 0, Result{Outcome: OutcomeError, Err: err}
	}
	return true, "", txResult.GetTransactionId(), Result{Outcome: OutcomeSuccess, TxID: txResult.GetTransactionId()}
}

func (s *Service) now() time.Time {
	if s.Clock == nil {
		return time.Now()
	}
	return s.Clock.Now()
}

// chooseDispenseAmount decides the amount (in microMinotari) to dispense
// for a single Dispense call.
//
// When Config.MaxDispenseAmount is 0 or <= Config.DispenseAmount, the
// random-range feature is off: this returns Config.DispenseAmount
// directly, an early return that never touches s.Rand at all -- not
// even a zero-width-range call -- so this path (the only one today's
// testnet deployment exercises, since it only ever sets
// -dispense-amount) is provably untouched by any randomness.
//
// Otherwise, it picks a new random amount, independently per call,
// uniformly distributed over the inclusive range [DispenseAmount,
// MaxDispenseAmount], quantized to whole-token steps: we intentionally
// never dispense a fractional token amount -- it would look like a bug,
// not a feature. This is done by picking a random integer count of
// whole tokens in [DispenseAmount/1_000_000, MaxDispenseAmount/1_000_000]
// (inclusive) via s.Rand, then multiplying that count back up by
// 1_000_000.
func (s *Service) chooseDispenseAmount() uint64 {
	if s.Config.MaxDispenseAmount == 0 || s.Config.MaxDispenseAmount <= s.Config.DispenseAmount {
		return s.Config.DispenseAmount
	}
	minTokens := int64(s.Config.DispenseAmount / microMinotariPerXTM)
	maxTokens := int64(s.Config.MaxDispenseAmount / microMinotariPerXTM)
	var tokens int64
	if s.Rand == nil {
		tokens = cryptoRandAmountRandomizer{}.IntRange(minTokens, maxTokens)
	} else {
		tokens = s.Rand.IntRange(minTokens, maxTokens)
	}
	return uint64(tokens) * microMinotariPerXTM
}

// CurrentBalance returns the wallet's currently spendable (available)
// balance, in microMinotari, via a live WalletClient.GetBalance call.
// ctx is accepted for symmetry with HealthCheck/Dispense's context-
// shaped signatures, even though the underlying WalletClient.GetBalance
// call (like SendTransactions/GetWalletConnectivity) doesn't take one
// yet.
//
// Handler.Index does NOT call this directly -- it reads StatusCache
// instead, so the balance display never blocks a request on a live
// wallet GRPC call (WalletClient.GetBalance has no timeout and has been
// observed to occasionally hang for 10+ seconds). StatusCache's
// background poll loop calls the wallet directly rather than through
// this method (Repo isn't involved in the balance/connectivity poll at
// all), so this method today exists as the live/synchronous primitive
// this package's tests exercise directly; it remains available for any
// caller that genuinely needs a fresh, uncached read.
func (s *Service) CurrentBalance(_ context.Context) (uint64, error) {
	resp, err := s.Wallet.GetBalance()
	if err != nil {
		return 0, err
	}
	if resp == nil {
		return 0, errors.New("wallet returned no balance response")
	}
	return resp.GetAvailableBalance(), nil
}

// HealthCheck reports whether both Postgres (via Repository.Ping) and the
// wallet GRPC connection (via WalletClient.GetWalletConnectivity) are
// reachable. A non-nil error means /healthz should return 503.
//
// Handler.Healthz does NOT call this directly -- it pings Postgres live
// (via Service.Repo.Ping, same as here) but reads the wallet's
// connectivity from StatusCache instead of calling
// WalletClient.GetWalletConnectivity synchronously per request, for the
// same no-request-should-block-on-a-live-wallet-call reason
// CurrentBalance's doc comment describes. This method remains available
// as the live/synchronous primitive.
func (s *Service) HealthCheck(ctx context.Context) error {
	if err := s.Repo.Ping(ctx); err != nil {
		return err
	}
	resp, err := s.Wallet.GetWalletConnectivity()
	if err != nil {
		return err
	}
	if resp == nil || resp.GetStatus() != tari_generated.CheckConnectivityResponse_Online {
		return errors.New("wallet GRPC connectivity is not online")
	}
	return nil
}
