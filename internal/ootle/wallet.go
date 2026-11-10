package ootle

import (
	"context"
	"fmt"
)

// WalletClient is the narrow Ootle wallet-daemon surface Service
// depends on, mirroring the role internal/faucet.WalletClient plays
// for the L1 GRPC wallet -- an interface so tests can supply a fake
// instead of talking to a live Ootle wallet-daemon.
type WalletClient interface {
	// SubmitDispense runs the full per-dispense RPC flow (auth.method
	// -> auth.request -> transactions.submit_manifest ->
	// transactions.wait_result) and returns the finalized transaction
	// id. faucetComponent and dest are the manifest's "faucet"/"dest"
	// variable bindings (component addresses); amount and maxFee are
	// baked into the manifest text itself (see BuildManifest).
	SubmitDispense(ctx context.Context, faucetComponent, dest string, amount, maxFee uint64, waitTimeoutSecs int) (txID string, err error)
	// Ping reports whether the wallet-daemon is reachable at all, via
	// a bare auth.method call -- used only for StatusCache's
	// background reachability poll (see statuscache.go), never for an
	// actual dispense decision. It intentionally does not care whether
	// the reported method is "none" or "webauthn" -- either answer
	// means the daemon itself responded, which is all a reachability
	// check needs to know.
	Ping(ctx context.Context) error
}

// RPCWalletClient is the production WalletClient, backed by a *Client
// talking to a real Ootle wallet-daemon over JSON-RPC.
type RPCWalletClient struct {
	Client *Client
}

// SubmitDispense implements WalletClient.
func (w *RPCWalletClient) SubmitDispense(ctx context.Context, faucetComponent, dest string, amount, maxFee uint64, waitTimeoutSecs int) (string, error) {
	method, err := w.Client.AuthMethod(ctx)
	if err != nil {
		return "", fmt.Errorf("ootle: auth.method: %w", err)
	}
	if method == "webauthn" {
		return "", ErrWebAuthnUnsupported
	}
	if method != "none" {
		return "", fmt.Errorf("ootle: auth.method reported unrecognized method %q", method)
	}

	token, err := w.Client.AuthRequest(ctx)
	if err != nil {
		return "", fmt.Errorf("ootle: auth.request: %w", err)
	}

	manifest := BuildManifest(amount, maxFee)
	variables := map[string]string{
		"faucet": faucetComponent,
		"dest":   dest,
	}
	txID, err := w.Client.SubmitManifest(ctx, token, manifest, variables, maxFee)
	if err != nil {
		return "", fmt.Errorf("ootle: transactions.submit_manifest: %w", err)
	}

	if err := w.Client.WaitResult(ctx, token, txID, waitTimeoutSecs); err != nil {
		// submit_manifest already succeeded and txID is real/known at
		// this point, but the caller must still treat this as a
		// failure overall -- finalization was never confirmed -- so
		// the error is returned alongside, not swallowed. Callers
		// that want the (unconfirmed) txID for logging purposes can
		// still recover it from the wrapped state via the dispense
		// Service, which logs txID separately from the finalize
		// outcome.
		return txID, fmt.Errorf("ootle: transactions.wait_result: %w", err)
	}
	return txID, nil
}

// Ping implements WalletClient.
func (w *RPCWalletClient) Ping(ctx context.Context) error {
	_, err := w.Client.AuthMethod(ctx)
	return err
}
