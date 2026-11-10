package ootle

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/sirupsen/logrus"
)

// Handler wires Service into net/http handlers for the Ootle dispense
// path's two routes: GET /ootle (the form) and POST /ootle/dispense
// (submit a destination address) -- the L2 analog of
// internal/faucet.Handler, deliberately a separate type rather than
// extending faucet.Handler (see package doc).
type Handler struct {
	Service *Service

	// StatusCache holds the last-known wallet-daemon reachability,
	// refreshed in the background (see StatusCache.StartPolling).
	// Optional for Routes/Index to still render without it, but
	// main.go always supplies one in production.
	StatusCache *StatusCache
}

// NewHandler builds a Handler around svc, reading wallet-daemon
// reachability from cache for display purposes.
func NewHandler(svc *Service, cache *StatusCache) *Handler {
	return &Handler{Service: svc, StatusCache: cache}
}

// Routes registers this Handler's routes on mux, alongside (not
// instead of) internal/faucet.Handler's existing routes -- main.go
// calls both Handlers' Routes methods on the same mux.
func (h *Handler) Routes(mux *http.ServeMux) {
	mux.HandleFunc("/ootle", h.Index)
	mux.HandleFunc("/ootle/dispense", h.Dispense)
}

// genericRejectionMessage is the message rendered for any request
// rejection that shouldn't reveal its real cause to the caller --
// currently just the honeypot trip, mirroring
// faucet.genericRejectionMessage's role.
const genericRejectionMessage = "Something went wrong dispensing XTM (L2). Please try again shortly."

// Index renders the plain HTML request form for the Ootle dispense
// path.
func (h *Handler) Index(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/ootle" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	h.renderIndex(w, indexData{})
}

// dispenseRequest is the JSON request body shape /ootle/dispense
// accepts when the caller POSTs Content-Type: application/json --
// e.g. a programmatic API caller, as opposed to this package's own
// plain-HTML form submission (form-encoded, handled via
// r.FormValue below).
type dispenseRequest struct {
	Destination string `json:"destination"`
	// Website is the JSON-caller equivalent of the HTML form's hidden
	// honeypot field. A real JSON API caller has no reason to ever
	// set it; a bot blindly replaying form field names into a JSON
	// body gets rejected the same way a bot filling the HTML
	// honeypot field does.
	Website string `json:"website"`
}

// dispenseResponse is the JSON response body shape /ootle/dispense
// returns when the caller requested JSON (see wantsJSON).
type dispenseResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	TxID    string `json:"tx_id,omitempty"`
	Amount  uint64 `json:"amount,omitempty"`
}

// wantsJSON reports whether r should get a JSON response instead of
// the rendered HTML page -- true when the request itself was JSON
// (Content-Type: application/json) or the caller explicitly asked for
// JSON via Accept. The plain HTML form this package serves at GET
// /ootle submits as application/x-www-form-urlencoded (no JS, per this
// repo's existing convention), so a normal form submission always
// falls through to the HTML-rendering path, exactly mirroring how
// internal/faucet.Handler.Request behaves for its own form.
func wantsJSON(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Content-Type"), "application/json") ||
		strings.Contains(r.Header.Get("Accept"), "application/json")
}

// Dispense handles the destination submission -- either a JSON API
// call or the plain HTML form's POST -- rejects honeypot-tripped spam,
// and dispenses Ootle (L2) XTM via Service.Dispense on success. Every
// outcome (honeypot, malformed destination, rate limited, not
// configured, or a wallet-daemon/DB error) is rendered back as a clean
// response instead of a 500, in whichever shape (HTML or JSON) the
// caller asked for.
func (h *Handler) Dispense(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	asJSON := wantsJSON(r)
	var destination, honeypot string
	if strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		var body dispenseRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			h.reject(w, r, asJSON, "", "Could not parse request body.", http.StatusBadRequest)
			return
		}
		destination = body.Destination
		honeypot = body.Website
	} else {
		if err := r.ParseForm(); err != nil {
			h.reject(w, r, asJSON, "", "Could not parse form submission.", http.StatusBadRequest)
			return
		}
		destination = r.FormValue("destination")
		honeypot = r.FormValue(honeypotFieldName)
	}

	ip := clientIP(r)

	if honeypot != "" {
		h.logger().WithFields(logrus.Fields{"ip": ip}).Info("ootle: honeypot field populated, rejecting as spam")
		h.reject(w, r, asJSON, destination, genericRejectionMessage, 0)
		return
	}

	result := h.Service.Dispense(r.Context(), destination, ip)
	switch result.Outcome {
	case OutcomeSuccess:
		h.respondSuccess(w, asJSON, result)
	case OutcomeInvalidAddress:
		h.reject(w, r, asJSON, destination, fmt.Sprintf("That doesn't look like a valid Ootle destination address: %v", result.Err), http.StatusBadRequest)
	case OutcomeRateLimited:
		h.reject(w, r, asJSON, destination, fmt.Sprintf("This address or IP has already received XTM (L2) recently. Try again after %s.", result.RetryAfter.Format("2006-01-02 15:04:05 MST")), http.StatusTooManyRequests)
	case OutcomeNotConfigured:
		h.reject(w, r, asJSON, destination, "The Ootle (L2) faucet is not yet funded -- the faucet component has not been deployed to mainnet. Please check back later.", http.StatusServiceUnavailable)
	default:
		h.reject(w, r, asJSON, destination, genericRejectionMessage, 0)
	}
}

// respondSuccess renders OutcomeSuccess in whichever shape the caller
// asked for.
func (h *Handler) respondSuccess(w http.ResponseWriter, asJSON bool, result Result) {
	if asJSON {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(dispenseResponse{
			Success: true,
			Message: fmt.Sprintf("Success! Sent %s (tx id %s).", formatXTM(result.Amount), result.TxID),
			TxID:    result.TxID,
			Amount:  result.Amount,
		})
		return
	}
	h.renderIndex(w, indexData{
		Message: fmt.Sprintf("Success! Sent %s (tx id %s).", formatXTM(result.Amount), result.TxID),
	})
}

// reject renders any non-success outcome in whichever shape the caller
// asked for. statusCode == 0 means "use the HTML path's default" (400)
// for HTML, and is mapped to 400 for JSON too in that case.
func (h *Handler) reject(w http.ResponseWriter, _ *http.Request, asJSON bool, destination, message string, statusCode int) {
	if asJSON {
		if statusCode == 0 {
			statusCode = http.StatusBadRequest
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(statusCode)
		_ = json.NewEncoder(w).Encode(dispenseResponse{Success: false, Message: message})
		return
	}
	h.renderIndex(w, indexData{
		Destination: destination,
		Message:     message,
		IsError:     true,
		StatusCode:  statusCode,
	})
}

// renderIndex renders data through indexTemplate, filling in
// DispenseAmount from h.Service.Config so every call site gets it
// without having to set it individually -- same centralizing role as
// faucet.Handler.renderIndex.
func (h *Handler) renderIndex(w http.ResponseWriter, data indexData) {
	data.DispenseAmount = h.Service.Config.DispenseAmount
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	switch {
	case data.StatusCode != 0:
		w.WriteHeader(data.StatusCode)
	case data.IsError:
		w.WriteHeader(http.StatusBadRequest)
	}
	_ = indexTemplate.Execute(w, data)
}

func (h *Handler) logger() *logrus.Logger {
	if h.Service != nil && h.Service.Logger != nil {
		return h.Service.Logger
	}
	return logrus.StandardLogger()
}
