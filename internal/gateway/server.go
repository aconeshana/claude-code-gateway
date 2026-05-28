package gateway

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/anthropics/claude-code-gateway/internal/runtime"
	"github.com/anthropics/claude-code-gateway/internal/session"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

type Server struct {
	sessionMgr   *session.Manager
	registry     *runtime.Registry
	defaultKind  string
	upgrader     websocket.Upgrader
	authToken    string
	writeTimeout time.Duration
	httpServer   *http.Server
	readyCheck   func() bool
}

// NewServer constructs a Server. registry maps runtime "kind" → Factory;
// defaultKind is used when a CreateSessionPayload omits the runtime envelope.
func NewServer(mgr *session.Manager, registry *runtime.Registry, defaultKind, listenAddr, authToken string, writeTimeout time.Duration) *Server {
	s := &Server{
		sessionMgr:  mgr,
		registry:    registry,
		defaultKind: defaultKind,
		upgrader: websocket.Upgrader{
			// Intended for programmatic (non-browser) clients. If browser clients
			// are needed, implement proper origin checking here.
			CheckOrigin: func(r *http.Request) bool { return true },
		},
		authToken:    authToken,
		writeTimeout: writeTimeout,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", s.handleWebSocket)
	mux.HandleFunc("/health", s.handleHealth)

	s.httpServer = &http.Server{
		Addr:    listenAddr,
		Handler: mux,
	}

	return s
}

func (s *Server) Start() error {
	log.Printf("[server] listening on %s", s.httpServer.Addr)
	return s.httpServer.ListenAndServe()
}

func (s *Server) Shutdown(ctx context.Context) error {
	return s.httpServer.Shutdown(ctx)
}

func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	if s.authToken != "" {
		token := r.Header.Get("Authorization")
		if len(token) > 7 && token[:7] == "Bearer " {
			token = token[7:]
		}
		if subtle.ConstantTimeCompare([]byte(token), []byte(s.authToken)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}

	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[server] upgrade error: %v", err)
		return
	}

	clientID := uuid.New().String()
	log.Printf("[server] client connected: %s", clientID)

	handler := NewWSHandler(conn, clientID, s.sessionMgr, s.registry, s.defaultKind, s.writeTimeout)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	handler.Run(ctx, cancel)
	log.Printf("[server] client disconnected: %s", clientID)
}

func (s *Server) SetReadyCheck(fn func() bool) {
	s.readyCheck = fn
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	sessions := s.sessionMgr.List()
	ready := s.readyCheck == nil || s.readyCheck()
	status := "ok"
	if !ready {
		status = "warming_up"
	}
	resp := map[string]interface{}{
		"status":   status,
		"ready":    ready,
		"sessions": len(sessions),
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}
