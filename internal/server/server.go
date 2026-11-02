// Package server implements the HTTP API v1 control plane, SSE event stream,
// and HTTP audio streaming endpoint.
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/frdn/eklips/internal/apperr"
	"github.com/frdn/eklips/internal/config"
	"github.com/frdn/eklips/internal/engine"
	"github.com/frdn/eklips/internal/events"
	"github.com/frdn/eklips/internal/sink"
)

// MaxBodyBytes caps JSON request body sizes (64 KiB).
const MaxBodyBytes = 64 << 10

// Options configures the HTTP server.
type Options struct {
	Addr              string
	AuthToken         string
	HeartbeatInterval time.Duration
	OnShutdown        func()
}

// Server wraps the HTTP handler and underlying net/http.Server.
type Server struct {
	player     engine.Player
	opts       Options
	mux        *http.ServeMux
	httpSrv    *http.Server
	shutdownCh chan struct{}
	once       sync.Once
}

// New creates an HTTP API v1 server for player.
func New(player engine.Player, opts Options) *Server {
	if opts.HeartbeatInterval <= 0 {
		opts.HeartbeatInterval = 15 * time.Second
	}
	s := &Server{
		player:     player,
		opts:       opts,
		mux:        http.NewServeMux(),
		shutdownCh: make(chan struct{}),
	}
	s.registerRoutes()
	s.httpSrv = &http.Server{
		Addr:              opts.Addr,
		Handler:           s,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      0, // disabled globally for /stream and /events; set per control request
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	return s
}

// ServeHTTP implements http.Handler with auth, body size limits, and recovery.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/api/v1/health" {
		if err := s.checkAuth(r); err != nil {
			writeError(w, err)
			return
		}
	}
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, MaxBodyBytes)
	}
	s.mux.ServeHTTP(w, r)
}

func (s *Server) checkAuth(r *http.Request) error {
	if s.opts.AuthToken == "" {
		return nil
	}
	hdr := strings.TrimSpace(r.Header.Get("Authorization"))
	if hdr == "" {
		return apperr.New(apperr.AuthRequired, "missing Authorization header")
	}
	prefix, token, ok := strings.Cut(hdr, " ")
	if !ok || !strings.EqualFold(prefix, "Bearer") || strings.TrimSpace(token) == "" {
		return apperr.New(apperr.Unauthorized, "invalid Authorization header format")
	}
	token = strings.TrimSpace(token)
	expected := s.opts.AuthToken
	if len(token) != len(expected) || subtle.ConstantTimeCompare([]byte(token), []byte(expected)) != 1 {
		return apperr.New(apperr.Unauthorized, "invalid bearer token")
	}
	return nil
}

func (s *Server) registerRoutes() {
	s.mux.HandleFunc("GET /api/v1/health", s.handleHealth)
	s.mux.HandleFunc("GET /api/v1/status", s.handleStatus)
	s.mux.HandleFunc("GET /api/v1/queue", s.handleQueue)
	s.mux.HandleFunc("GET /api/v1/events", s.handleEvents)
	s.mux.HandleFunc("GET /api/v1/stream", s.handleStream)
	s.mux.HandleFunc("HEAD /api/v1/stream", s.handleStream)

	s.mux.HandleFunc("POST /api/v1/play", s.handlePlay)
	s.mux.HandleFunc("POST /api/v1/pause", s.handlePause)
	s.mux.HandleFunc("POST /api/v1/stop", s.handleStop)
	s.mux.HandleFunc("POST /api/v1/next", s.handleNext)
	s.mux.HandleFunc("POST /api/v1/previous", s.handlePrevious)
	s.mux.HandleFunc("POST /api/v1/prev", s.handlePrevious)
	s.mux.HandleFunc("POST /api/v1/volume/up", s.handleVolumeUp)
	s.mux.HandleFunc("POST /api/v1/volume/down", s.handleVolumeDown)
	s.mux.HandleFunc("POST /api/v1/mute", s.handleMute)
	s.mux.HandleFunc("POST /api/v1/shuffle", s.handleShuffle)
	s.mux.HandleFunc("POST /api/v1/loop", s.handleLoop)
	s.mux.HandleFunc("POST /api/v1/shutdown", s.handleShutdown)

	s.mux.HandleFunc("POST /api/v1/seek", s.handleSeek)
	s.mux.HandleFunc("POST /api/v1/queue/play", s.handleQueuePlay)
	s.mux.HandleFunc("POST /api/v1/queue/remove", s.handleQueueRemove)
	s.mux.HandleFunc("POST /api/v1/queue/move", s.handleQueueMove)
	s.mux.HandleFunc("POST /api/v1/queue/clear", s.handleQueueClear)
	s.mux.HandleFunc("POST /api/v1/queue/append", s.handleQueueAppend)
	s.mux.HandleFunc("GET /api/v1/levels", s.handleLevels)
	s.mux.HandleFunc("GET /api/v1/browse", s.handleBrowse)

	s.mux.HandleFunc("POST /api/v1/playlists/create", s.handlePlaylistCreate)
	s.mux.HandleFunc("POST /api/v1/playlists/play", s.handlePlaylistPlay)
	s.mux.HandleFunc("GET /api/v1/playlists", s.handlePlaylistList)
	s.mux.HandleFunc("GET /api/v1/playlists/tracks", s.handlePlaylistInspect)
	s.mux.HandleFunc("DELETE /api/v1/playlists", s.handlePlaylistDelete)

	s.mux.HandleFunc("GET /api/v1/aliases", s.handleAliasList)
	s.mux.HandleFunc("PUT /api/v1/aliases/{name}", s.handleAliasSet)
	s.mux.HandleFunc("DELETE /api/v1/aliases/{name}", s.handleAliasDelete)

	s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, apperr.New(apperr.InvalidCommand, "unknown endpoint %s %s", r.Method, r.URL.Path))
	})
}

// PlayRequest is the optional JSON body for POST /api/v1/play.
type PlayRequest struct {
	Path string `json:"path,omitempty"`
	Loop bool   `json:"loop,omitempty"`
}

// PlaylistCreateRequest is the JSON body for POST /api/v1/playlists/create.
type PlaylistCreateRequest struct {
	Dir    string `json:"dir,omitempty"`
	Output string `json:"output,omitempty"`
	Force  bool   `json:"force,omitempty"`
}

// PlaylistCreateResponse is the JSON response for POST /api/v1/playlists/create.
type PlaylistCreateResponse struct {
	Path string `json:"path"`
}

// PlaylistPlayRequest is the JSON body for POST /api/v1/playlists/play.
type PlaylistPlayRequest struct {
	Target string `json:"target,omitempty"`
	Path   string `json:"path,omitempty"`
	Alias  string `json:"alias,omitempty"`
	Loop   bool   `json:"loop,omitempty"`
}

// AliasSetRequest is the JSON body for PUT /api/v1/aliases/{name}.
type AliasSetRequest struct {
	Path         string `json:"path,omitempty"`
	PlaylistPath string `json:"playlist_path,omitempty"`
}

// SeekRequest is the JSON body for POST /api/v1/seek.
type SeekRequest struct {
	PositionMs *int64 `json:"position_ms,omitempty"`
	DeltaMs    *int64 `json:"delta_ms,omitempty"`
}

// IndexRequest is the JSON body for queue index operations.
type IndexRequest struct {
	Index int `json:"index"`
}

// MoveRequest is the JSON body for POST /api/v1/queue/move.
type MoveRequest struct {
	From int `json:"from"`
	To   int `json:"to"`
}

// QueueAppendRequest is the JSON body for POST /api/v1/queue/append.
type QueueAppendRequest struct {
	Paths []string `json:"paths"`
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"status":  "ok",
		"version": config.Version,
	})
}

func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.player.Status())
}

func (s *Server) handleQueue(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.player.Queue())
}

func (s *Server) handlePlay(w http.ResponseWriter, r *http.Request) {
	var req PlayRequest
	if err := decodeOptionalJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	st, err := s.player.Play(req.Path, req.Loop)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handlePause(w http.ResponseWriter, r *http.Request) {
	if err := drainEmptyBody(r); err != nil {
		writeError(w, err)
		return
	}
	st, err := s.player.Pause()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	if err := drainEmptyBody(r); err != nil {
		writeError(w, err)
		return
	}
	st, err := s.player.Stop()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleNext(w http.ResponseWriter, r *http.Request) {
	if err := drainEmptyBody(r); err != nil {
		writeError(w, err)
		return
	}
	st, err := s.player.Next()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handlePrevious(w http.ResponseWriter, r *http.Request) {
	if err := drainEmptyBody(r); err != nil {
		writeError(w, err)
		return
	}
	st, err := s.player.Previous()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleVolumeUp(w http.ResponseWriter, r *http.Request) {
	if err := drainEmptyBody(r); err != nil {
		writeError(w, err)
		return
	}
	st, err := s.player.VolumeUp()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleVolumeDown(w http.ResponseWriter, r *http.Request) {
	if err := drainEmptyBody(r); err != nil {
		writeError(w, err)
		return
	}
	st, err := s.player.VolumeDown()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleMute(w http.ResponseWriter, r *http.Request) {
	if err := drainEmptyBody(r); err != nil {
		writeError(w, err)
		return
	}
	st, err := s.player.ToggleMute()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleShuffle(w http.ResponseWriter, r *http.Request) {
	if err := drainEmptyBody(r); err != nil {
		writeError(w, err)
		return
	}
	st, err := s.player.ToggleShuffle()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleLoop(w http.ResponseWriter, r *http.Request) {
	if err := drainEmptyBody(r); err != nil {
		writeError(w, err)
		return
	}
	st, err := s.player.ToggleLoop()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleShutdown(w http.ResponseWriter, r *http.Request) {
	if err := drainEmptyBody(r); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "shutting_down"})
	if s.opts.OnShutdown != nil {
		go s.opts.OnShutdown()
	}
}

func (s *Server) handleSeek(w http.ResponseWriter, r *http.Request) {
	var req SeekRequest
	if err := decodeRequiredJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	if req.PositionMs == nil && req.DeltaMs == nil {
		writeError(w, apperr.New(apperr.InvalidCommand, "provide position_ms or delta_ms"))
		return
	}
	var (
		st  engine.Status
		err error
	)
	if req.PositionMs != nil {
		st, err = s.player.SeekTo(*req.PositionMs)
	} else {
		st, err = s.player.SeekBy(*req.DeltaMs)
	}
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleQueuePlay(w http.ResponseWriter, r *http.Request) {
	var req IndexRequest
	if err := decodeRequiredJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	st, err := s.player.PlayIndex(req.Index)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleQueueRemove(w http.ResponseWriter, r *http.Request) {
	var req IndexRequest
	if err := decodeRequiredJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	st, err := s.player.QueueRemove(req.Index)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleQueueMove(w http.ResponseWriter, r *http.Request) {
	var req MoveRequest
	if err := decodeRequiredJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	st, err := s.player.QueueMove(req.From, req.To)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleQueueClear(w http.ResponseWriter, r *http.Request) {
	if err := drainEmptyBody(r); err != nil {
		writeError(w, err)
		return
	}
	st, err := s.player.QueueClear()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleQueueAppend(w http.ResponseWriter, r *http.Request) {
	var req QueueAppendRequest
	if err := decodeRequiredJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	if len(req.Paths) == 0 {
		writeError(w, apperr.New(apperr.InvalidCommand, "paths must not be empty"))
		return
	}
	st, err := s.player.QueueAppend(req.Paths)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleLevels(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.player.Levels())
}

func (s *Server) handleBrowse(w http.ResponseWriter, r *http.Request) {
	res, err := s.player.Browse(r.URL.Query().Get("path"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handlePlaylistList(w http.ResponseWriter, _ *http.Request) {
	list, err := s.player.PlaylistList()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"playlists": list})
}

func (s *Server) handlePlaylistInspect(w http.ResponseWriter, r *http.Request) {
	target := r.URL.Query().Get("target")
	if strings.TrimSpace(target) == "" {
		target = r.URL.Query().Get("path")
	}
	view, err := s.player.PlaylistInspect(target)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) handlePlaylistDelete(w http.ResponseWriter, r *http.Request) {
	target := r.URL.Query().Get("target")
	if strings.TrimSpace(target) == "" {
		target = r.URL.Query().Get("path")
	}
	if err := s.player.PlaylistRemove(target); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"deleted": target})
}

func (s *Server) handlePlaylistCreate(w http.ResponseWriter, r *http.Request) {
	var req PlaylistCreateRequest
	if err := decodeOptionalJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	path, err := s.player.PlaylistCreate(req.Dir, req.Output, req.Force)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, PlaylistCreateResponse{Path: path})
}

func (s *Server) handlePlaylistPlay(w http.ResponseWriter, r *http.Request) {
	var req PlaylistPlayRequest
	if err := decodeRequiredJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	target := req.Target
	if target == "" {
		target = req.Path
	}
	if target == "" {
		target = req.Alias
	}
	st, err := s.player.PlaylistPlay(target, req.Loop)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleAliasList(w http.ResponseWriter, _ *http.Request) {
	list, err := s.player.AliasList()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"aliases": list})
}

func (s *Server) handleAliasSet(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req AliasSetRequest
	if err := decodeRequiredJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	p := req.Path
	if p == "" {
		p = req.PlaylistPath
	}
	if strings.TrimSpace(p) == "" {
		writeError(w, apperr.New(apperr.InvalidCommand, "missing playlist path"))
		return
	}
	alias, err := s.player.AliasSet(name, p)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, alias)
}

func (s *Server) handleAliasDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.player.AliasRemove(name); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"deleted": name})
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, apperr.New(apperr.Internal, "streaming unsupported"))
		return
	}
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})

	var lastID uint64
	if raw := strings.TrimSpace(r.Header.Get("Last-Event-ID")); raw != "" {
		if v, err := strconv.ParseUint(raw, 10, 64); err == nil {
			lastID = v
		}
	}

	bus := s.player.Events()
	sub, replay, replayed := bus.Subscribe(lastID)
	defer bus.Unsubscribe(sub)

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache, no-store")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	if replayed {
		for _, ev := range replay {
			if err := writeSSE(w, ev); err != nil {
				return
			}
		}
	} else {
		snapData, _ := json.Marshal(s.player.Status())
		snapEv := events.Event{
			ID:   bus.LastID(),
			Type: events.Snapshot,
			Data: snapData,
		}
		if err := writeSSE(w, snapEv); err != nil {
			return
		}
	}
	flusher.Flush()

	ticker := time.NewTicker(s.opts.HeartbeatInterval)
	defer ticker.Stop()

	ctx := r.Context()
	for {
		select {
		case ev, ok := <-sub.C:
			if !ok {
				return
			}
			_ = rc.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := writeSSE(w, ev); err != nil {
				return
			}
			flusher.Flush()
		case <-ticker.C:
			_ = rc.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-ctx.Done():
			return
		case <-s.shutdownCh:
			return
		}
	}
}

func writeSSE(w io.Writer, ev events.Event) error {
	if ev.ID > 0 {
		if _, err := fmt.Fprintf(w, "id: %d\n", ev.ID); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, ev.Data)
	return err
}

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	bcast := s.player.Broadcaster()
	if bcast == nil {
		writeError(w, apperr.New(apperr.SinkUnavailable, "HTTP streaming sink is not available"))
		return
	}
	if err := bcast.ServeHTTP(w, r, s.shutdownCh); err != nil && !errors.Is(err, sink.ErrEvicted) {
		writeError(w, err)
	}
}

// Start binds to s.opts.Addr and serves requests until Shutdown is called.
func (s *Server) Start() (net.Listener, error) {
	ln, err := net.Listen("tcp", s.opts.Addr)
	if err != nil {
		return nil, err
	}
	go func() {
		_ = s.httpSrv.Serve(ln)
	}()
	return ln, nil
}

// Shutdown gracefully stops the HTTP server and closes active streams.
func (s *Server) Shutdown(ctx context.Context) error {
	s.once.Do(func() {
		close(s.shutdownCh)
	})
	return s.httpSrv.Shutdown(ctx)
}

func decodeOptionalJSON(r *http.Request, dst any) error {
	if r.Body == nil {
		return nil
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return apperr.Wrap(apperr.InvalidCommand, err, "read request body")
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return apperr.Wrap(apperr.InvalidCommand, err, "invalid JSON request body")
	}
	if dec.More() {
		return apperr.New(apperr.InvalidCommand, "trailing data after JSON request body")
	}
	return nil
}

func decodeRequiredJSON(r *http.Request, dst any) error {
	if r.Body == nil {
		return apperr.New(apperr.InvalidCommand, "missing JSON request body")
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return apperr.Wrap(apperr.InvalidCommand, err, "read request body")
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return apperr.New(apperr.InvalidCommand, "empty JSON request body")
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return apperr.Wrap(apperr.InvalidCommand, err, "invalid JSON request body")
	}
	if dec.More() {
		return apperr.New(apperr.InvalidCommand, "trailing data after JSON request body")
	}
	return nil
}

func drainEmptyBody(r *http.Request) error {
	if r.Body == nil {
		return nil
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return apperr.Wrap(apperr.InvalidCommand, err, "read request body")
	}
	if s := strings.TrimSpace(string(data)); s != "" && s != "{}" {
		return apperr.New(apperr.InvalidCommand, "unexpected request body")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Now().Add(10 * time.Second))
	b, err := json.Marshal(v)
	if err != nil {
		writeError(w, apperr.Wrap(apperr.Internal, err, "encode response"))
		return
	}
	b = append(b, '\n')
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

func writeError(w http.ResponseWriter, err error) {
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Now().Add(10 * time.Second))
	ae := apperr.As(err)
	status := apperr.HTTPStatus(ae.Code)
	b := append(apperr.MarshalEnvelope(ae), '\n')
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}
