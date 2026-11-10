package ootle

import (
	"fmt"
	"html/template"
	"strconv"
	"strings"
)

// honeypotFieldName is the name of indexTemplate's hidden anti-spam
// field -- same name and same visually-hidden-via-position technique
// internal/faucet.honeypotFieldName/indexTemplate use, duplicated here
// (rather than imported) to keep this package self-contained. See
// faucet.honeypotFieldName's doc comment for why position:absolute
// (not display:none) is used.
const honeypotFieldName = "website"

// microMinotariPerXTM mirrors faucet.microMinotariPerXTM's role: 1 XTM
// == 1,000,000 atomic units. Ootle amounts are denominated in the same
// atomic unit Tari uses everywhere else, so the same conversion applies
// for this page's human-readable display.
const microMinotariPerXTM = 1_000_000

// formatWithCommas renders n with thousands separators, the same
// implementation as faucet.formatWithCommas, duplicated here to keep
// this package self-contained.
func formatWithCommas(n uint64) string {
	s := strconv.FormatUint(n, 10)
	if len(s) <= 3 {
		return s
	}
	var groups []string
	for len(s) > 3 {
		groups = append([]string{s[len(s)-3:]}, groups...)
		s = s[:len(s)-3]
	}
	groups = append([]string{s}, groups...)
	return strings.Join(groups, ",")
}

// formatXTM renders a raw atomic-unit amount as a human-readable "XTM"
// display string, the same implementation as faucet.formatXTM,
// duplicated here to keep this package self-contained.
func formatXTM(n uint64) string {
	whole := n / microMinotariPerXTM
	frac := n % microMinotariPerXTM
	if frac == 0 {
		return formatWithCommas(whole) + " XTM"
	}
	fracStr := strings.TrimRight(fmt.Sprintf("%06d", frac), "0")
	return formatWithCommas(whole) + "." + fracStr + " XTM"
}

// indexTemplate renders the Ootle (L2) faucet's single page: a plain
// server-rendered HTML form (no JS framework, no build step), matching
// internal/faucet's indexTemplate branding/layout exactly (same CSS
// tokens/classes) so the two pages look like the same product. The
// only structural difference is the form field itself (a destination
// component address instead of an L1 wallet address) and the intro
// copy explaining this is the L2 path.
var indexTemplate = template.Must(template.New("ootle-index").Funcs(template.FuncMap{
	"commas": formatWithCommas,
	"xtm":    formatXTM,
}).Parse(`<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Tari Ootle (L2) Faucet</title>
  <link rel="preconnect" href="https://fonts.googleapis.com" />
  <link rel="preconnect" href="https://fonts.gstatic.com" crossorigin />
  <link href="https://fonts.googleapis.com/css2?family=Cormorant+Garamond:wght@600&family=Inter:wght@300&family=JetBrains+Mono:wght@400&display=swap" rel="stylesheet" />
  <style>
    :root {
      --jag-alexandrite-daylight: #1a8a6a;
      --jag-alexandrite-incandescent: #9b2d5a;
      --jag-alexandrite-olive: #6a8a50;
      --jag-deep-teal: #1a5a46;
      --jag-deep-raspberry: #6a1a3a;
      --jag-alexandrite-rose: #c94070;
      --jag-bg-dark: #07080a;
      --jag-text-on-dark: #f0ece4;
      --accent-a: var(--jag-alexandrite-daylight);
      --accent-b: var(--jag-alexandrite-incandescent);
      --accent-grad: linear-gradient(135deg, #1a8a6a 0%, #6a8a50 50%, #9b2d5a 100%);
      --border-glow: rgba(26, 138, 106, 0.3);
    }
    * { box-sizing: border-box; }
    body {
      background: var(--jag-bg-dark);
      color: var(--jag-text-on-dark);
      font-family: 'Inter', sans-serif;
      font-weight: 300;
      max-width: 640px;
      margin: 0 auto;
      padding: 4rem 1.5rem;
    }
    h1, h2, h3 {
      font-family: 'Cormorant Garamond', serif;
      font-weight: 600;
      text-transform: uppercase;
      letter-spacing: 0.16em;
    }
    h1 { font-size: 1.65rem; margin: 0 0 1.5rem; }
    p { line-height: 1.7; }
    .balance {
      border: 1px solid var(--border-glow);
      border-radius: 4px;
      padding: 1rem 1.25rem;
      margin: 1.75rem 0;
    }
    .balance .label {
      display: block;
      color: var(--jag-alexandrite-daylight);
      text-transform: uppercase;
      letter-spacing: 0.08em;
      font-size: 0.75rem;
      font-family: 'JetBrains Mono', monospace;
      margin-bottom: 0.4rem;
    }
    .balance .value {
      font-family: 'JetBrains Mono', monospace;
      font-size: 1.05rem;
    }
    label {
      display: block;
      font-family: 'JetBrains Mono', monospace;
      font-size: 0.85rem;
      margin-bottom: 0.5rem;
    }
    input[type=text] {
      width: 100%;
      padding: 0.65rem 0.75rem;
      font-family: 'JetBrains Mono', monospace;
      background: transparent;
      border: 1px solid var(--border-glow);
      color: var(--jag-text-on-dark);
      border-radius: 4px;
    }
    input[type=text]:focus { outline: none; border-color: var(--jag-alexandrite-daylight); }
    button {
      padding: 0.65rem 1.75rem;
      margin-top: 1rem;
      background: var(--accent-grad);
      border: none;
      color: var(--jag-bg-dark);
      font-family: 'JetBrains Mono', monospace;
      font-weight: 400;
      letter-spacing: 0.05em;
      border-radius: 4px;
      cursor: pointer;
    }
    .message {
      padding: 0.85rem 1rem;
      margin-bottom: 1.5rem;
      border-radius: 4px;
      font-family: 'JetBrains Mono', monospace;
      font-size: 0.9rem;
    }
    .message.error {
      background: rgba(106, 26, 58, 0.18);
      color: var(--jag-alexandrite-rose);
      border: 1px solid var(--jag-deep-raspberry);
    }
    .message.success {
      background: rgba(26, 138, 106, 0.12);
      color: var(--jag-alexandrite-daylight);
      border: 1px solid var(--border-glow);
    }
    .hp {
      position: absolute;
      left: -9999px;
      top: auto;
      width: 1px;
      height: 1px;
      overflow: hidden;
    }
    .l1-note {
      margin-top: 2rem;
      padding-top: 1rem;
      border-top: 1px solid var(--border-glow);
      font-size: 0.85rem;
      color: rgba(240, 236, 228, 0.65);
    }
    .l1-note a {
      color: var(--jag-alexandrite-daylight);
    }
  </style>
</head>
<body>
  <h1>Tari Ootle (L2) Faucet</h1>
  <p>Enter an Ootle (L2) account or component destination address below to receive {{xtm .DispenseAmount}} on the Ootle layer.</p>

  <div class="balance">
    <span class="label">Dispense amount</span>
    <span class="value">{{xtm .DispenseAmount}} per request</span>
  </div>

  {{if .Message}}
  <div class="message {{if .IsError}}error{{else}}success{{end}}">{{.Message}}</div>
  {{end}}
  <form method="post" action="/ootle/dispense">
    <label for="destination">Ootle destination address</label>
    <input type="text" id="destination" name="destination" placeholder="component_..." value="{{.Destination}}" required>
    <div class="hp" aria-hidden="true">
      <label for="website">Leave this field blank</label>
      <input type="text" id="website" name="website" tabindex="-1" autocomplete="off">
    </div>
    <br>
    <button type="submit">Request XTM (L2)</button>
  </form>

  <p class="l1-note">Need to fund the Ootle faucet first? See the <a href="https://ootle.tari.com/guides/burn-minotari/" target="_blank" rel="noopener noreferrer">burn guide</a> for burning L1 Minotari into a claimable L2 balance.</p>
</body>
</html>
`))

// indexData is the template context for indexTemplate.
type indexData struct {
	Destination string
	Message     string
	IsError     bool

	// DispenseAmount mirrors Service.Config.DispenseAmount, sourced by
	// renderIndex on every call.
	DispenseAmount uint64

	// StatusCode is the HTTP status to send with the rendered page.
	// Zero means "use the default" (200, or 400 if IsError).
	StatusCode int
}
