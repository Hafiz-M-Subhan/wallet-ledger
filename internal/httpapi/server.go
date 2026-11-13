// Package httpapi exposes the ledger over HTTP/JSON.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"wallet-ledger/internal/ledger"
)

// Ledger is what the HTTP layer needs from storage.
type Ledger interface {
	Apply(ctx context.Context, op ledger.Op, userID string, amount int64, idemKey string) (int64, error)
	Balance(ctx context.Context, userID string) (int64, error)
	Ping(ctx context.Context) error
}

type Server struct {
	ledger      Ledger
	auth        authenticator
	log         *slog.Logger
	readTimeout time.Duration
}

func New(l Ledger, token string, log *slog.Logger) *Server {
	return &Server{ledger: l, auth: newAuthenticator(token), log: log, readTimeout: 5 * time.Second}
}

// ServeHTTP routes by hand: three endpoints do not need a router, and this
// keeps the guarantee obvious that authentication runs first for every path,
// before any body parsing or database access.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !s.auth.allowed(r.Header.Get("Authorization")) {
		w.Header().Set("WWW-Authenticate", `Bearer`)
		writeError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid bearer token")
		return
	}

	path := r.URL.Path
	switch {
	case path == "/credit":
		s.handleWrite(w, r, ledger.OpCredit)
	case path == "/debit":
		s.handleWrite(w, r, ledger.OpDebit)
	case strings.HasPrefix(path, "/balance/"):
		s.handleBalance(w, r, path[len("/balance/"):])
	case path == "/health":
		s.handleHealth(w, r)
	default:
		writeError(w, http.StatusNotFound, "not_found", "no such endpoint")
	}
}

func (s *Server) handleWrite(w http.ResponseWriter, r *http.Request, op ledger.Op) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "could not read request body")
		return
	}
	req, msg := parseOpRequest(body)
	if msg != "" {
		writeError(w, http.StatusBadRequest, "invalid_request", msg)
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if key != "" && !validIdempotencyKey(key) {
		writeError(w, http.StatusBadRequest, "invalid_request", "Idempotency-Key must be 1-255 visible ASCII characters")
		return
	}

	bal, err := s.ledger.Apply(r.Context(), op, req.UserID, req.Amount, key)
	if err != nil {
		s.writeLedgerError(w, r, err)
		return
	}
	writeBalance(w, req.UserID, bal)
}

func (s *Server) handleBalance(w http.ResponseWriter, r *http.Request, userID string) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	if !validUserID(userID) {
		writeError(w, http.StatusBadRequest, "invalid_request", msgUserInvalid)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.readTimeout)
	defer cancel()
	bal, err := s.ledger.Balance(ctx, userID)
	if err != nil {
		s.writeLedgerError(w, r, err)
		return
	}
	writeBalance(w, userID, bal)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.ledger.Ping(ctx); err != nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "database unreachable")
		return
	}
	writeJSON(w, http.StatusOK, []byte(`{"status":"ok"}`))
}

func (s *Server) writeLedgerError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ledger.ErrUserNotFound):
		writeError(w, http.StatusNotFound, "user_not_found", "user has never been credited")
	case errors.Is(err, ledger.ErrInsufficientFunds):
		writeError(w, http.StatusConflict, "insufficient_funds", "balance is lower than the debit amount")
	case errors.Is(err, ledger.ErrIdempotencyKeyReused):
		writeError(w, http.StatusUnprocessableEntity, "idempotency_key_reused", "Idempotency-Key was already used with a different request")
	case errors.Is(err, ledger.ErrBalanceOverflow):
		writeError(w, http.StatusUnprocessableEntity, "balance_limit_exceeded", "credit would exceed the maximum balance")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusServiceUnavailable, "timeout", "request timed out before it was applied")
	default:
		s.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal error")
	}
}

func methodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use "+allow)
}

// writeBalance builds the body by hand: user_id has already been validated to
// [A-Za-z0-9_-], so it never needs JSON escaping.
func writeBalance(w http.ResponseWriter, userID string, balance int64) {
	b := make([]byte, 0, 48+len(userID))
	b = append(b, `{"user_id":"`...)
	b = append(b, userID...)
	b = append(b, `","balance":`...)
	b = strconv.AppendInt(b, balance, 10)
	b = append(b, '}')
	writeJSON(w, http.StatusOK, b)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	b, _ := json.Marshal(struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}{code, message})
	writeJSON(w, status, b)
}

func writeJSON(w http.ResponseWriter, status int, body []byte) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	w.Write(body) //nolint:errcheck
}
