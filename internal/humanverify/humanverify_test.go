package humanverify

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// The service speaks two protocols to this bot and this file stands in for both:
// an HTTP endpoint that issues a link, and one long connection that carries the
// results. What is checked here is what the service's own document says to
// check -- the marker, the address the challenge was solved on, and the heartbeat
// that keeps the connection from being reclaimed.

// issue is one call to the issue endpoint, as it arrived.
type issue struct {
	authorization string
	body          map[string]any
}

// service is the verification service, standing in.
type service struct {
	t      *testing.T
	server *httptest.Server

	mu sync.Mutex
	// issues is what arrived at the issue endpoint.
	issues []issue
	// answer overrides the issue endpoint's reply, for the tests about what a
	// refusal looks like.
	answer func(w http.ResponseWriter)
	// sockets are the connections this service accepted, oldest first.
	sockets []*websocket.Conn
	// pings counts the heartbeats that arrived.
	pings int

	// writes serialises writes to a socket: the heartbeat answer and whatever a
	// test sends come from different goroutines, and a websocket takes one
	// writer at a time.
	writes sync.Mutex
}

func newService(t *testing.T) *service {
	t.Helper()
	standing := &service{t: t}
	standing.server = httptest.NewServer(http.HandlerFunc(standing.serve))
	t.Cleanup(standing.server.Close)
	return standing
}

func (s *service) serve(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/api/issue":
		var body map[string]any
		_ = decodeJSON(r, &body)
		s.mu.Lock()
		s.issues = append(s.issues, issue{
			authorization: r.Header.Get("Authorization"),
			body:          body,
		})
		answer := s.answer
		s.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if answer != nil {
			answer(w)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"id":"LINK-1",` +
			`"url":"https://verify.test/?t=abc","exp":1791971431,"ttl":900}`))

	case "/ws":
		connection, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			s.t.Logf("upgrading: %v", err)
			return
		}
		s.mu.Lock()
		s.sockets = append(s.sockets, connection)
		s.mu.Unlock()

		s.write(connection, `{"type":"hello"}`)
		for {
			_, payload, err := connection.ReadMessage()
			if err != nil {
				// The client dropped the connection, or a test closed it.
				return
			}
			if string(payload) == "ping" {
				s.mu.Lock()
				s.pings++
				s.mu.Unlock()
				s.write(connection, "pong")
			}
		}

	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// write sends one frame to one connection.
func (s *service) write(connection *websocket.Conn, payload string) {
	s.writes.Lock()
	defer s.writes.Unlock()
	_ = connection.WriteMessage(websocket.TextMessage, []byte(payload))
}

// tell sends one frame to every connection it has accepted.
func (s *service) tell(payload string) {
	s.mu.Lock()
	sockets := append([]*websocket.Conn(nil), s.sockets...)
	s.mu.Unlock()
	for _, connection := range sockets {
		s.write(connection, payload)
	}
}

// drop closes every connection, which is what a network that reclaims an idle
// one looks like from here.
func (s *service) drop() {
	s.mu.Lock()
	sockets := append([]*websocket.Conn(nil), s.sockets...)
	s.mu.Unlock()
	for _, connection := range sockets {
		_ = connection.Close()
	}
}

// connections reports how many connections were accepted.
func (s *service) connections() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sockets)
}

// heartbeats reports how many heartbeats arrived.
func (s *service) heartbeats() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pings
}

// issued returns the calls that reached the issue endpoint.
func (s *service) issued() []issue {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]issue(nil), s.issues...)
}

// decodeJSON reads a request body.
func decodeJSON(r *http.Request, into any) error {
	payload, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	return json.Unmarshal(payload, into)
}

// newClient builds a client against one service, with a heartbeat short enough
// for a test to see.
func newClient(t *testing.T, standing *service, overrides ...func(*Config)) *Client {
	t.Helper()
	cfg := Config{
		BaseURL:          standing.server.URL,
		IssueToken:       "test-issue-token",
		WSToken:          "test-ws-token",
		HeartbeatSeconds: 1,
	}
	for _, override := range overrides {
		override(&cfg)
	}
	client, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("building the client: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// waitFor polls until what is asked for is true.
func waitFor(t *testing.T, what string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestIssueAsksForALinkAndReadsItBack covers the request the service documents,
// down to the business marker: the marker is what groups the statistics and what
// Cloudflare echoes, so a bot that did not send one would not be told apart from
// anything else using the same service.
func TestIssueAsksForALinkAndReadsItBack(t *testing.T) {
	standing := newService(t)
	client := newClient(t, standing)

	link, err := client.Issue(context.Background(), Request{
		Action: "join_group",
		Target: "token-abc",
	})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if link.URL == "" || link.ID != "LINK-1" || link.TTLSeconds != 900 {
		t.Errorf("link = %+v, want what the service answered", link)
	}

	calls := standing.issued()
	if len(calls) != 1 {
		t.Fatalf("issue calls = %d, want one", len(calls))
	}
	if calls[0].authorization != "Bearer test-issue-token" {
		t.Errorf("authorization = %q, want the configured issue token",
			calls[0].authorization)
	}
	if got := calls[0].body["cdata"]; got != BusinessMarker {
		t.Errorf("cdata = %v, want the business marker %q", got, BusinessMarker)
	}
	if got := calls[0].body["action"]; got != "join_group" {
		t.Errorf("action = %v, want the action it was asked for", got)
	}
	if got := calls[0].body["target"]; got != "token-abc" {
		t.Errorf("target = %v, want the target it was asked for", got)
	}
	if got := calls[0].body["ttl"]; got != float64(DefaultTTLSeconds) {
		t.Errorf("ttl = %v, want the default %d", got, DefaultTTLSeconds)
	}
}

// TestTheBusinessMarkerCanBeOverridden covers the one thing about the marker a
// deployment may want to change: two bots sharing one service are two businesses
// and should not be one line in its statistics.
func TestTheBusinessMarkerCanBeOverridden(t *testing.T) {
	standing := newService(t)
	client := newClient(t, standing, func(cfg *Config) {
		cfg.Cdata = "another-bot"
	})

	if _, err := client.Issue(context.Background(), Request{Target: "token-abc"}); err != nil {
		t.Fatalf("Issue: %v", err)
	}
	calls := standing.issued()
	if len(calls) != 1 {
		t.Fatalf("issue calls = %d, want one", len(calls))
	}
	if got := calls[0].body["cdata"]; got != "another-bot" {
		t.Errorf("cdata = %v, want the configured marker", got)
	}
}

// TestIssueReportsWhatTheServiceRefused covers the answer an operator can act on:
// the service says why it refused, and a status code alone would leave them
// guessing between a wrong token and a lifetime outside its bounds.
func TestIssueReportsWhatTheServiceRefused(t *testing.T) {
	standing := newService(t)
	standing.answer = func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"ok":false,"error":"ttl 需在 60..86400 秒之间"}`))
	}
	client := newClient(t, standing)

	_, err := client.Issue(context.Background(), Request{Target: "token-abc"})
	if err == nil {
		t.Fatal("a refused issue was reported as a link")
	}
	if !strings.Contains(err.Error(), "60..86400") {
		t.Errorf("err = %v, want the service's own words", err)
	}
}

// TestIssueRefusesALinkWithNoURL keeps the failure where it happens: a link
// nobody can open is not something to hand to a member and find out about later.
func TestIssueRefusesALinkWithNoURL(t *testing.T) {
	standing := newService(t)
	standing.answer = func(w http.ResponseWriter) {
		_, _ = w.Write([]byte(`{"ok":true,"id":"LINK-1"}`))
	}
	client := newClient(t, standing)

	if _, err := client.Issue(context.Background(), Request{Target: "t"}); err == nil {
		t.Error("a link with no url was accepted")
	}
}

// result is one verified event as the service sends it: the marker the link was
// signed with, the marker Cloudflare echoed back, and the site the challenge was
// solved on. The two markers are separate parameters because the two coming
// apart is exactly what the client has to notice.
func result(marker string, echoed string, hostname string) string {
	payload := `{"type":"verified","at":1791885116216,"action":"join_group",` +
		`"target":"token-abc","id":"LINK-1","cdata":"` + marker + `",` +
		`"client":{"ip":"203.0.113.7","ip_country":"CN",` +
		`"as_organization":"Chinanet"},` +
		`"turnstile":{"success":true,"hostname":"` + hostname + `","cdata":"` + echoed + `"}}`
	return payload
}

// TestAResultReachesTheSubscriber covers the whole point of the connection: the
// member verified, and the bot is told about it.
func TestAResultReachesTheSubscriber(t *testing.T) {
	standing := newService(t)
	client := newClient(t, standing)
	waitFor(t, "the connection", func() bool { return client.Connected() })

	results := make(chan Event, 4)
	client.OnVerified(func(event Event) { results <- event })

	host := strings.TrimPrefix(standing.server.URL, "http://")
	standing.tell(result(BusinessMarker, BusinessMarker, host))

	select {
	case event := <-results:
		if event.Target != "token-abc" {
			t.Errorf("target = %q, want the one the link was issued for", event.Target)
		}
		if event.Client.IP != "203.0.113.7" {
			t.Errorf("client ip = %q, want what the service saw", event.Client.IP)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a verified result never reached the subscriber")
	}
}

// TestAResultForSomethingElseIsDiscarded covers the two checks the service's
// document asks a client to make. Either failing means the token was not solved
// for one of this bot's own links, and acting on it would let somebody through
// on somebody else's verification.
func TestAResultForSomethingElseIsDiscarded(t *testing.T) {
	standing := newService(t)
	client := newClient(t, standing)
	waitFor(t, "the connection", func() bool { return client.Connected() })

	results := make(chan Event, 4)
	client.OnVerified(func(event Event) { results <- event })

	host := strings.TrimPrefix(standing.server.URL, "http://")
	// A marker that is another business's, then a marker Cloudflare did not echo
	// back, then a challenge solved on somebody else's site, and finally the
	// genuine article: only the last one may arrive.
	standing.tell(result("another-business", "another-business", host))
	standing.tell(result(BusinessMarker, "something-else", host))
	standing.tell(result(BusinessMarker, BusinessMarker, "human-verification.example"))
	standing.tell(result(BusinessMarker, BusinessMarker, host))

	select {
	case event := <-results:
		if event.Turnstile.Hostname != host {
			t.Errorf("the result that arrived was solved on %q, not on this service",
				event.Turnstile.Hostname)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no result arrived")
	}
	select {
	case extra := <-results:
		t.Errorf("a second result arrived: %+v", extra)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestTheConnectionSaysSomethingSoItIsNotReclaimed covers the one rule the
// service insists on: an idle connection is quietly reclaimed by whatever is in
// the middle of the network, and a result pushed at it is then lost with nothing
// said on either side.
func TestTheConnectionSaysSomethingSoItIsNotReclaimed(t *testing.T) {
	standing := newService(t)
	newClient(t, standing)
	waitFor(t, "a heartbeat", func() bool { return standing.heartbeats() > 0 })
}

// TestTheConnectionIsMadeAgainAfterItEnds covers what the service promises and
// still asks for: a release does not drop connections, but the network does, so
// the client has to come back on its own.
func TestTheConnectionIsMadeAgainAfterItEnds(t *testing.T) {
	standing := newService(t)
	client := newClient(t, standing)
	waitFor(t, "the connection", func() bool { return client.Connected() })

	standing.drop()
	waitFor(t, "the connection to be made again", func() bool {
		return standing.connections() >= 2 && client.Connected()
	})

	// And results reach the new one, which is the only thing the reconnection is
	// for.
	results := make(chan Event, 1)
	client.OnVerified(func(event Event) { results <- event })
	going := strings.Split(standing.server.URL, "//")[1]
	standing.tell(result(BusinessMarker, BusinessMarker, going))
	select {
	case <-results:
	case <-time.After(5 * time.Second):
		t.Fatal("a result never reached the connection that replaced the lost one")
	}
}

// TestAStoppedClientIsNotConnected covers the answer Connected gives after the
// client was stopped: a link asked for then would be a link whose result nobody
// can receive.
func TestAStoppedClientIsNotConnected(t *testing.T) {
	standing := newService(t)
	client := newClient(t, standing)
	waitFor(t, "the connection", func() bool { return client.Connected() })

	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if client.Connected() {
		t.Error("a client that was closed still reports a connection")
	}
}
