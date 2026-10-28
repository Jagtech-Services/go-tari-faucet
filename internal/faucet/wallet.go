package faucet

import (
	"github.com/Jagtech-Services/go-tari-grpc-lib/v3/tari_generated"
	"github.com/Jagtech-Services/go-tari-lib/v3/walletGRPC"
)

// WalletClient is the narrow GRPC surface Service depends on. It mirrors
// go-tari-lib/walletGRPC's package-level SendTransactions/
// GetWalletConnectivity/GetBalances functions exactly, just behind an
// interface so tests can supply a fake wallet instead of dialing a live
// Tari wallet daemon (same role as go-crypto-pool/internal/backend/chain's
// ChainVerifier interface plays for chain RPC calls).
type WalletClient interface {
	SendTransactions(transactions []*tari_generated.PaymentRecipient, singleTx bool) (*tari_generated.TransferResponse, error)
	GetWalletConnectivity() (*tari_generated.CheckConnectivityResponse, error)
	// GetBalance mirrors walletGRPC.GetBalances() exactly (same as
	// go-crypto-pool's internal/backend/wallet/tari.go GetBalance
	// implementation for reference on the real response shape) --
	// AvailableBalance is the immediately spendable figure.
	GetBalance() (*tari_generated.GetBalanceResponse, error)
}

// GRPCWalletClient is the production WalletClient, backed by
// walletGRPC.InitWalletGRPC's package-level connection. Callers must call
// walletGRPC.InitWalletGRPC(addr) once before using it (same requirement
// go-tari-tools/cmd/payoutDaemon and cmd/walletBalanceExporter already
// have).
type GRPCWalletClient struct{}

// SendTransactions wraps walletGRPC.SendTransactions.
func (GRPCWalletClient) SendTransactions(transactions []*tari_generated.PaymentRecipient, singleTx bool) (*tari_generated.TransferResponse, error) {
	return walletGRPC.SendTransactions(transactions, singleTx)
}

// GetWalletConnectivity wraps walletGRPC.GetWalletConnectivity.
func (GRPCWalletClient) GetWalletConnectivity() (*tari_generated.CheckConnectivityResponse, error) {
	return walletGRPC.GetWalletConnectivity()
}

// GetBalance wraps walletGRPC.GetBalances.
func (GRPCWalletClient) GetBalance() (*tari_generated.GetBalanceResponse, error) {
	return walletGRPC.GetBalances()
}
