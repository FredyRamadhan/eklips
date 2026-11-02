// Package client provides the shared HTTP and SSE client used by both the CLI
// and the built-in TUI.
package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/frdn/eklips/internal/apperr"
	"github.com/frdn/eklips/internal/config"
	"github.com/frdn/eklips/internal/engine"
	"github.com/frdn/eklips/internal/events"
	"github.com/frdn/eklips/internal/server"
	"github.com/frdn/eklips/internal/store"
)

// Client talks to an eklips daemon over HTTP.
type Client struct {
	baseURL    string
	authToken  string // never logged or persisted
	httpClient *http.Client
	sseClient  *http.Client
}

// New creates a Client for the given base URL and optional bearer token.
func New(baseURL, authToken string) (*Client, error) {
	u, err := config.ValidateRemoteURL(baseURL)
	if err != nil {
		return nil, err
	}
	return &Client{
		baseURL:   u,
		authToken: authToken,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
		sseClient: &http.Client{
			Timeout: 0, // SSE stream is long-lived
		},
	}, nil
}

// FromConfig creates a Client from a Config.
func FromConfig(cfg config.Config) (*Client, error) {
	u, err := cfg.BaseURL()
	if err != nil {
		return nil, err
	}
	return New(u, cfg.AuthToken)
}

// BaseURL returns the daemon's base URL (without any credentials).
func (c *Client) BaseURL() string { return c.baseURL }

// HasToken reports whether an auth token is configured, without exposing it.
func (c *Client) HasToken() bool { return c.authToken != "" }

func (c *Client) doJSON(ctx context.Context, method, path string, reqBody, respBody any) error {
	var bodyReader io.Reader
	if reqBody != nil {
		b, err := json.Marshal(reqBody)
		if err != nil {
			return apperr.Wrap(apperr.Internal, err, "encode request")
		}
		bodyReader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
	if err != nil {
		return apperr.Wrap(apperr.InvalidCommand, err, "build request")
	}
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.authToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.authToken)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return apperr.New(apperr.ServiceNotRunning, "cannot reach daemon at %s: %v", c.baseURL, err)
		}
		return apperr.New(apperr.ServiceNotRunning, "daemon is not running at %s", c.baseURL)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return apperr.Wrap(apperr.Internal, err, "read response")
	}
	if resp.StatusCode >= 400 {
		var env apperr.Envelope
		if json.Unmarshal(data, &env) == nil && env.Error != nil && env.Error.Code != "" {
			return env.Error
		}
		return apperr.New(apperr.Internal, "HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if respBody != nil && len(data) > 0 {
		if err := json.Unmarshal(data, respBody); err != nil {
			return apperr.Wrap(apperr.Internal, err, "decode response")
		}
	}
	return nil
}

// Health checks GET /api/v1/health.
func (c *Client) Health(ctx context.Context) error {
	var out map[string]string
	return c.doJSON(ctx, http.MethodGet, "/api/v1/health", nil, &out)
}

// Status calls GET /api/v1/status.
func (c *Client) Status(ctx context.Context) (engine.Status, error) {
	var st engine.Status
	err := c.doJSON(ctx, http.MethodGet, "/api/v1/status", nil, &st)
	return st, err
}

// Queue calls GET /api/v1/queue.
func (c *Client) Queue(ctx context.Context) (engine.Queue, error) {
	var q engine.Queue
	err := c.doJSON(ctx, http.MethodGet, "/api/v1/queue", nil, &q)
	return q, err
}

// Play calls POST /api/v1/play.
func (c *Client) Play(ctx context.Context, path string, loop bool) (engine.Status, error) {
	var st engine.Status
	var req any
	if path != "" || loop {
		req = server.PlayRequest{Path: path, Loop: loop}
	}
	err := c.doJSON(ctx, http.MethodPost, "/api/v1/play", req, &st)
	return st, err
}

// Pause calls POST /api/v1/pause.
func (c *Client) Pause(ctx context.Context) (engine.Status, error) {
	var st engine.Status
	err := c.doJSON(ctx, http.MethodPost, "/api/v1/pause", nil, &st)
	return st, err
}

// Stop calls POST /api/v1/stop.
func (c *Client) Stop(ctx context.Context) (engine.Status, error) {
	var st engine.Status
	err := c.doJSON(ctx, http.MethodPost, "/api/v1/stop", nil, &st)
	return st, err
}

// Next calls POST /api/v1/next.
func (c *Client) Next(ctx context.Context) (engine.Status, error) {
	var st engine.Status
	err := c.doJSON(ctx, http.MethodPost, "/api/v1/next", nil, &st)
	return st, err
}

// Previous calls POST /api/v1/previous.
func (c *Client) Previous(ctx context.Context) (engine.Status, error) {
	var st engine.Status
	err := c.doJSON(ctx, http.MethodPost, "/api/v1/previous", nil, &st)
	return st, err
}

// VolumeUp calls POST /api/v1/volume/up.
func (c *Client) VolumeUp(ctx context.Context) (engine.Status, error) {
	var st engine.Status
	err := c.doJSON(ctx, http.MethodPost, "/api/v1/volume/up", nil, &st)
	return st, err
}

// VolumeDown calls POST /api/v1/volume/down.
func (c *Client) VolumeDown(ctx context.Context) (engine.Status, error) {
	var st engine.Status
	err := c.doJSON(ctx, http.MethodPost, "/api/v1/volume/down", nil, &st)
	return st, err
}

// Mute calls POST /api/v1/mute.
func (c *Client) Mute(ctx context.Context) (engine.Status, error) {
	var st engine.Status
	err := c.doJSON(ctx, http.MethodPost, "/api/v1/mute", nil, &st)
	return st, err
}

// Shuffle calls POST /api/v1/shuffle.
func (c *Client) Shuffle(ctx context.Context) (engine.Status, error) {
	var st engine.Status
	err := c.doJSON(ctx, http.MethodPost, "/api/v1/shuffle", nil, &st)
	return st, err
}

// Loop calls POST /api/v1/loop.
func (c *Client) Loop(ctx context.Context) (engine.Status, error) {
	var st engine.Status
	err := c.doJSON(ctx, http.MethodPost, "/api/v1/loop", nil, &st)
	return st, err
}

// SeekTo calls POST /api/v1/seek with an absolute position.
func (c *Client) SeekTo(ctx context.Context, posMs int64) (engine.Status, error) {
	var st engine.Status
	err := c.doJSON(ctx, http.MethodPost, "/api/v1/seek", server.SeekRequest{PositionMs: &posMs}, &st)
	return st, err
}

// SeekBy calls POST /api/v1/seek with a relative delta.
func (c *Client) SeekBy(ctx context.Context, deltaMs int64) (engine.Status, error) {
	var st engine.Status
	err := c.doJSON(ctx, http.MethodPost, "/api/v1/seek", server.SeekRequest{DeltaMs: &deltaMs}, &st)
	return st, err
}

// PlayIndex calls POST /api/v1/queue/play.
func (c *Client) PlayIndex(ctx context.Context, index int) (engine.Status, error) {
	var st engine.Status
	err := c.doJSON(ctx, http.MethodPost, "/api/v1/queue/play", server.IndexRequest{Index: index}, &st)
	return st, err
}

// QueueRemove calls POST /api/v1/queue/remove.
func (c *Client) QueueRemove(ctx context.Context, index int) (engine.Status, error) {
	var st engine.Status
	err := c.doJSON(ctx, http.MethodPost, "/api/v1/queue/remove", server.IndexRequest{Index: index}, &st)
	return st, err
}

// QueueMove calls POST /api/v1/queue/move.
func (c *Client) QueueMove(ctx context.Context, from, to int) (engine.Status, error) {
	var st engine.Status
	err := c.doJSON(ctx, http.MethodPost, "/api/v1/queue/move", server.MoveRequest{From: from, To: to}, &st)
	return st, err
}

// QueueClear calls POST /api/v1/queue/clear.
func (c *Client) QueueClear(ctx context.Context) (engine.Status, error) {
	var st engine.Status
	err := c.doJSON(ctx, http.MethodPost, "/api/v1/queue/clear", nil, &st)
	return st, err
}

// QueueAppend calls POST /api/v1/queue/append.
func (c *Client) QueueAppend(ctx context.Context, paths []string) (engine.Status, error) {
	var st engine.Status
	err := c.doJSON(ctx, http.MethodPost, "/api/v1/queue/append", server.QueueAppendRequest{Paths: paths}, &st)
	return st, err
}

// Levels calls GET /api/v1/levels.
func (c *Client) Levels(ctx context.Context) (engine.Levels, error) {
	var lv engine.Levels
	err := c.doJSON(ctx, http.MethodGet, "/api/v1/levels", nil, &lv)
	return lv, err
}

// Browse calls GET /api/v1/browse.
func (c *Client) Browse(ctx context.Context, path string) (engine.BrowseResult, error) {
	var res engine.BrowseResult
	q := ""
	if path != "" {
		q = "?path=" + url.QueryEscape(path)
	}
	err := c.doJSON(ctx, http.MethodGet, "/api/v1/browse"+q, nil, &res)
	return res, err
}

// PlaylistList calls GET /api/v1/playlists.
func (c *Client) PlaylistList(ctx context.Context) ([]engine.PlaylistInfo, error) {
	var resp struct {
		Playlists []engine.PlaylistInfo `json:"playlists"`
	}
	err := c.doJSON(ctx, http.MethodGet, "/api/v1/playlists", nil, &resp)
	return resp.Playlists, err
}

// PlaylistInspect calls GET /api/v1/playlists/tracks.
func (c *Client) PlaylistInspect(ctx context.Context, target string) (engine.PlaylistView, error) {
	var view engine.PlaylistView
	err := c.doJSON(ctx, http.MethodGet, "/api/v1/playlists/tracks?target="+url.QueryEscape(target), nil, &view)
	return view, err
}

// PlaylistRemove calls DELETE /api/v1/playlists.
func (c *Client) PlaylistRemove(ctx context.Context, target string) error {
	var out map[string]string
	return c.doJSON(ctx, http.MethodDelete, "/api/v1/playlists?target="+url.QueryEscape(target), nil, &out)
}

// Shutdown calls POST /api/v1/shutdown.
func (c *Client) Shutdown(ctx context.Context) error {
	var out map[string]string
	return c.doJSON(ctx, http.MethodPost, "/api/v1/shutdown", nil, &out)
}

// PlaylistCreate calls POST /api/v1/playlists/create.
func (c *Client) PlaylistCreate(ctx context.Context, dir, output string, force bool) (string, error) {
	var resp server.PlaylistCreateResponse
	err := c.doJSON(ctx, http.MethodPost, "/api/v1/playlists/create", server.PlaylistCreateRequest{
		Dir:    dir,
		Output: output,
		Force:  force,
	}, &resp)
	return resp.Path, err
}

// PlaylistPlay calls POST /api/v1/playlists/play.
func (c *Client) PlaylistPlay(ctx context.Context, target string, loop bool) (engine.Status, error) {
	var st engine.Status
	err := c.doJSON(ctx, http.MethodPost, "/api/v1/playlists/play", server.PlaylistPlayRequest{
		Target: target,
		Loop:   loop,
	}, &st)
	return st, err
}

// AliasList calls GET /api/v1/aliases.
func (c *Client) AliasList(ctx context.Context) ([]store.Alias, error) {
	var resp struct {
		Aliases []store.Alias `json:"aliases"`
	}
	err := c.doJSON(ctx, http.MethodGet, "/api/v1/aliases", nil, &resp)
	return resp.Aliases, err
}

// AliasSet calls PUT /api/v1/aliases/{name}.
func (c *Client) AliasSet(ctx context.Context, name, path string) (store.Alias, error) {
	var a store.Alias
	err := c.doJSON(ctx, http.MethodPut, "/api/v1/aliases/"+url.PathEscape(name), server.AliasSetRequest{
		Path: path,
	}, &a)
	return a, err
}

// AliasRemove calls DELETE /api/v1/aliases/{name}.
func (c *Client) AliasRemove(ctx context.Context, name string) error {
	var out map[string]string
	return c.doJSON(ctx, http.MethodDelete, "/api/v1/aliases/"+url.PathEscape(name), nil, &out)
}

// SSEEvent is a server-sent event received from GET /api/v1/events.
type SSEEvent struct {
	ID   uint64
	Type events.Type
	Data []byte
}

// SubscribeEvents connects to GET /api/v1/events and streams events onto the
// returned channel until ctx is cancelled or the connection drops.
func (c *Client) SubscribeEvents(ctx context.Context, lastID uint64) (<-chan SSEEvent, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/v1/events", nil)
	if err != nil {
		return nil, apperr.Wrap(apperr.InvalidCommand, err, "build SSE request")
	}
	req.Header.Set("Accept", "text/event-stream")
	if c.authToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.authToken)
	}
	if lastID > 0 {
		req.Header.Set("Last-Event-ID", strconv.FormatUint(lastID, 10))
	}

	resp, err := c.sseClient.Do(req)
	if err != nil {
		return nil, apperr.New(apperr.ServiceNotRunning, "daemon is not running at %s", c.baseURL)
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		var env apperr.Envelope
		if json.Unmarshal(data, &env) == nil && env.Error != nil && env.Error.Code != "" {
			return nil, env.Error
		}
		return nil, apperr.New(apperr.Internal, "SSE HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}

	ch := make(chan SSEEvent, 32)
	go func() {
		defer close(ch)
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64<<10), 1<<20)

		var curID uint64
		var curType events.Type
		var dataBuf bytes.Buffer

		flush := func() bool {
			if curType == "" && dataBuf.Len() == 0 {
				return true
			}
			b := bytes.TrimSuffix(dataBuf.Bytes(), []byte("\n"))
			cp := make([]byte, len(b))
			copy(cp, b)
			ev := SSEEvent{ID: curID, Type: curType, Data: cp}
			curType = ""
			dataBuf.Reset()
			select {
			case ch <- ev:
				return true
			case <-ctx.Done():
				return false
			}
		}

		for sc.Scan() {
			line := sc.Text()
			if line == "" {
				if !flush() {
					return
				}
				continue
			}
			if strings.HasPrefix(line, ":") {
				// Heartbeat comment.
				continue
			}
			field, val, ok := strings.Cut(line, ":")
			if !ok {
				continue
			}
			val = strings.TrimPrefix(val, " ")
			switch field {
			case "id":
				if v, err := strconv.ParseUint(val, 10, 64); err == nil {
					curID = v
				}
			case "event":
				curType = events.Type(val)
			case "data":
				dataBuf.WriteString(val)
				dataBuf.WriteByte('\n')
			}
		}
	}()
	return ch, nil
}

// String returns a safe description without the auth token.
func (c *Client) String() string {
	return fmt.Sprintf("Client(%s)", c.baseURL)
}
