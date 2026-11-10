package ootle

import (
	"strings"
	"testing"
)

// TestBuildManifest proves the manifest template substitution is exact
// -- the literal fn fee_main/main structure, var!["faucet"]/var!["dest"]
// bindings, and the %d substitutions for max_fee/amount -- per
// tari-ootle's own SKILL.md contract (see BuildManifest's doc comment).
// No live network/wallet-daemon involved at all.
func TestBuildManifest(t *testing.T) {
	tests := []struct {
		name   string
		amount uint64
		maxFee uint64
		want   string
	}{
		{
			name:   "typical amount and fee",
			amount: 10000000000,
			maxFee: 2000000,
			want: `fn fee_main() {
  let faucet = var!["faucet"];
  faucet.pay_fee(2000000);
}

fn main() {
  let faucet = var!["faucet"];
  let dest = var!["dest"];
  let bucket = faucet.take_free_coins(amount!(10000000000));
  dest.deposit(bucket);
}`,
		},
		{
			name:   "zero amount and fee still substitute correctly",
			amount: 0,
			maxFee: 0,
			want: `fn fee_main() {
  let faucet = var!["faucet"];
  faucet.pay_fee(0);
}

fn main() {
  let faucet = var!["faucet"];
  let dest = var!["dest"];
  let bucket = faucet.take_free_coins(amount!(0));
  dest.deposit(bucket);
}`,
		},
		{
			name:   "amount and maxFee land in the correct, non-swapped positions",
			amount: 111,
			maxFee: 222,
			want: `fn fee_main() {
  let faucet = var!["faucet"];
  faucet.pay_fee(222);
}

fn main() {
  let faucet = var!["faucet"];
  let dest = var!["dest"];
  let bucket = faucet.take_free_coins(amount!(111));
  dest.deposit(bucket);
}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BuildManifest(tt.amount, tt.maxFee)
			if got != tt.want {
				t.Errorf("BuildManifest(%d, %d) =\n%s\nwant:\n%s", tt.amount, tt.maxFee, got, tt.want)
			}
		})
	}
}

// TestBuildManifest_NeverEmbedsComponentAddresses proves component
// addresses are never baked into the manifest text itself -- they're
// bound separately via submit_manifest's "variables" map (see
// RPCWalletClient.SubmitDispense) -- only amount/maxFee are. This
// guards against a future edit accidentally reintroducing address
// interpolation into the manifest string, which would silently diverge
// from the wallet-daemon's expected contract.
func TestBuildManifest_NeverEmbedsComponentAddresses(t *testing.T) {
	got := BuildManifest(123, 456)
	for _, forbidden := range []string{"component_", "faucet_address", "dest_address"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("BuildManifest output unexpectedly contains %q -- component addresses must only be bound via submit_manifest's variables map, never embedded in the manifest text:\n%s", forbidden, got)
		}
	}
}
