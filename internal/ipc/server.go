package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/Svector-anu/claude-go/internal/registry"
)

const (
	maxBody         = 64 << 10
	maxSocketPath   = 100 // sun_path is 104 bytes on macOS
	acceptTimeout   = 25 * time.Second
	replayWindow    = 2 * time.Minute
	activeSafety    = 90 * time.Second
	maxHistoryLimit = 200
	maxWindows      = 8
)

var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)

// validWindowKinds are the rate-limit windows Claude reports.
var validWindowKinds = map[string]bool{"five_hour": true, "seven_day": true, "spend_limit": true}

// Controller is what the server asks of the Go controller. It is the only
// place account decisions are made.
type Controller interface {
	Status() Status
	Accounts() ([]AccountView, error)
	History(limit int) ([]HistoryEntry, error)
	// RequestHandoff validates the request and, if it passes, starts the
	// handoff. It returns once the request is accepted or refused; finished is
	// closed when the handoff has completed.
	RequestHandoff(ctx context.Context, to string) (finished <-chan struct{}, err error)
	RecordUsage(windows []Window) error
}

// Server serves the controller on a Unix socket.
type Server struct {
	// AcceptTimeout bounds how long a handoff request may wait to be accepted.
	AcceptTimeout time.Duration

	ctrl Controller
	now  func() time.Time

	mu      sync.Mutex
	active  string    // request id of the handoff in progress
	until   time.Time // safety release for active
	replays map[string]replay

	ln   net.Listener
	srv  *http.Server
	path string
}

type replay struct {
	at       time.Time
	to       string
	rejected *ErrorDetail
}

// NewServer returns a server for ctrl. now may be nil.
func NewServer(ctrl Controller, now func() time.Time) *Server {
	if now == nil {
		now = time.Now
	}
	return &Server{ctrl: ctrl, now: now, replays: map[string]replay{}}
}

// Listen binds the socket at path with mode 0600 and starts serving. A socket
// left by a dead process is replaced; one that answers is never stolen.
func (s *Server) Listen(path string) error {
	if len(path) > maxSocketPath {
		return fmt.Errorf("socket path is %d bytes, over the %d the OS allows: %s", len(path), maxSocketPath, path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create socket dir: %w", err)
	}
	if c, err := net.DialTimeout("unix", path, 300*time.Millisecond); err == nil {
		_ = c.Close()
		return fmt.Errorf("another process is already serving %s", path)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove stale socket: %w", err)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		_ = os.Remove(path)
		return fmt.Errorf("restrict socket permissions: %w", err)
	}
	s.ln, s.path = ln, path
	s.srv = &http.Server{
		Handler:           s.routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      acceptTimeout + 10*time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	go func() { _ = s.srv.Serve(ln) }()
	return nil
}

// Close stops serving and removes the socket.
func (s *Server) Close() error {
	if s.srv == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := s.srv.Shutdown(ctx)
	if err != nil {
		err = s.srv.Close()
	}
	_ = os.Remove(s.path)
	return err
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/health", s.get(s.health))
	mux.HandleFunc("/v1/status", s.get(s.status))
	mux.HandleFunc("/v1/accounts", s.get(s.accounts))
	mux.HandleFunc("/v1/history", s.get(s.history))
	mux.HandleFunc("/v1/handoff", s.post(s.handoff))
	mux.HandleFunc("/v1/usage", s.post(s.usage))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, CodeNotFound, "unknown path")
	})
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, ErrorBody{Error: ErrorDetail{Code: code, Message: msg}, Version: APIVersion})
}

func (s *Server) get(h func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeError(w, http.StatusMethodNotAllowed, CodeMethod, "use GET")
			return
		}
		h(w, r)
	}
}

func (s *Server) post(h func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeError(w, http.StatusMethodNotAllowed, CodeMethod, "use POST")
			return
		}
		h(w, r)
	}
}

// decode reads one strict JSON object: size-limited, no unknown fields, no trailing data.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if ct := r.Header.Get("Content-Type"); ct != "" && ct != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, CodeInvalid, "Content-Type must be application/json")
		return false
	}
	body := http.MaxBytesReader(w, r.Body, maxBody)
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, CodeTooLarge, "request body too large")
		} else {
			writeError(w, http.StatusBadRequest, CodeInvalid, "malformed request: "+err.Error())
		}
		return false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, CodeInvalid, "unexpected data after the JSON object")
		return false
	}
	return true
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	st := s.ctrl.Status()
	writeJSON(w, http.StatusOK, Health{OK: true, APIVersion: APIVersion, PID: st.PID, Version: st.Version})
}

func (s *Server) status(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.ctrl.Status())
}

func (s *Server) accounts(w http.ResponseWriter, _ *http.Request) {
	accts, err := s.ctrl.Accounts()
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternal, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, AccountsResponse{Accounts: accts})
}

func (s *Server) history(w http.ResponseWriter, r *http.Request) {
	limit := 10
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxHistoryLimit {
			writeError(w, http.StatusBadRequest, CodeInvalid, fmt.Sprintf("limit must be 1-%d", maxHistoryLimit))
			return
		}
		limit = n
	}
	entries, err := s.ctrl.History(limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternal, err.Error())
		return
	}
	if entries == nil {
		entries = []HistoryEntry{}
	}
	writeJSON(w, http.StatusOK, HistoryResponse{Entries: entries})
}

func (s *Server) usage(w http.ResponseWriter, r *http.Request) {
	var req UsageReport
	if !decode(w, r, &req) {
		return
	}
	if len(req.Windows) > maxWindows {
		writeError(w, http.StatusBadRequest, CodeInvalid, fmt.Sprintf("at most %d windows", maxWindows))
		return
	}
	for _, win := range req.Windows {
		if !validWindowKinds[win.Kind] {
			writeError(w, http.StatusBadRequest, CodeInvalid, fmt.Sprintf("unknown window kind %q", win.Kind))
			return
		}
		if win.PercentUsed < 0 || win.PercentUsed > 1000 || win.PercentUsed != win.PercentUsed {
			writeError(w, http.StatusBadRequest, CodeInvalid, "percent_used out of range")
			return
		}
	}
	if err := s.ctrl.RecordUsage(req.Windows); err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternal, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, OK{OK: true})
}

func (s *Server) handoff(w http.ResponseWriter, r *http.Request) {
	var req HandoffRequest
	if !decode(w, r, &req) {
		return
	}
	if !requestIDPattern.MatchString(req.RequestID) {
		writeError(w, http.StatusBadRequest, CodeInvalid, "request_id must be 8-64 characters of A-Z, a-z, 0-9, '-' or '_'")
		return
	}
	if err := registry.ValidateID(req.To); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalid, err.Error())
		return
	}

	now := s.now()
	s.mu.Lock()
	for id, rp := range s.replays {
		if now.Sub(rp.at) > replayWindow {
			delete(s.replays, id)
		}
	}
	if rp, ok := s.replays[req.RequestID]; ok {
		s.mu.Unlock()
		if rp.rejected != nil {
			writeJSON(w, http.StatusUnprocessableEntity, ErrorBody{Error: *rp.rejected, Version: APIVersion})
			return
		}
		writeJSON(w, http.StatusAccepted, HandoffAccepted{RequestID: req.RequestID, Accepted: true, To: rp.to, Replayed: true})
		return
	}
	if s.active != "" && now.Before(s.until) {
		active := s.active
		s.mu.Unlock()
		writeJSON(w, http.StatusConflict, ErrorBody{Version: APIVersion, Error: ErrorDetail{
			Code: CodeBusy, Message: "another handoff is in progress", RequestID: active}})
		return
	}
	s.active, s.until = req.RequestID, now.Add(activeSafety)
	s.mu.Unlock()

	timeout := s.AcceptTimeout
	if timeout <= 0 {
		timeout = acceptTimeout
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	finished, err := s.ctrl.RequestHandoff(ctx, req.To)
	if err != nil {
		detail := ErrorDetail{Code: CodeRejected, Message: err.Error()}
		if errors.Is(err, context.DeadlineExceeded) {
			detail = ErrorDetail{Code: CodeTimeout, Message: "the controller did not answer in time"}
		}
		s.mu.Lock()
		s.active = ""
		s.replays[req.RequestID] = replay{at: now, rejected: &detail}
		s.mu.Unlock()
		status := http.StatusUnprocessableEntity
		if detail.Code == CodeTimeout {
			status = http.StatusGatewayTimeout
		}
		writeJSON(w, status, ErrorBody{Error: detail, Version: APIVersion})
		return
	}
	s.mu.Lock()
	s.replays[req.RequestID] = replay{at: now, to: req.To}
	s.mu.Unlock()
	go s.release(req.RequestID, finished)
	writeJSON(w, http.StatusAccepted, HandoffAccepted{RequestID: req.RequestID, Accepted: true, To: req.To})
}

// release frees the in-progress slot once the handoff finishes.
func (s *Server) release(id string, finished <-chan struct{}) {
	select {
	case <-finished:
	case <-time.After(activeSafety):
	}
	s.mu.Lock()
	if s.active == id {
		s.active = ""
	}
	s.mu.Unlock()
}
