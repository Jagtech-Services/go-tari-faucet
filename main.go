// Command tari-faucet runs the Tari testnet faucet HTTP service: it serves
// a plain HTML form for requesting test Tari, rate limits by address and
// client IP, and dispenses a fixed test amount via the wallet GRPC daemon
// on approval. See internal/faucet for the actual request-handling logic;
// this file is just flag parsing and dependency wiring.
//
// This is the production backend for testnet-faucet.supportxtm.com,
// fronted by a Caddy reverse proxy that terminates TLS and forwards to
// -listen-addr; it does not serve TLS itself.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	core "github.com/Jagtech-Services/core-go-lib/milieu"
	"github.com/Jagtech-Services/go-tari-faucet/internal/faucet"
	"github.com/Jagtech-Services/go-tari-lib/v3/walletGRPC"
	"github.com/sirupsen/logrus"
)

func getEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}

func main() {
	listenAddrPtr := flag.String("listen-addr", "0.0.0.0:8091", "HTTP listen address (fronted by Caddy as testnet-faucet.supportxtm.com in production; this flag is plain HTTP, not TLS)")
	walletGRPCAddressPtr := flag.String("wallet-grpc-address", "192.168.40.10:18143", "Tari wallet GRPC address")
	psqlServerPtr := flag.String("psql-server", getEnv("PSQL_SERVER", ""), "Postgres DSN (env PSQL_SERVER), required")
	sentryServerPtr := flag.String("sentry-server", getEnv("SENTRY_SERVER", ""), "Sentry DSN (env SENTRY_SERVER), optional")
	dispenseAmountPtr := flag.Uint64("dispense-amount", 10000000000, "Amount to dispense per request, in microMinotari")
	dispenseMaxAmountPtr := flag.Uint64("dispense-max-amount", 0, "Upper bound, in microMinotari, of a random dispense range; when 0 or <= -dispense-amount, the feature is off and every dispense sends exactly -dispense-amount (unchanged fixed-amount behavior)")
	rateLimitWindowPtr := flag.Duration("rate-limit-window", 24*time.Hour, "Minimum time between successful dispenses for the same address or IP")
	tickerPtr := flag.String("ticker", "tXTM", "Display ticker word used in the faucet's user-facing branding text (e.g. \"tXTM\" on testnet, \"XTM\" on mainnet); defaults to the testnet ticker, matching the default -wallet-grpc-address")
	networkLabelPtr := flag.String("network-label", "Testnet", "Display network label used alongside -ticker in the faucet's user-facing branding text (e.g. \"Testnet\" or \"Mainnet\"); defaults to \"Testnet\", matching the default -ticker")
	networkNicknamePtr := flag.String("network-nickname", "Esme", "Optional display network nickname used in parentheses after -network-label in the intro paragraph (e.g. \"Esme\" for Esmeralda testnet); empty string means no nickname display")
	statusPollIntervalPtr := flag.Duration("status-poll-interval", 30*time.Second, "How often to refresh the background-polled wallet balance/connectivity cache used by / and /healthz")
	turnstileEnabledPtr := flag.Bool("turnstile-enabled", false, "Enable Cloudflare Turnstile CAPTCHA verification on /request (off by default)")
	turnstileSiteKeyPtr := flag.String("turnstile-site-key", "", "Cloudflare Turnstile site key, rendered into the request form when -turnstile-enabled is true")
	turnstileSecretKeyPtr := flag.String("turnstile-secret-key", getEnv("TURNSTILE_SECRET_KEY", ""), "Cloudflare Turnstile secret key used to verify tokens server-side (env TURNSTILE_SECRET_KEY); never put a real secret on the command line")
	debugEnabledPtr := flag.Bool("debug-enabled", false, "Enable debug logging")
	flag.Parse()

	if *psqlServerPtr == "" {
		fmt.Fprintln(os.Stderr, "tari-faucet: -psql-server (or PSQL_SERVER env var) is required")
		os.Exit(1)
	}

	sentryURI := *sentryServerPtr
	milieu, err := core.NewMilieu(psqlServerPtr, nil, &sentryURI)
	if err != nil {
		milieu.CaptureException(err)
		milieu.Fatal(err.Error())
	}
	if *debugEnabledPtr {
		milieu.SetLogLevel(logrus.DebugLevel)
	}

	walletGRPC.InitWalletGRPC(*walletGRPCAddressPtr)

	logger := logrus.StandardLogger()
	if *debugEnabledPtr {
		logger.SetLevel(logrus.DebugLevel)
	}

	svc := &faucet.Service{
		Repo:   faucet.NewPGRepository(milieu.GetRawPGXPool()),
		Wallet: faucet.GRPCWalletClient{},
		Clock:  faucet.RealClock{},
		Config: faucet.Config{
			DispenseAmount:     *dispenseAmountPtr,
			MaxDispenseAmount:  *dispenseMaxAmountPtr,
			RateLimitWindow:    *rateLimitWindowPtr,
			Ticker:             *tickerPtr,
			NetworkLabel:       *networkLabelPtr,
			NetworkNickname:    *networkNicknamePtr,
			TurnstileEnabled:   *turnstileEnabledPtr,
			TurnstileSiteKey:   *turnstileSiteKeyPtr,
			TurnstileSecretKey: *turnstileSecretKeyPtr,
		},
		Logger: logger,
	}

	// statusCache holds the last-known wallet balance/connectivity,
	// refreshed on -status-poll-interval by a background goroutine, so
	// Handler.Index/Healthz never block a request on a live wallet GRPC
	// call (WalletClient.GetBalance/GetWalletConnectivity have no
	// timeout of their own and have been observed in production to
	// occasionally hang for 10+ seconds over the Tailscale link to the
	// wallet daemon). main.go has no existing signal-handling shutdown
	// pattern, so StartPolling just runs for the process lifetime via
	// context.Background() -- there's nothing to cancel it with today.
	statusCache := faucet.NewStatusCache()
	statusCache.Logger = logger
	go statusCache.StartPolling(context.Background(), svc.Wallet, *statusPollIntervalPtr)

	handler := faucet.NewHandler(svc, statusCache)
	mux := http.NewServeMux()
	handler.Routes(mux)

	logger.WithFields(logrus.Fields{
		"listen_addr":          *listenAddrPtr,
		"wallet_grpc_address":  *walletGRPCAddressPtr,
		"dispense_amount":      *dispenseAmountPtr,
		"dispense_max_amount":  *dispenseMaxAmountPtr,
		"rate_limit_window":    rateLimitWindowPtr.String(),
		"status_poll_interval": statusPollIntervalPtr.String(),
		"ticker":               *tickerPtr,
		"network_label":        *networkLabelPtr,
		"network_nickname":     *networkNicknamePtr,
		"turnstile_enabled":    *turnstileEnabledPtr,
		"turnstile_site_key":   *turnstileSiteKeyPtr,
	}).Info("tari-faucet: starting")

	if err := http.ListenAndServe(*listenAddrPtr, mux); err != nil {
		logger.Fatal(err)
	}
}
