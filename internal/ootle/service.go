package ootle

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

// Outcome classifies the result of a Dispense call, the Ootle-path
// analog of internal/faucet.Outcome.
type Outcome int

const (
	// OutcomeSuccess means the wallet-daemon finalized the manifest
	// transaction.
	OutcomeSuccess Outcome = iota
	// OutcomeInvalidAddress means the submitted destination address
	// failed validation.
	OutcomeInvalidAddress
	// OutcomeRateLimited means the address or IP has dispensed
	// successfully within the configured rate-limit window.
	OutcomeRateLimited
	// OutcomeNotConfigured means Config.FaucetComponentAddress is
	// unset -- the Ootle faucet component has not been deployed to
	// mainnet yet (pending burn-claim funding). This is a distinct,
	// expected-for-now outcome, not a generic error: Service.Dispense
	// fails closed on every actual dispense attempt while this is
	// unset, without panicking and without affecting the rest of the
	// service (including the L1 path) in any way.
	OutcomeNotConfigured
	// OutcomeError means something else went wrong (DB or wallet-
	// daemon JSON-RPC failure, including an unsupported webauthn auth
	// method) -- the request should still get a clean error, not a
	// 500.
	OutcomeError
)

// ErrFaucetComponentNotConfigured is Result.Err's value whenever
// Outcome == OutcomeNotConfigured.
var ErrFaucetComponentNotConfigured = errors.New("ootle: faucet component address is not configured; refusing to dispense")

// ErrInvalidDestination is Result.Err's value whenever Outcome ==
// OutcomeInvalidAddress.
var ErrInvalidDestination = errors.New("ootle: destination address is empty or malformed")

// Result is the outcome of a single Dispense call, the Ootle-path
// analog of internal/faucet.Result.
type Result struct {
	Outcome    Outcome
	RetryAfter time.Time // valid only when Outcome == OutcomeRateLimited
	Err        error     // non-nil for every Outcome except OutcomeSuccess
	TxID       string    // valid only when Outcome == OutcomeSuccess
	Amount     uint64    // the amount attempted/sent, set on OutcomeSuccess and OutcomeError
}

// Config holds Service's tunables, mirroring internal/faucet.Config's
// role, all wired from main.go's flags.
type Config struct {
	// WalletBaseURL is the Ootle wallet-daemon's base URL, e.g.
	// "http://localhost:12008" -- see Client.BaseURL.
	WalletBaseURL string

	// FaucetComponentAddress is the deployed Ootle faucet component's
	// address, bound as the manifest's "faucet" variable. THIS HAS NO
	// REAL VALUE YET in production -- the component is not deployed to
	// mainnet (pending burn-claim funding) -- so it defaults to empty
	// string, and Service.Dispense fails closed with
	// OutcomeNotConfigured on every actual dispense attempt while it's
	// unset, rather than panicking or preventing the rest of the
	// service (startup, the L1 path, healthz, etc.) from working.
	FaucetComponentAddress string

	// DispenseAmount is the fixed amount sent per successful Ootle
	// dispense, analogous to faucet.Config.DispenseAmount. Unlike the
	// L1 path, there is currently no random-range feature here.
	DispenseAmount uint64

	// MaxFee is the maximum transaction fee (in the same atomic unit
	// as DispenseAmount) the faucet component pays via the manifest's
	// fee_main function.
	MaxFee uint64

	// WaitResultTimeoutSecs bounds how long transactions.wait_result
	// waits for the submitted manifest transaction to finalize before
	// giving up (see Client.WaitResult). A reasonable default (120)
	// is set by main.go's flag default, not hardcoded here.
	WaitResultTimeoutSecs int

	// RateLimitWindow is the minimum time between successful Ootle
	// dispenses for the same destination address or requester IP,
	// analogous to faucet.Config.RateLimitWindow but tracked
	// completely independently (see Repository/ootle_dispenses).
	RateLimitWindow time.Duration
}

// Service is the Ootle dispense path's core business logic: address
// validation, rate limiting, and wallet-daemon dispensing. It depends
// only on the narrow Repository/WalletClient/Clock interfaces, so it
// can be fully unit tested without a live Postgres or Ootle
// wallet-daemon -- the exact same shape internal/faucet.Service follows
// for the L1 path, deliberately NOT sharing a struct with it (see
// package doc).
type Service struct {
	Repo   Repository
	Wallet WalletClient
	Clock  Clock
	Config Config
	Logger *logrus.Logger
}

// validateDestination trims raw and rejects it if empty. Ootle account/
// component addresses have a different format than L1 Tari addresses
// (internal/faucet.ValidateAddress's base58/hex/emoji Ristretto255
// encoding does not apply here), and tari-ootle's own tooling does not
// publish a narrower client-side format this faucet should validate
// against -- so this is deliberately the same "non-empty, trimmed"
// check an upstream wallet-daemon call itself would reject anyway, with
// no attempt to second-guess the daemon's own format validation.
func validateDestination(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", ErrInvalidDestination
	}
	return trimmed, nil
}

func (s *Service) logger() *logrus.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return logrus.StandardLogger()
}

func (s *Service) now() time.Time {
	if s.Clock == nil {
		return time.Now()
	}
	return s.Clock.Now()
}

// Dispense validates dest, fails closed if Config.FaucetComponentAddress
// is unset, checks the rate limit for both dest and ip, and -- if
// allowed -- sends Config.DispenseAmount to dest via the Ootle
// wallet-daemon, recording the attempt in Repository regardless of
// outcome. Mirrors faucet.Service.Dispense's structure and logging
// idioms exactly, adapted for this path's extra fail-closed
// precondition.
func (s *Service) Dispense(ctx context.Context, rawDestination, ip string) Result {
	logger := s.logger()

	// Checked before address validation and before ever touching the
	// rate-limit table: an unconfigured faucet component is a
	// guaranteed failure for every request regardless of
	// address/rate-limit state, so there is no reason to consume a
	// rate-limit slot (or a DB round trip) on a request that can never
	// possibly succeed.
	if s.Config.FaucetComponentAddress == "" {
		logger.Warn("ootle: dispense attempted but faucet component address is not configured (not yet deployed to mainnet); failing closed")
		return Result{Outcome: OutcomeNotConfigured, Err: ErrFaucetComponentNotConfigured}
	}

	dest, err := validateDestination(rawDestination)
	if err != nil {
		logger.WithFields(logrus.Fields{
			"destination": rawDestination,
			"ip":          ip,
		}).Warn("ootle: rejected malformed destination address")
		return Result{Outcome: OutcomeInvalidAddress, Err: err}
	}

	now := s.now()
	since := now.Add(-s.Config.RateLimitWindow)
	amount := s.Config.DispenseAmount

	reservation, err := s.Repo.ReserveDispense(ctx, dest, ip, amount, since, now)
	if err != nil {
		logger.WithFields(logrus.Fields{
			"destination": dest,
			"ip":          ip,
			"error":       err,
		}).Warn("ootle: rate-limit reservation failed")
		return Result{Outcome: OutcomeError, Err: err, Amount: amount}
	}
	if reservation.RateLimited {
		retryAfter := reservation.LastSuccessAt.Add(s.Config.RateLimitWindow)
		logger.WithFields(logrus.Fields{
			"destination": dest,
			"ip":          ip,
			"retry_after": retryAfter,
		}).Info("ootle: rate limited")
		return Result{Outcome: OutcomeRateLimited, RetryAfter: retryAfter}
	}

	txID, sendErr := s.Wallet.SubmitDispense(ctx, s.Config.FaucetComponentAddress, dest, amount, s.Config.MaxFee, s.waitTimeoutSecs())

	success := sendErr == nil
	errMsg := ""
	var result Result
	if sendErr != nil {
		errMsg = sendErr.Error()
		result = Result{Outcome: OutcomeError, Err: sendErr, Amount: amount, TxID: txID}
	} else {
		result = Result{Outcome: OutcomeSuccess, Amount: amount, TxID: txID}
	}

	if finalizeErr := s.Repo.FinalizeDispense(ctx, reservation.ID, success, errMsg, txID); finalizeErr != nil {
		logger.WithFields(logrus.Fields{
			"destination": dest,
			"ip":          ip,
			"error":       finalizeErr,
		}).Error("ootle: failed to finalize dispense audit row")
	}

	logFields := logrus.Fields{
		"destination": dest,
		"ip":          ip,
		"amount":      amount,
		"success":     success,
		"tx_id":       txID,
	}
	if errMsg != "" {
		logFields["error"] = errMsg
	}
	if success {
		logger.WithFields(logFields).Info("ootle: dispense attempt")
	} else {
		logger.WithFields(logFields).Warn("ootle: dispense attempt")
	}

	return result
}

// waitTimeoutSecs returns Config.WaitResultTimeoutSecs, falling back to
// 120 (matching main.go's flag default) if it's unset/zero -- Service
// itself must never send a 0-second timeout to the wallet-daemon.
func (s *Service) waitTimeoutSecs() int {
	if s.Config.WaitResultTimeoutSecs <= 0 {
		return 120
	}
	return s.Config.WaitResultTimeoutSecs
}
