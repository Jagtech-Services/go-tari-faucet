package faucet

import (
	"errors"
	"strings"

	"github.com/Jagtech-Services/go-tari-lib/v3/address"
)

// ErrPaymentIDNotAllowed is a faucet-specific policy rejection, not an
// address.Parse error -- the address is perfectly well-formed, but this
// faucet only ever dispenses to a plain destination address and has no
// legitimate use for payment IDs, so any address carrying the
// PAYMENT_ID feature bit is refused here rather than in go-tari-lib.
var ErrPaymentIDNotAllowed = errors.New("payment-id-bearing addresses are not accepted by this faucet")

// ValidateAddress checks that raw is a well-formed Tari address (base58,
// hex, or emoji encoding -- anything address.Parse accepts) using go-tari-
// lib's DammSum/Ristretto255-based validation, and returns the parsed
// address plus its canonical base58 form. Malformed input returns
// address.Address{} and the underlying parse error unchanged, so callers
// can render it directly rather than needing their own validation logic.
func ValidateAddress(raw string) (address.Address, string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return address.Address{}, "", address.ErrInvalidAddressString
	}
	addr, err := address.Parse(trimmed)
	if err != nil {
		return address.Address{}, "", err
	}
	if addr.Features().Contains(address.FeaturePaymentID) {
		return address.Address{}, "", ErrPaymentIDNotAllowed
	}
	return addr, addr.Base58(), nil
}
