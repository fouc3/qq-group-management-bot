// Package humanverify speaks to the online human verification service: it asks
// the service for a link, and it listens on one long connection for the results
// of the links it asked for.
//
// The two halves are one client because the second is what makes the first
// usable. The service delivers a result once, to whatever is connected at that
// moment: a link issued while nothing is listening is a person who passes the
// check, sees 验证成功, and is never let out of the hold -- the link is used up
// and asking again is the only way to get another one.
//
// The connection is therefore kept for as long as the process runs rather than
// for as long as one feature is built. A configuration change rebuilds features,
// and a feature's connection would be down for the length of every rebuild,
// which is exactly the window a result must not land in.
package humanverify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// BusinessMarker is which business asked for a link, and it is what every link
// is signed with unless the configuration says otherwise.
//
// It is a constant because a value that only ever has one right answer is not
// something to make an operator write down -- and because Turnstile silently
// drops it if it carries anything outside letters, digits, underscores and
// hyphens, which is a rule the service refuses the whole request over. The
// configuration may still override it: a deployment that shares one service
// with another bot needs its own name in the statistics, and one that wants
// every link told apart could put a per-link marker here if the service ever
// offers one.
const BusinessMarker = "qq-bot-human-verification"

// Defaults and bounds.
const (
	// DefaultTTLSeconds is how long a link stays usable when the file says
	// nothing, which is the service's own default.
	DefaultTTLSeconds = 900
	// MinTTLSeconds and MaxTTLSeconds are the service's bounds: it refuses a
	// link outside them rather than clipping it.
	MinTTLSeconds = 60
	MaxTTLSeconds = 86400
	// DefaultTimeoutSeconds bounds one call to the service.
	DefaultTimeoutSeconds = 10

	// heartbeatInterval is how often a literal "ping" is sent by default. The
	// service's connection lives on a hibernating object that only wakes for
	// messages, and the documented interval is about half a minute: without a
	// heartbeat, a home router or a mobile carrier quietly drops the connection
	// and the next result is lost with nothing said on either side.
	heartbeatInterval = 25 * time.Second
	// initialBackoff and maxBackoff bound the wait between connection attempts.
	// Every attempt is a request to somebody else's server, so a service that is
	// down is not hammered once a second.
	initialBackoff = time.Second
	maxBackoff     = 30 * time.Second

	// EventVerified is the kind of message that carries a result.
	EventVerified = "verified"

	// closeTimeout bounds how long Close waits for the connection to stop.
	closeTimeout = 5 * time.Second
)

// Config is where the service lives and how it is reached.
type Config struct {
	// BaseURL is the service's address, http(s)://host[:port], without a path.
	// The result connection is derived from it.
	BaseURL string
	// IssueToken authorises asking for a link. Server-side secret.
	IssueToken string
	// WSToken authorises the connection results arrive on. Server-side secret.
	WSToken string
	// Cdata is the business marker every link is signed with. Empty takes
	// BusinessMarker.
	Cdata string
	// TTLSeconds is how long one link stays usable. Zero takes the default.
	TTLSeconds int
	// TimeoutSeconds bounds one call to the service. Zero takes the default.
	TimeoutSeconds int
	// HeartbeatSeconds is how often the connection says something. Zero takes
	// the default, which is what the service documents.
	HeartbeatSeconds int
}

// Request is one link to ask for.
type Request struct {
	// Action names what the link is for. The service passes it back untouched,
	// so it is the caller's own word for the kind of verification.
	Action string
	// Target is what the link is about, at most 128 characters, and it is what
	// comes back in the result. The caller puts its own name for the thing being
	// verified here, because the result carries nothing else it chose.
	Target string
	// TTLSeconds overrides the configured lifetime for this one link. Zero takes
	// the configured value.
	TTLSeconds int
}

// Link is one issued link.
type Link struct {
	// ID identifies the link, and it is the service's key for the one-time use
	// record rather than anything this bot needs.
	ID string
	// URL is what the person opens.
	URL string
	// ExpiresAt is when the link stops working, as a Unix timestamp.
	ExpiresAt int64
	// TTLSeconds is the lifetime the service actually gave it.
	TTLSeconds int
}

// Event is one result the service reports.
//
// The fields this bot has a use for are read and the rest of the payload is
// ignored on purpose: a service that adds a field should not be able to break
// the code that reads it.
type Event struct {
	// Type is the kind of message. Only EventVerified reaches a handler; the
	// others are the service's own bookkeeping.
	Type string `json:"type"`
	// At is when the service decided, in milliseconds.
	At int64 `json:"at"`
	// Action is what the link was asked for with.
	Action string `json:"action"`
	// Target is what the link was asked about: the caller's own name for the
	// thing being verified.
	Target string `json:"target"`
	// ID is the service's id for the link.
	ID string `json:"id"`
	// Cdata is the business marker the link was signed with.
	Cdata string `json:"cdata"`
	// Client is what the service saw of the browser that verified.
	Client ClientInfo `json:"client"`
	// Turnstile is Cloudflare's own answer, which is what tells a genuine result
	// from one that was reported about something else.
	Turnstile Turnstile `json:"turnstile"`
}

// ClientInfo is what the service observed about the browser that verified.
//
// Only what the bot writes into a log line is read: the address and the network
// it came from are what make a surprising verification answerable ("that was
// the member's own phone" against "that was a data centre"). The rest of what
// the service collects is deliberately not kept here.
type ClientInfo struct {
	// IP is the browser's own egress address, which is what the service saw.
	IP string `json:"ip"`
	// Country is where that address is registered.
	Country string `json:"ip_country"`
	// Organization is who announces it.
	Organization string `json:"as_organization"`
}

// Turnstile is Cloudflare's answer to the challenge.
type Turnstile struct {
	// Success reports whether the challenge was passed at all.
	Success bool `json:"success"`
	// Hostname is the site the challenge was solved on. It has to be this
	// service, or the token was solved somewhere else.
	Hostname string `json:"hostname"`
	// Cdata is the business marker Cloudflare echoed back. It has to be the one
	// this service signed into the link, or the token was not solved for it.
	Cdata string `json:"cdata"`
}

// Client is one connection to the service, and the way to ask it for links.
type Client struct {
	base       *url.URL
	issueToken string
	wsToken    string
	cdata      string
	ttl        int
	timeout    time.Duration
	// keepalive is how often the connection says something, and silence is how
	// long it may say nothing before it is treated as dead. Silence is a few
	// heartbeats rather than a number of its own: what it has to miss is
	// heartbeats.
	keepalive time.Duration
	silence   time.Duration
	http      *http.Client
	log       *slog.Logger

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	mu       sync.Mutex
	up       bool
	lastSeen time.Time
	handlers []func(Event)

	closeOnce sync.Once
}

// New opens the client and starts keeping the connection up.
//
// The connection is started here rather than on the first link: a result that
// arrives before anybody asked for a link concerns nobody, but a connection
// that is only opened when a link is asked for is one that is still handshaking
// when the person is already done.
func New(cfg Config, logger *slog.Logger) (*Client, error) {
	base, err := url.Parse(strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/"))
	if err != nil {
		return nil, fmt.Errorf("humanverify: base url %q: %w", cfg.BaseURL, err)
	}
	switch base.Scheme {
	case "http", "https":
	default:
		return nil, fmt.Errorf("humanverify: base url %q is not http or https", cfg.BaseURL)
	}
	if base.Host == "" {
		return nil, fmt.Errorf("humanverify: base url %q names no host", cfg.BaseURL)
	}

	ttl := cfg.TTLSeconds
	if ttl <= 0 {
		ttl = DefaultTTLSeconds
	}
	timeout := time.Duration(cfg.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = DefaultTimeoutSeconds * time.Second
	}
	heartbeat := time.Duration(cfg.HeartbeatSeconds) * time.Second
	if heartbeat <= 0 {
		heartbeat = heartbeatInterval
	}
	cdata := strings.TrimSpace(cfg.Cdata)
	if cdata == "" {
		cdata = BusinessMarker
	}

	ctx, cancel := context.WithCancel(context.Background())
	client := &Client{
		base:       base,
		issueToken: cfg.IssueToken,
		wsToken:    cfg.WSToken,
		cdata:      cdata,
		ttl:        ttl,
		timeout:    timeout,
		keepalive:  heartbeat,
		silence:    3 * heartbeat,
		http:       &http.Client{Timeout: timeout},
		log:        logger,
		ctx:        ctx,
		cancel:     cancel,
		done:       make(chan struct{}),
	}
	go client.run()
	return client, nil
}

// Issue asks for one link.
//
// One link is for one person and one use: the service refuses a second
// submission of the same link, so a link that is sent to the wrong member, or
// re-used after it was verified, is simply a link nobody can verify with.
func (c *Client) Issue(ctx context.Context, request Request) (Link, error) {
	ttl := request.TTLSeconds
	if ttl <= 0 {
		ttl = c.ttl
	}
	body, err := json.Marshal(struct {
		Action string `json:"action,omitempty"`
		Target string `json:"target,omitempty"`
		Cdata  string `json:"cdata"`
		TTL    int    `json:"ttl"`
	}{
		Action: request.Action,
		Target: request.Target,
		Cdata:  c.cdata,
		TTL:    ttl,
	})
	if err != nil {
		return Link{}, fmt.Errorf("humanverify: encoding the request: %w", err)
	}

	endpoint := c.base.String() + "/api/issue"
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint,
		bytes.NewReader(body))
	if err != nil {
		return Link{}, fmt.Errorf("humanverify: building the request: %w", err)
	}
	httpRequest.Header.Set("Authorization", "Bearer "+c.issueToken)
	httpRequest.Header.Set("Content-Type", "application/json")

	response, err := c.http.Do(httpRequest)
	if err != nil {
		return Link{}, fmt.Errorf("humanverify: asking for a link: %w", err)
	}
	defer response.Body.Close()

	// Read into memory with a ceiling: an HTTP response is somebody else's
	// bytes, and how much memory one may ask this process for is not its
	// decision. Four kilobytes is far more than this service's answers take.
	payload, err := io.ReadAll(io.LimitReader(response.Body, 4<<10))
	if err != nil {
		return Link{}, fmt.Errorf("humanverify: reading the answer: %w", err)
	}

	var answer struct {
		OK    bool   `json:"ok"`
		ID    string `json:"id"`
		URL   string `json:"url"`
		Exp   int64  `json:"exp"`
		TTL   int    `json:"ttl"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(payload, &answer); err != nil {
		return Link{}, fmt.Errorf("humanverify: the service answered %s with %s",
			response.Status, excerpt(payload))
	}
	if !answer.OK {
		// The service's own words are worth more than a status code: "人机验证
		// 未通过" and "ttl 需在 60..86400 秒之间" are the two an operator sees.
		if answer.Error != "" {
			return Link{}, fmt.Errorf("humanverify: the service refused: %s (%s)",
				answer.Error, response.Status)
		}
		return Link{}, fmt.Errorf("humanverify: the service answered %s with %s",
			response.Status, excerpt(payload))
	}
	if strings.TrimSpace(answer.URL) == "" {
		return Link{}, errors.New("humanverify: the service issued a link with no url")
	}
	return Link{
		ID:         answer.ID,
		URL:        answer.URL,
		ExpiresAt:  answer.Exp,
		TTLSeconds: answer.TTL,
	}, nil
}

// OnVerified subscribes to the results, and returns how to stop listening.
//
// One callback per subscriber, called from the connection's own goroutine:
// handlers are called one after another rather than together, so a slow one
// holds the others up, and a handler that talks to the platform does so with the
// context its caller chose rather than with the connection's.
func (c *Client) OnVerified(handler func(Event)) (cancel func()) {
	c.mu.Lock()
	c.handlers = append(c.handlers, handler)
	index := len(c.handlers) - 1
	c.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			if index < len(c.handlers) {
				// Nil rather than removed: the index of every other subscriber
				// stays what it was, which is what makes cancelling twice, or
				// cancelling out of order, harmless.
				c.handlers[index] = nil
			}
		})
	}
}

// Connected reports whether a result could reach this process right now.
//
// It is what decides whether a link is worth asking for at all. The service
// reports a result once, on whatever connection is up at that moment, and a link
// asked for while this process is not connected is a person who passes the check
// and is never released.
func (c *Client) Connected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.up && time.Since(c.lastSeen) < c.silence
}

// Close ends the connection and the attempts to keep it up.
func (c *Client) Close() error {
	c.closeOnce.Do(c.cancel)
	select {
	case <-c.done:
		return nil
	case <-time.After(closeTimeout):
		return errors.New("humanverify: the connection did not stop in time")
	}
}

// run keeps one connection up for as long as the client lives.
func (c *Client) run() {
	defer func() {
		c.setDown()
		close(c.done)
	}()
	backoff := initialBackoff
	for {
		if c.ctx.Err() != nil {
			return
		}
		started := time.Now()
		err := c.session()
		if c.ctx.Err() != nil {
			return
		}
		c.setDown()
		// A connection that was up for a while is not evidence that the next
		// attempt will fail, so a service that is up again is not made to wait
		// out the backoff the last outage earned.
		if time.Since(started) > time.Minute {
			backoff = initialBackoff
		}
		c.log.Warn("the verification connection ended, reconnecting",
			"host", c.base.Host, "error", err, "in", backoff)
		select {
		case <-c.ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// session holds one connection until it ends.
func (c *Client) session() error {
	connection, response, err := websocket.DefaultDialer.DialContext(c.ctx,
		c.wsURL(), nil)
	if err != nil {
		if response != nil {
			_ = response.Body.Close()
		}
		return err
	}
	defer connection.Close()

	c.setUp()
	c.log.Info("connected to the verification service",
		"host", c.base.Host, "heartbeat", c.keepalive)

	sessionCtx, endSession := context.WithCancel(c.ctx)
	defer endSession()
	// A read does not notice the context it was dialled with: the only thing
	// that ends it is the connection going away. Closing it here is therefore
	// what makes stopping prompt -- without it, Close waits out its whole
	// timeout on a socket nobody is sending anything on.
	go func() {
		<-sessionCtx.Done()
		_ = connection.Close()
	}()
	go c.heartbeat(sessionCtx, connection)
	go c.watchdog(sessionCtx, connection)

	for {
		_, payload, err := connection.ReadMessage()
		if err != nil {
			return err
		}
		c.saw()
		c.dispatch(payload)
	}
}

// heartbeat keeps the connection awake.
//
// The service's own words: the connection lives on an object that hibernates,
// and the heartbeat is what stops the middle of the network from quietly
// reclaiming an idle connection. It is also what tells this process that the
// connection is still carrying something, which is what Connected reports.
func (c *Client) heartbeat(ctx context.Context, connection *websocket.Conn) {
	ticker := time.NewTicker(c.keepalive)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = connection.SetWriteDeadline(time.Now().Add(c.timeout))
			if err := connection.WriteMessage(websocket.TextMessage, []byte("ping")); err != nil {
				// Closing is what ends the session: the read that is blocked on
				// the same connection returns, and the loop reconnects.
				_ = connection.Close()
				return
			}
		}
	}
}

// watchdog drops a connection that has gone quiet.
func (c *Client) watchdog(ctx context.Context, connection *websocket.Conn) {
	ticker := time.NewTicker(c.keepalive)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.mu.Lock()
			silent := time.Since(c.lastSeen)
			c.mu.Unlock()
			if silent > c.silence {
				c.log.Warn("the verification connection has said nothing for too "+
					"long, so it is being replaced",
					"host", c.base.Host, "silent_for", silent.Round(time.Second))
				_ = connection.Close()
				return
			}
		}
	}
}

// dispatch reads one message and passes a result on.
func (c *Client) dispatch(payload []byte) {
	if string(payload) == "pong" {
		return
	}
	var envelope struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		// Not this client's business: the service answers its own probes, and a
		// connection that only ever carries what it should not is not worth
		// dropping over a frame nobody asked for.
		c.log.Debug("the verification service sent something that is not json",
			"bytes", len(payload))
		return
	}
	switch envelope.Type {
	case "hello", "pong":
		// The handshake and the heartbeat's answer, which the read itself has
		// already counted as proof of life.
		return
	case EventVerified:
	default:
		c.log.Debug("the verification service sent an unknown message",
			"type", envelope.Type)
		return
	}

	var event Event
	if err := json.Unmarshal(payload, &event); err != nil {
		c.log.Warn("a verification result could not be read", "error", err)
		return
	}
	if err := c.genuine(event); err != nil {
		c.log.Warn("a verification result was discarded", "error", err,
			"target", event.Target, "link", event.ID)
		return
	}

	c.mu.Lock()
	handlers := make([]func(Event), len(c.handlers))
	copy(handlers, c.handlers)
	c.mu.Unlock()
	for _, handler := range handlers {
		if handler != nil {
			handler(event)
		}
	}
}

// genuine reports whether a result was really produced for a link this service
// issued for this bot.
//
// The service's own advice, and the reason the result carries Cloudflare's
// answer at all. The marker the link was signed with has to be this bot's own,
// Cloudflare has to have echoed that same marker back, and the challenge has to
// have been solved on this service's own site. Any of them failing means the
// token was not solved for one of this bot's links, and acting on it would let
// somebody through on somebody else's verification.
func (c *Client) genuine(event Event) error {
	if !event.Turnstile.Success {
		return errors.New("the challenge did not pass")
	}
	if event.Cdata != c.cdata {
		return fmt.Errorf("the result is marked %q, and this bot signs %q",
			event.Cdata, c.cdata)
	}
	if event.Turnstile.Cdata != event.Cdata {
		return fmt.Errorf("Cloudflare echoed %q for a link signed %q",
			event.Turnstile.Cdata, event.Cdata)
	}
	if host := event.Turnstile.Hostname; host != "" && !strings.EqualFold(host, c.base.Host) {
		return fmt.Errorf("the challenge was solved on %q, not on %q",
			host, c.base.Host)
	}
	return nil
}

// wsURL is the address results arrive on, with the token that opens it.
func (c *Client) wsURL() string {
	scheme := "wss"
	if c.base.Scheme == "http" {
		scheme = "ws"
	}
	query := url.Values{"token": {c.wsToken}}.Encode()
	return scheme + "://" + c.base.Host + strings.TrimRight(c.base.Path, "/") +
		"/ws?" + query
}

// setUp, setDown and saw are the connection's state, kept in one place because
// three goroutines touch it.
func (c *Client) setUp() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.up = true
	c.lastSeen = time.Now()
}

func (c *Client) setDown() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.up = false
}

func (c *Client) saw() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastSeen = time.Now()
}

// excerpt renders a response body for an error message, bounded.
func excerpt(payload []byte) string {
	text := strings.TrimSpace(string(payload))
	if len(text) > 200 {
		text = text[:200] + "…"
	}
	if text == "" {
		return "(no body)"
	}
	return text
}
