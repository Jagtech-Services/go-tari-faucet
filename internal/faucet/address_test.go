package faucet

import (
	"errors"
	"testing"

	"github.com/Jagtech-Services/go-tari-lib/v3/address"
)

func TestValidateAddress_AcceptsWellFormedAddress(t *testing.T) {
	valid := validTestnetAddress(t)
	addr, base58, err := ValidateAddress(valid)
	if err != nil {
		t.Fatalf("ValidateAddress(%q) returned unexpected error: %v", valid, err)
	}
	if base58 == "" {
		t.Fatal("ValidateAddress returned an empty base58 string for a valid address")
	}
	if addr.Kind() != address.KindSingle {
		t.Fatalf("ValidateAddress returned kind %v, want KindSingle", addr.Kind())
	}
}

func TestValidateAddress_TrimsWhitespace(t *testing.T) {
	valid := validTestnetAddress(t)
	_, base58, err := ValidateAddress("  " + valid + "\n")
	if err != nil {
		t.Fatalf("ValidateAddress with surrounding whitespace returned unexpected error: %v", err)
	}
	if base58 != valid {
		t.Fatalf("ValidateAddress trimmed base58 = %q, want %q", base58, valid)
	}
}

func TestValidateAddress_RejectsMalformedAddress(t *testing.T) {
	cases := []string{
		"",
		"   ",
		"not-a-tari-address-at-all",
		"12345",
		"!!!invalid-base58-chars-@#$%",
	}
	for _, tc := range cases {
		t.Run(tc, func(t *testing.T) {
			_, _, err := ValidateAddress(tc)
			if err == nil {
				t.Fatalf("ValidateAddress(%q) = nil error, want a validation error", tc)
			}
		})
	}
}

func TestValidateAddress_RejectsTruncatedValidAddress(t *testing.T) {
	valid := validTestnetAddress(t)
	truncated := valid[:len(valid)-5]
	_, _, err := ValidateAddress(truncated)
	if err == nil {
		t.Fatalf("ValidateAddress(%q) = nil error, want a validation error for a truncated address", truncated)
	}
	if !errors.Is(err, address.ErrInvalidAddressString) && !errors.Is(err, address.ErrInvalidSize) {
		// Any address package error is acceptable here, just make sure we
		// got one that describeAddressError can render.
		t.Logf("truncated address rejected with: %v", err)
	}
}

func TestValidateAddress_RejectsPaymentIDAddress(t *testing.T) {
	withPaymentID := validDualTestnetAddress(t, true)
	_, base58, err := ValidateAddress(withPaymentID)
	if !errors.Is(err, ErrPaymentIDNotAllowed) {
		t.Fatalf("ValidateAddress(%q) error = %v, want ErrPaymentIDNotAllowed", withPaymentID, err)
	}
	if base58 != "" {
		t.Fatalf("ValidateAddress returned base58 = %q on the payment-id rejection path, want empty string", base58)
	}
}

func TestValidateAddress_AcceptsDualAddressWithoutPaymentID(t *testing.T) {
	plain := validDualTestnetAddress(t, false)
	addr, base58, err := ValidateAddress(plain)
	if err != nil {
		t.Fatalf("ValidateAddress(%q) returned unexpected error: %v", plain, err)
	}
	if base58 == "" {
		t.Fatal("ValidateAddress returned an empty base58 string for a valid dual address")
	}
	if addr.Kind() != address.KindDual {
		t.Fatalf("ValidateAddress returned kind %v, want KindDual", addr.Kind())
	}
	if addr.Features().Contains(address.FeaturePaymentID) {
		t.Fatal("ValidateAddress accepted an address whose features unexpectedly include FeaturePaymentID")
	}
}
