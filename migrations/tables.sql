-- dispenses tracks every faucet dispense attempt (successful or not), and is
-- the sole bookkeeping table this service owns. It is deliberately separate
-- from go-tari-tools/cmd/payoutDaemon's `balances`/`payment_batch`/
-- `transactions` schema -- this faucet does not do payouts, it just needs an
-- audit trail plus a fast lookup for the address+IP rate limit.
create table dispenses
(
    id         bigserial
        constraint dispenses_pk
            primary key,
    address    text                                   not null,
    ip         text                                   not null,
    amount     bigint                   default 0     not null,
    success    boolean                  default false not null,
    error      text,
    tx_id      numeric,
    created_at timestamp with time zone default now() not null
);

-- Rate limiting checks "has this address dispensed successfully in the last
-- <window>" and "has this IP dispensed successfully in the last <window>"
-- independently, so each needs its own index with created_at trailing for an
-- efficient "most recent row" lookup instead of a table scan.
create index dispenses_address_created_at_idx
    on dispenses (address, created_at desc);

create index dispenses_ip_created_at_idx
    on dispenses (ip, created_at desc);

-- ootle_dispenses mirrors dispenses exactly (same audit/rate-limit
-- discipline: one row per attempt, rolling-window lookup by address or
-- ip) but is a SEPARATE table for the Tari L2 "Ootle" dispense path
-- (internal/ootle), so L1 and L2 rate limits never collide -- a wallet
-- that's exhausted its L1 24h window can still request an L2 dispense
-- (and vice versa), and an incident/rollback on one path never touches
-- the other's bookkeeping. tx_id is text here (not numeric, as in
-- dispenses) because Ootle transaction ids are hex strings of the form
-- "tx_<hex>", not the L1 wallet's numeric transaction id.
create table ootle_dispenses
(
    id         bigserial
        constraint ootle_dispenses_pk
            primary key,
    address    text                                   not null,
    ip         text                                   not null,
    amount     bigint                   default 0     not null,
    success    boolean                  default false not null,
    error      text,
    tx_id      text,
    created_at timestamp with time zone default now() not null
);

create index ootle_dispenses_address_created_at_idx
    on ootle_dispenses (address, created_at desc);

create index ootle_dispenses_ip_created_at_idx
    on ootle_dispenses (ip, created_at desc);
