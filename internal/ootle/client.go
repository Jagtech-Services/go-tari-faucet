// Package ootle adds a second, independent dispense path alongside the
// existing L1 (GRPC wallet Transfer) path in internal/faucet: dispensing
// test/claimed Tari on the "Ootle" L2 layer via a local Ootle
// wallet-daemon's JSON-RPC 2.0 API instead of the L1 wallet GRPC
// Transfer call. It follows the same narrow-interface,
// fake-friendly-for-tests convention internal/faucet uses (Repository,
// WalletClient, Clock) but is otherwise fully self-contained -- it does
// not import internal/faucet, and internal/faucet does not import it
// (see faucet.OotleStatusChecker for the one deliberately narrow,
// structural-typing exception that lets Handler.Healthz optionally
// report Ootle wallet reachability without either package depending on
// the other's concrete types).
package ootle

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// ErrWebAuthnUnsupported is returned by Client.Dispense (by way of
// RPCWalletClient.SubmitDispense) when the Ootle wallet-daemon's
// auth.method reports "webauthn" instead of "none" -- interactive
// WebAuthn auth has no meaningful implementation for a server-side,
// unattended faucet process, so that case is rejected outright rather
// than attempting (and hanging on) an interactive challenge.
var ErrWebAuthnUnsupported = errors.New("ootle: wallet-daemon requires webauthn authentication, which this faucet does not support")

// rpcRequest is the generic JSON-RPC 2.0 request envelope shared by all
// four wallet-daemon calls this package makes (auth.method,
// auth.request, transactions.submit_manifest,
// transactions.wait_result) -- only Method and Params vary per call.
type rpcRequest struct {
	Jsonrpc string      `json:"jsonrpc"`
	ID      int         `json:"id"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params"`
}

// rpcError is the JSON-RPC 2.0 error object, surfaced to Go callers as
// an *RPCError (see Error) rather than a bare string, so callers that
// care can inspect Method/Code/Message individually instead of just
// comparing error strings.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// rpcResponse is the generic JSON-RPC 2.0 response envelope. Result is
// left as json.RawMessage so Client.call's caller can unmarshal it into
// whatever shape that particular method returns, instead of this
// package needing one Go struct per RPC method's result shape up
// front.
type rpcResponse struct {
	Jsonrpc string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
}

// RPCError is the Go error type Client.call returns when the
// wallet-daemon's JSON-RPC response carries a non-nil "error" object --
// it names the method that failed alongside the daemon's own
// code/message, so logs/errors.Is callers can tell "auth.request
// failed" from "transactions.submit_manifest failed" without parsing
// strings.
type RPCError struct {
	Method  string
	Code    int
	Message string
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("ootle: wallet-daemon %s: %s (code %d)", e.Method, e.Message, e.Code)
}

// Client is a minimal JSON-RPC 2.0 client for the Ootle wallet-daemon,
// using only net/http + encoding/json (no new third-party dependency,
// per this change's constraints). It POSTs every call to
// <BaseURL>/json_rpc.
type Client struct {
	// BaseURL is the wallet-daemon's base URL, e.g.
	// "http://localhost:12008" -- never includes the trailing
	// "/json_rpc" path segment, which call appends itself.
	BaseURL string
	// HTTPClient is the underlying HTTP transport. Defaults to
	// http.DefaultClient when nil.
	HTTPClient *http.Client
}

func (c *Client) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return http.DefaultClient
}

// call POSTs a single JSON-RPC request for method/params to
// <BaseURL>/json_rpc, optionally setting an "Authorization: Bearer
// <bearerToken>" header when bearerToken is non-empty (per
// transactions.submit_manifest and transactions.wait_result's auth
// requirement -- auth.method and auth.request are called with no auth
// header at all), and unmarshals the response's "result" field into
// out (which may be nil when the caller doesn't need the result body,
// e.g. wait_result's success case). A non-nil JSON-RPC "error" object
// in the response is surfaced as *RPCError, naming method, rather than
// a generic decode/transport error.
func (c *Client) call(ctx context.Context, method string, params interface{}, bearerToken string, out interface{}) error {
	reqBody, err := json.Marshal(rpcRequest{
		Jsonrpc: "2.0",
		ID:      1,
		Method:  method,
		Params:  params,
	})
	if err != nil {
		return fmt.Errorf("ootle: marshaling %s request: %w", method, err)
	}

	url := c.BaseURL + "/json_rpc"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("ootle: building %s request: %w", method, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if bearerToken != "" {
		httpReq.Header.Set("Authorization", "Bearer "+bearerToken)
	}

	resp, err := c.httpClient().Do(httpReq)
	if err != nil {
		return fmt.Errorf("ootle: %s request failed: %w", method, err)
	}
	defer resp.Body.Close()

	var rpcResp rpcResponse
	if err := json.NewDecoder(resp.Body).Decode(&rpcResp); err != nil {
		return fmt.Errorf("ootle: decoding %s response: %w", method, err)
	}
	if rpcResp.Error != nil {
		return &RPCError{Method: method, Code: rpcResp.Error.Code, Message: rpcResp.Error.Message}
	}
	if out == nil {
		return nil
	}
	if len(rpcResp.Result) == 0 {
		return fmt.Errorf("ootle: %s response had no result and no error", method)
	}
	if err := json.Unmarshal(rpcResp.Result, out); err != nil {
		return fmt.Errorf("ootle: unmarshaling %s result: %w", method, err)
	}
	return nil
}

// AuthMethod calls auth.method with empty params and no Authorization
// header, returning the wallet-daemon's reported auth method -- "none"
// or "webauthn" per the wallet-daemon's own contract.
func (c *Client) AuthMethod(ctx context.Context) (string, error) {
	var result struct {
		Method string `json:"method"`
	}
	if err := c.call(ctx, "auth.method", struct{}{}, "", &result); err != nil {
		return "", err
	}
	return result.Method, nil
}

// AuthRequest calls auth.request with permissions ["Admin"] and
// credentials "None" (the no-interactive-auth path, only valid when
// AuthMethod previously reported "none"), returning a fresh JWT. Per
// the wallet-daemon's own contract these tokens expire in 5 minutes;
// callers are expected to request one per dispense call rather than
// caching/refreshing it, since a single dispense's whole
// submit_manifest+wait_result round trip is expected to complete well
// within that window.
func (c *Client) AuthRequest(ctx context.Context) (string, error) {
	params := struct {
		Permissions []string `json:"permissions"`
		Credentials string   `json:"credentials"`
	}{
		Permissions: []string{"Admin"},
		Credentials: "None",
	}
	var result struct {
		Token string `json:"token"`
	}
	if err := c.call(ctx, "auth.request", params, "", &result); err != nil {
		return "", err
	}
	return result.Token, nil
}

// SubmitManifest calls transactions.submit_manifest, authenticated via
// Authorization: Bearer <bearerToken>, submitting manifest (built by
// BuildManifest) with the given variables (the manifest's var! bindings
// -- "faucet" and "dest" component addresses) and maxFee, with dry_run
// always false (this is a real, submitted transaction, never a dry
// run). It returns the wallet-daemon's assigned transaction id (of the
// form "tx_<hex>").
func (c *Client) SubmitManifest(ctx context.Context, bearerToken, manifest string, variables map[string]string, maxFee uint64) (string, error) {
	params := struct {
		Manifest  string            `json:"manifest"`
		Variables map[string]string `json:"variables"`
		MaxFee    uint64            `json:"max_fee"`
		DryRun    bool              `json:"dry_run"`
	}{
		Manifest:  manifest,
		Variables: variables,
		MaxFee:    maxFee,
		DryRun:    false,
	}
	var result struct {
		TransactionID string          `json:"transaction_id"`
		Result        json.RawMessage `json:"result"`
	}
	if err := c.call(ctx, "transactions.submit_manifest", params, bearerToken, &result); err != nil {
		return "", err
	}
	return result.TransactionID, nil
}

// WaitResult calls transactions.wait_result, authenticated via
// Authorization: Bearer <bearerToken>, blocking (from the wallet-
// daemon's perspective; this call itself is also bounded by ctx) until
// transactionID finalizes or timeoutSecs elapses. A non-nil error here
// means the transaction did not confirm finalization within the
// configured window -- the caller must not report success to its own
// caller in that case, even though submit_manifest itself already
// succeeded.
func (c *Client) WaitResult(ctx context.Context, bearerToken, transactionID string, timeoutSecs int) error {
	params := struct {
		TransactionID string `json:"transaction_id"`
		TimeoutSecs   int    `json:"timeout_secs"`
	}{
		TransactionID: transactionID,
		TimeoutSecs:   timeoutSecs,
	}
	return c.call(ctx, "transactions.wait_result", params, bearerToken, nil)
}
