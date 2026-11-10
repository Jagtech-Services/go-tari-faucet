# go-tari-faucet
Faucet for the Tari network

An HTTP service that dispenses a small, fixed amount of test Tari to a
submitted testnet address, rate limited per-address and per-IP. It does not
perform payouts itself (see `go-tari-tools/cmd/payoutDaemon` for that); it
owns only its own `dispenses` audit/rate-limit table.

This is the production backend for `testnet-faucet.supportxtm.com`: it
serves plain HTTP on `-listen-addr` and expects a reverse proxy (Caddy) in
front of it to terminate TLS and forward the public domain to that
address.

It also serves a second, independent dispense path for Tari's L2 "Ootle"
layer (`internal/ootle`), which dispenses via a local Ootle wallet-daemon's
JSON-RPC 2.0 API instead of the L1 wallet GRPC `Transfer` call, and owns its
own separate `ootle_dispenses` audit/rate-limit table so the two paths'
rate limits never collide. See `GET /ootle` / `POST /ootle/dispense` below.
**As of this writing the Ootle faucet component has not been deployed to
mainnet yet** (pending burn-claim funding) -- `-ootle-faucet-component-address`
is unset by default, and `/ootle/dispense` fails closed with a clear error
on every request until an operator configures it; the rest of the service,
including the L1 `/request` path, is unaffected either way.

## Running

```
go build ./...
PSQL_SERVER='postgres://user:pass@host:5432/dbname?sslmode=disable' \
  ./go-tari-faucet -wallet-grpc-address=127.0.0.1:18143
```

Apply `migrations/tables.sql` against the target Postgres database/role
before first run.

### Flags

| Flag                   | Env             | Default                  | Description                                             |
|-------------------------|-----------------|---------------------------|-----------------------------------------------------------|
| `-listen-addr`          | -               | `0.0.0.0:8091`             | HTTP listen address (behind Caddy in production)            |
| `-wallet-grpc-address`  | -               | `100.88.139.119:12345`     | Tari wallet GRPC address                                    |
| `-psql-server`          | `PSQL_SERVER`   | *(required)*               | Postgres DSN for this faucet's own database                |
| `-sentry-server`        | `SENTRY_SERVER` | *(empty, disabled)*        | Sentry DSN, optional                                        |
| `-dispense-amount`      | -               | `1000000`                  | microMinotari dispensed per successful request              |
| `-rate-limit-window`    | -               | `24h`                      | Minimum time between successful dispenses per address/IP    |
| `-debug-enabled`        | -               | `false`                    | Enable debug logging                                        |
| `-ootle-wallet-url`     | `OOTLE_WALLET_URL` | `http://localhost:12008` | Ootle wallet-daemon JSON-RPC base URL (`<url>/json_rpc`)   |
| `-ootle-faucet-component-address` | `OOTLE_FAUCET_COMPONENT_ADDRESS` | *(empty)* | Deployed Ootle faucet component address; empty means `/ootle/dispense` fails closed (component not yet deployed to mainnet) |
| `-ootle-dispense-amount` | -              | `10000000000`               | Amount dispensed per successful Ootle (L2) request           |
| `-ootle-max-fee`        | -               | `2000000`                   | Max transaction fee the Ootle faucet component pays per dispense |
| `-ootle-wait-result-timeout-secs` | -     | `120`                       | Seconds to wait for an Ootle dispense's finalization        |
| `-ootle-rate-limit-window` | -            | `24h`                       | Minimum time between successful Ootle dispenses per address/IP (independent of `-rate-limit-window`) |
| `-ootle-status-poll-interval` | -         | `30s`                       | How often to refresh the background Ootle wallet-daemon reachability cache |

Routes: `GET /` (L1 request form), `POST /request` (submit an L1 address),
`GET /ootle` (L2/Ootle request form), `POST /ootle/dispense` (submit an
Ootle destination address, as form data or JSON), `GET /healthz` (200 if
Postgres + L1 wallet GRPC are both reachable, 503 otherwise; additionally
reports Ootle wallet-daemon reachability as an informational
`ootle_wallet: up|down|unknown` line that never affects the 200/503
decision).

See `deploy/tari-faucet.service` for a reference (documentation-only)
systemd unit.
