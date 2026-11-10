package ootle

import "time"

// Clock is the narrow time source Service depends on, so tests can
// freeze "now" instead of racing a real rate-limit window -- the same
// role internal/faucet.Clock plays, duplicated here rather than
// imported so this package stays fully self-contained (see package
// doc).
type Clock interface {
	Now() time.Time
}

// RealClock is the production Clock, backed by time.Now.
type RealClock struct{}

// Now returns the current wall-clock time.
func (RealClock) Now() time.Time { return time.Now() }
