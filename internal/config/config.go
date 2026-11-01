// Package config holds daemon and client configuration, validation, and path
// security helpers.
package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/frdn/eklips/internal/apperr"
	"github.com/frdn/eklips/internal/sink"
	"github.com/frdn/eklips/internal/store"
)

// Version of the eklips binary.
const Version = "1.0.0"

// Defaults.
const (
	DefaultBind         = "127.0.0.1"
	DefaultPort         = 8080
	DefaultSampleRate   = 44100
	DefaultMaxListeners = 32
	DefaultMaxQueue     = 10000
)

// Config holds daemon and client settings.
type Config struct {
	DBPath        string
	Sink          sink.Kind
	Bind          string
	Port          int
	AuthToken     string
	AllowRemote   bool
	ResumeOnStart bool
	MediaRoot     string
	SampleRate    int
	MaxListeners  int
	MaxQueue      int

	// TUI / client flags.
	Remote     string
	NoStart    bool
	StopOnExit bool
}

// Default returns a Config populated with safe defaults and environment
// overrides (`EKLIPS_TOKEN`, `EKLIPS_DB`, `EKLIPS_PORT`, `EKLIPS_BIND`,
// `EKLIPS_SINK`).
func Default() Config {
	c := Config{
		DBPath:       store.DefaultPath(),
		Sink:         sink.KindDevice,
		Bind:         DefaultBind,
		Port:         DefaultPort,
		AuthToken:    os.Getenv("EKLIPS_TOKEN"),
		SampleRate:   DefaultSampleRate,
		MaxListeners: DefaultMaxListeners,
		MaxQueue:     DefaultMaxQueue,
	}
	if v := os.Getenv("EKLIPS_DB"); v != "" {
		c.DBPath = v
	}
	if v := os.Getenv("EKLIPS_BIND"); v != "" {
		c.Bind = v
	}
	if v := os.Getenv("EKLIPS_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			c.Port = p
		}
	}
	if v := os.Getenv("EKLIPS_SINK"); v != "" {
		if k, ok := sink.ParseKind(v); ok {
			c.Sink = k
		}
	}
	return c
}

// Addr returns the TCP listen address ("host:port").
func (c Config) Addr() string {
	return net.JoinHostPort(c.Bind, strconv.Itoa(c.Port))
}

// BaseURL returns the HTTP base URL for client connections.
func (c Config) BaseURL() (string, error) {
	if c.Remote != "" {
		return ValidateRemoteURL(c.Remote)
	}
	host := c.Bind
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, strconv.Itoa(c.Port)), nil
}

// IsLoopbackBind reports whether Bind is a loopback address.
func (c Config) IsLoopbackBind() bool {
	if strings.EqualFold(c.Bind, "localhost") {
		return true
	}
	ip := net.ParseIP(c.Bind)
	return ip != nil && ip.IsLoopback()
}

// ValidateDaemon validates daemon startup settings, including non-loopback
// authentication enforcement.
func (c Config) ValidateDaemon() error {
	if _, ok := sink.ParseKind(string(c.Sink)); !ok {
		return apperr.New(apperr.InvalidCommand, "invalid --sink %q (expected device, http, both, or null)", c.Sink)
	}
	if c.Port < 0 || c.Port > 65535 {
		return apperr.New(apperr.InvalidCommand, "invalid --port %d (must be 0..65535)", c.Port)
	}
	if c.SampleRate < 8000 || c.SampleRate > 192000 {
		return apperr.New(apperr.InvalidCommand, "invalid --sample-rate %d", c.SampleRate)
	}
	if !c.IsLoopbackBind() && c.AuthToken == "" && !c.AllowRemote {
		return apperr.New(apperr.AuthRequired,
			"refusing to bind to non-loopback address %s without --auth-token (or EKLIPS_TOKEN); pass --allow-remote to override",
			c.Bind)
	}
	return nil
}

// ForwardedDaemonArgs returns the CLI flags to forward when auto-starting a
// background daemon from `eklips start` or `eklips tui`. Note: AuthToken is
// passed via environment variable `EKLIPS_TOKEN`, never on the command line,
// so it does not appear in process listings or logs.
func (c Config) ForwardedDaemonArgs() []string {
	args := []string{
		"serve",
		"--db", c.DBPath,
		"--sink", string(c.Sink),
		"--bind", c.Bind,
		"--port", strconv.Itoa(c.Port),
	}
	if c.AllowRemote {
		args = append(args, "--allow-remote")
	}
	if c.ResumeOnStart {
		args = append(args, "--resume-on-start")
	}
	if c.MediaRoot != "" {
		args = append(args, "--media-root", c.MediaRoot)
	}
	return args
}

// ValidateRemoteURL ensures rawURL uses http or https and has a host.
func ValidateRemoteURL(rawURL string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return "", apperr.Wrap(apperr.InvalidCommand, err, "invalid --remote URL")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", apperr.New(apperr.InvalidCommand, "invalid --remote scheme %q (only http and https are allowed)", u.Scheme)
	}
	if u.Host == "" {
		return "", apperr.New(apperr.InvalidCommand, "invalid --remote URL: missing host")
	}
	if u.User != nil {
		return "", apperr.New(apperr.InvalidCommand, "--remote URL must not embed credentials; use --auth-token or EKLIPS_TOKEN")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawQuery = ""
	u.Fragment = ""
	return fmt.Sprintf("%s://%s%s", scheme, u.Host, u.Path), nil
}

// NormalizePath cleans p, resolves `~`, converts relative paths to absolute,
// and optionally checks that the result stays inside mediaRoot (after symlink
// resolution when the target exists).
func NormalizePath(p, mediaRoot string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", apperr.New(apperr.InvalidCommand, "empty path")
	}
	if strings.ContainsRune(p, 0) {
		return "", apperr.New(apperr.InvalidCommand, "path contains NUL byte")
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			if p == "~" {
				p = home
			} else {
				p = filepath.Join(home, p[2:])
			}
		}
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", apperr.Wrap(apperr.InvalidCommand, err, "resolve path")
	}
	abs = filepath.Clean(abs)
	if mediaRoot != "" {
		rootAbs, err := filepath.Abs(mediaRoot)
		if err != nil {
			return "", apperr.Wrap(apperr.Forbidden, err, "invalid media root")
		}
		if realRoot, err := filepath.EvalSymlinks(rootAbs); err == nil {
			rootAbs = realRoot
		}
		check := abs
		if realTarget, err := filepath.EvalSymlinks(abs); err == nil {
			check = realTarget
		}
		rel, err := filepath.Rel(rootAbs, check)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", apperr.New(apperr.Forbidden, "path %s is outside configured media root", p)
		}
	}
	return abs, nil
}
