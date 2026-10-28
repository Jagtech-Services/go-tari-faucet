package faucet

import (
	"crypto/rand"
	"testing"

	"github.com/Jagtech-Services/go-tari-lib/v3/address"
	"github.com/gtank/ristretto255"
)

// validTestnetAddress generates a real, valid Esmeralda single-address
// (a genuine Ristretto255 point, canonically encoded via
// address.PublicKeyFromCanonicalBytes -- same technique go-tari-lib's own
// address_test.go uses for randomPublicKey), then returns its base58
// encoding for use as a "well-formed address" fixture across tests. This
// deliberately doesn't hand-roll validation: it exercises the exact same
// address.NewSingleInteractiveOnly/Base58 path a real client would.
func validTestnetAddress(t *testing.T) string {
	t.Helper()
	var seed [64]byte
	if _, err := rand.Read(seed[:]); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	scalar, err := new(ristretto255.Scalar).SetUniformBytes(seed[:])
	if err != nil {
		t.Fatalf("SetUniformBytes: %v", err)
	}
	elem := new(ristretto255.Element).ScalarBaseMult(scalar)
	key, err := address.PublicKeyFromCanonicalBytes(elem.Bytes())
	if err != nil {
		t.Fatalf("PublicKeyFromCanonicalBytes: %v", err)
	}
	addr := address.NewSingleInteractiveOnly(key, address.Esmeralda)
	return addr.Base58()
}

// randomTestnetPublicKey generates a real Ristretto255 public key via
// crypto/rand + ristretto255.Scalar, the same technique
// validTestnetAddress uses -- factored out so validDualTestnetAddress
// can call it twice (once for the view key, once for the spend key).
func randomTestnetPublicKey(t *testing.T) address.CompressedPublicKey {
	t.Helper()
	var seed [64]byte
	if _, err := rand.Read(seed[:]); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	scalar, err := new(ristretto255.Scalar).SetUniformBytes(seed[:])
	if err != nil {
		t.Fatalf("SetUniformBytes: %v", err)
	}
	elem := new(ristretto255.Element).ScalarBaseMult(scalar)
	key, err := address.PublicKeyFromCanonicalBytes(elem.Bytes())
	if err != nil {
		t.Fatalf("PublicKeyFromCanonicalBytes: %v", err)
	}
	return key
}

// validDualTestnetAddress generates a real, valid Esmeralda dual
// address (a genuine view key + spend key pair) and returns its base58
// encoding. When withPaymentID is true, a non-empty memo-field payment
// id is attached via address.NewDual, which automatically OR's
// address.FeaturePaymentID into the address's features whenever the
// memoFieldPaymentID argument is non-nil -- so this exercises the
// exact same feature-bit-setting path a real client would.
func validDualTestnetAddress(t *testing.T, withPaymentID bool) string {
	t.Helper()
	viewKey := randomTestnetPublicKey(t)
	spendKey := randomTestnetPublicKey(t)
	var memo []byte
	if withPaymentID {
		memo = []byte("test-payment-id")
	}
	addr, err := address.NewDual(viewKey, spendKey, address.Esmeralda, address.DefaultFeatures(), memo)
	if err != nil {
		t.Fatalf("address.NewDual: %v", err)
	}
	return addr.Base58()
}
