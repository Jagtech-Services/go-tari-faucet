package ootle

import "fmt"

// BuildManifest renders the two-function Tari Ootle transaction
// manifest this faucet always submits: fee_main pays maxFee from the
// "faucet" component (bound via variables at submit time -- see
// SubmitManifest), and main takes exactly amount free coins from
// "faucet" and deposits them into "dest". Both component addresses are
// NOT embedded in the manifest string itself -- they're bound via
// var!["faucet"]/var!["dest"] and the submit_manifest call's separate
// "variables" map (see RPCWalletClient.SubmitDispense) -- only amount
// and maxFee are baked into the manifest text itself, exactly per the
// field names and manifest shape specified by tari-ootle's own
// SKILL.md (do not rename fee_main/main/faucet/dest, and do not
// restructure this into embedding the addresses directly -- that would
// silently diverge from the contract the wallet-daemon expects).
func BuildManifest(amount, maxFee uint64) string {
	return fmt.Sprintf(`fn fee_main() {
  let faucet = var!["faucet"];
  faucet.pay_fee(%d);
}

fn main() {
  let faucet = var!["faucet"];
  let dest = var!["dest"];
  let bucket = faucet.take_free_coins(amount!(%d));
  dest.deposit(bucket);
}`, maxFee, amount)
}
