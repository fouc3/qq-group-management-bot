// Package httpapi serves the management console: the compiled front end and the
// read-only API behind it.
//
// It is a thin wrapper and nothing more. Every answer it gives comes from a
// function that already existed -- the store's own queries, the feature registry,
// the configuration in force, the command table a feature publishes -- and it
// holds no state of its own beyond the moment it started. That is deliberate: a
// second implementation of "what is a member's history" would be a second answer
// to a question the bot already answers, and the two would drift apart the first
// time one of them was changed.
//
// What it does own is the boundary: who may ask, what a request looks like, and
// what a failure says. Those are the things that have no home in the packages the
// bot is made of, because until now nobody was asking.
//
// Only reading is served. Nothing here silences, removes or records anything --
// the one exception is asking for the configuration to be read again, which is a
// reload of what the file already says rather than a change to it.
package httpapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/fouc3/qq-group-management-bot/internal/command"
	"github.com/fouc3/qq-group-management-bot/internal/config"
	"github.com/fouc3/qq-group-management-bot/internal/feature"
	"github.com/fouc3/qq-group-management-bot/internal/logring"
	"github.com/fouc3/qq-group-management-bot/internal/store"
)

// ConfigSource is the configuration in force.
//
// A source rather than a value because the file can change while the bot runs:
// a console built from the configuration read at startup would report settings
// that were edited away, which is worse than reporting none.
type ConfigSource interface {
	Current() *config.Config
}

// Features is what the console is allowed to ask about the running features.
//
// Three questions, because they are the three an operator has: is this running,
// what does it already know, and what do all of them offer together. The second is
// how a feature's own answers are read -- published through an interface the
// feature already implemented, rather than a list kept here that would go stale.
// The third is the command table, and it has to be plural: the table is kept by one
// feature while the commands themselves are offered by several, so asking the
// keeper alone answers nothing.
type Features interface {
	// Running reports whether a named feature is built and registered.
	Running(name string) bool
	// Instance returns a running feature, or nil.
	Instance(name string) feature.Feature
	// CommandDefs lists the commands the running features offer.
	CommandDefs() []command.Def
}

// Reloader re-reads the configuration file, reporting whether the whole change
// was adopted.
//
// An error is the point of the interface: a console that reported success
// whenever the file was touched would tell an operator their edit is in force
// when a parse failure had left the previous configuration running.
type Reloader interface {
	Reload(why string) error
}

// HumanVerify is the online verification service's health, as the console shows it.
//
// One question, and it is asked of the service in force rather than of the client
// the process started with: a reload replaces that client, so a console holding one
// would go on answering about a connection that has been closed.
type HumanVerify interface {
	// HumanVerifyConnected reports whether a result could reach this process now.
	HumanVerifyConnected() bool
}

// Options is what the service is built from.
type Options struct {
	// Admin is the section that decided to build this at all.
	Admin config.Admin
	// Config is the configuration in force.
	Config ConfigSource
	// ConfigPath is the file the console names and asks to be reloaded.
	ConfigPath string
	// Store is the data layer.
	Store store.Store
	// Features is what is running.
	Features Features
	// HumanVerify reports the online verification service's connection. Nil is a
	// console that cannot say: the answer to every question about it is "not
	// connected", which is what a build without one gets.
	HumanVerify HumanVerify
	// Reload re-reads the configuration file.
	Reload Reloader
	// Logs is the copy of the log the logger keeps. Nil answers as a buffer that
	// holds nothing, which is the truth for a build without one.
	Logs *logring.Ring
	// Web is the compiled front end. Nil serves the API alone, which is what a
	// test wants and what a deployment without a built front end gets.
	Web fs.FS
	// Logger records requests and failures.
	Logger *slog.Logger
	// Started is when the bot came up, for the uptime the console shows.
	Started time.Time
}

// Server answers the console's requests.
type Server struct {
	opts Options
	mux  *http.ServeMux
	// bound is the address the listener actually got.
	//
	// Recorded rather than reported from the configuration, because the two can
	// disagree: an edit to admin.listen cannot move a socket that is already
	// bound, and a console that showed the address it was told to use would
	// describe a place it is not.
	bound string
	// closing is closed when the bot is shutting down.
	//
	// A log stream is a request that deliberately does not end, and an HTTP
	// server's shutdown waits for the requests in flight: without this, stopping
	// the bot would sit in that wait until it timed out, and then report that the
	// console had not shut down cleanly.
	closing   chan struct{}
	closeOnce sync.Once
}

// New builds the service and its routes.
func New(opts Options) (*Server, error) {
	if opts.Store == nil {
		return nil, errors.New("httpapi: a store is required")
	}
	if opts.Config == nil {
		return nil, errors.New("httpapi: a configuration source is required")
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if opts.Started.IsZero() {
		opts.Started = time.Now()
	}
	server := &Server{opts: opts, mux: http.NewServeMux(), closing: make(chan struct{})}
	server.routes()
	return server, nil
}

// routes declares what is answered.
//
// The patterns carry their methods, so a request with the wrong one is refused by
// the mux rather than by each handler. Everything under /api/v1 is JSON and
// everything else is the front end.
func (s *Server) routes() {
	s.mux.HandleFunc("GET /api/v1/status", s.handleStatus)
	s.mux.HandleFunc("GET /api/v1/overview", s.handleOverview)
	s.mux.HandleFunc("GET /api/v1/groups", s.handleGroups)
	s.mux.HandleFunc("GET /api/v1/pending", s.handlePending)
	s.mux.HandleFunc("GET /api/v1/member-events", s.handleMemberEvents)
	s.mux.HandleFunc("GET /api/v1/judgements", s.handleJudgements)
	s.mux.HandleFunc("GET /api/v1/blacklist", s.handleBlacklist)
	s.mux.HandleFunc("GET /api/v1/high-risk", s.handleHighRisk)
	s.mux.HandleFunc("GET /api/v1/broadcasts", s.handleBroadcasts)
	s.mux.HandleFunc("GET /api/v1/commands", s.handleCommands)
	s.mux.HandleFunc("GET /api/v1/config", s.handleConfig)
	s.mux.HandleFunc("POST /api/v1/config/reload", s.handleReload)
	// The three things an operator does to somebody who is waiting to verify.
	// They are the console's first write operations: everything else it answers
	// still comes from a query, and the one write it used to have was asking for
	// the file to be read again.
	s.mux.HandleFunc("POST /api/v1/verifications/{token}/resend", s.handleVerificationAction("resend"))
	s.mux.HandleFunc("POST /api/v1/verifications/{token}/approve", s.handleVerificationAction("approve"))
	s.mux.HandleFunc("POST /api/v1/verifications/{token}/reject", s.handleVerificationAction("reject"))
	// The two lists an operator edits: who may not come in, and whose messages
	// are judged as they arrive.
	s.mux.HandleFunc("POST /api/v1/blacklist", s.handleBlacklistAdd)
	s.mux.HandleFunc("DELETE /api/v1/blacklist/{id}", s.handleBlacklistRemove)
	s.mux.HandleFunc("POST /api/v1/high-risk", s.handleWatchAdd)
	// The pair travels as query parameters: a mark that names no group has an
	// empty one, and an empty path segment cannot be told from a missing one.
	s.mux.HandleFunc("DELETE /api/v1/high-risk", s.handleWatchRemove)
	// The log: the newest lines as one answer, and the same lines as they
	// arrive. Two routes rather than one, because a page that has to redraw a
	// log every second is a page that makes the bot answer a question it has
	// already answered.
	s.mux.HandleFunc("GET /api/v1/logs", s.handleLogs)
	s.mux.HandleFunc("GET /api/v1/logs/stream", s.handleLogStream)
	// Anything else under the prefix is a typo, and answering it with the front
	// end would send a caller looking in the browser for a mistake made in a
	// script.
	s.mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "unknown_endpoint",
			"没有这个接口："+r.URL.Path)
	})
	s.mux.Handle("/", s.frontEnd())
}

// Handler returns what serves the routes, with the boundary in place.
func (s *Server) Handler() http.Handler {
	return s.securityHeaders(s.authorize(s.logRequests(s.mux)))
}

// Listen binds the address the configuration names.
//
// Binding happens here rather than inside Serve so that a port already in use
// stops the bot at startup, where somebody is watching, instead of being
// discovered by a goroutine whose failure has nowhere to go.
func (s *Server) Listen() (net.Listener, error) {
	listener, err := net.Listen("tcp", s.opts.Admin.AdminListen())
	if err != nil {
		return nil, fmt.Errorf("httpapi: listening on %s: %w", s.opts.Admin.AdminListen(), err)
	}
	s.bound = listener.Addr().String()
	return listener, nil
}

// admin is the console's own settings as they are now.
//
// Taken from the configuration in force rather than from the copy this was built
// with, so that rotating the token is an edit and a reload instead of a restart.
// The listener is the exception -- it is bound once -- which is why Listen keeps
// what it actually got.
func (s *Server) admin() config.Admin {
	if s.opts.Config != nil {
		return s.opts.Config.Current().Admin
	}
	return s.opts.Admin
}

// Serve answers on the listener until the context ends.
//
// The listener is closed when the context ends, because Shutdown waits for the
// requests in flight and a listener left open would accept new ones for as long
// as that took.
func (s *Server) Serve(ctx context.Context, listener net.Listener) error {
	server := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// A console request is a query or a file, never an upload. The bound is
		// here so that a caller with a body cannot make the bot read it all.
		MaxHeaderBytes: 1 << 16,
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-ctx.Done()
		// Long-lived answers are told to end before the server is asked to wait
		// for them: a log stream would otherwise hold shutdown open until it
		// timed out.
		s.closeOnce.Do(func() { close(s.closing) })
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			s.opts.Logger.Warn("the management console did not shut down cleanly", "error", err)
		}
	}()

	s.opts.Logger.Info("the management console is listening",
		"addr", listener.Addr().String(),
		"token_required", strings.TrimSpace(s.opts.Admin.Token) != "")

	err := server.Serve(listener)
	<-done
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// authorize refuses a request that does not carry the token.
//
// A configuration with no token is one that bound a loopback address: that is
// checked when the file is read, and it is refused there rather than here, so
// this can trust it. The comparison is constant time because the alternative --
// answering faster for a wrong token than for a right one -- is how a token is
// guessed a byte at a time.
func (s *Server) authorize(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		want := strings.TrimSpace(s.admin().Token)
		// A browser asks for the front end before it can have a token to send,
		// so the page itself is public and only what it asks for is not. The
		// page is a shell: everything in it comes from the API.
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		if want != "" {
			got := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
			if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
				writeError(w, http.StatusUnauthorized, "unauthorized",
					"缺少或错误的访问令牌")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// logRequests records what was asked for and how long it took.
//
// Only the API: a page load pulls a dozen files, and a log line each would bury
// the requests that carry an answer.
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		began := time.Now()
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(recorder, r)
		level := slog.LevelDebug
		switch {
		case recorder.status >= 500:
			level = slog.LevelError
		case recorder.status >= 400:
			level = slog.LevelWarn
		}
		s.opts.Logger.Log(r.Context(), level, "console request",
			"method", r.Method, "path", r.URL.Path,
			"status", recorder.status, "took", time.Since(began).Round(time.Millisecond))
	})
}

// securityHeaders states what the page may do.
//
// The console holds credentials in the browser and can reach an interface that
// silences members, so it is worth saying that the page loads nothing from
// elsewhere and may not be framed: the two ways a page like this is attacked are
// a script from somewhere else and a click hijacked from another tab.
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := w.Header()
		header.Set("X-Content-Type-Options", "nosniff")
		header.Set("Referrer-Policy", "no-referrer")
		header.Set("Content-Security-Policy",
			"default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; "+
				"img-src 'self' data: blob:; connect-src 'self'; object-src 'none'; "+
				"base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

// frontEnd serves the compiled application.
//
// It is served from the embedded copy, or from a directory when one is named --
// which is for working on the front end, where a rebuilt file is picked up by a
// refresh rather than by rebuilding the bot.
func (s *Server) frontEnd() http.Handler {
	source := s.opts.Web
	if dir := strings.TrimSpace(s.admin().WebDir); dir != "" {
		source = os.DirFS(dir)
	}
	if source == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			s.writeNotBuilt(w)
		})
	}
	files := http.FileServerFS(source)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := fs.Stat(source, "index.html"); err != nil {
			// A checkout without a built front end. Said in words rather than
			// as a 404, because the file that would have answered is the one
			// that is missing and its absence has exactly one cause.
			s.writeNotBuilt(w)
			return
		}
		// A path that names a file is that file; anything else is a client-side
		// route, and the shell is what answers it.
		path := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if path != "" {
			if _, err := fs.Stat(source, path); err == nil {
				// A hashed asset never changes under its own name, so it may be
				// kept; the shell may not, or the next build would not be picked
				// up until the browser was told to forget everything.
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				files.ServeHTTP(w, r)
				return
			}
		}
		w.Header().Set("Cache-Control", "no-store")
		http.ServeFileFS(w, r, source, "index.html")
	})
}

// writeNotBuilt says that the front end has not been compiled.
func (s *Server) writeNotBuilt(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusServiceUnavailable)
	fmt.Fprint(w, "前端尚未构建。\n\n"+
		"在 web/ 下运行：\n\n"+
		"    pnpm install && pnpm build\n\n"+
		"然后重启机器人（或设置 admin.web_dir 指向已构建的目录，刷新即可生效）。\n"+
		"接口本身不受影响，可以直接访问 /api/v1/status。\n")
}

// statusRecorder remembers what was answered.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

// WriteHeader records the status before it is written.
func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

// Flush passes the flush through to the writer underneath.
//
// The log stream is written one line at a time and has to reach the browser as it
// is written; without this method the handler's Flush would find a wrapper that
// does not implement it, and the stream would arrive in one lump when the request
// ended -- which is never.
func (r *statusRecorder) Flush() {
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// Unwrap names the writer underneath, which is how a handler can still find
// anything else it needs from it.
func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

// writeJSON answers with one value.
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// Read-only answers, and a cached one would show yesterday's pending queue.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		// The status is already on the wire, so there is nothing left to say to
		// the caller. It is worth a line in the log: a half-written answer is
		// how a client sees a JSON syntax error instead of a server error.
		slog.Default().Warn("writing a console response failed", "error", err)
	}
}

// errorBody is what a failure says.
type errorBody struct {
	Error errorDetail `json:"error"`
}

// errorDetail is one failure.
type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// writeError answers with a failure.
//
// The message is written for the person reading the console rather than for a
// programmer reading a log, because that is who sees it: it is shown as it is.
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorBody{Error: errorDetail{Code: code, Message: message}})
}

// fail answers with the failure a handler ran into.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	s.opts.Logger.Error("a console request failed",
		"path", r.URL.Path, "error", err)
	writeError(w, http.StatusInternalServerError, "internal_error",
		"读取数据失败，详情见机器人日志")
}

// now is the clock, kept in one place so a test can stand still.
func (s *Server) now() time.Time { return time.Now() }
