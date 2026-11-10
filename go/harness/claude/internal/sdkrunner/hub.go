package sdkrunner

import (
	"context"
	"encoding/json"
	"maps"
	"sync"
	"time"
)

// clientBuffer bounds the frames queued for one client. A client that falls
// this far behind is disconnected and reattaches with since=<seq>.
const clientBuffer = 1024

type client struct {
	send chan []byte
	// gone is closed when the session drops a slow client.
	gone     chan struct{}
	goneOnce sync.Once
}

func newClient() *client {
	return &client{send: make(chan []byte, clientBuffer), gone: make(chan struct{})}
}

func (c *client) drop() { c.goneOnce.Do(func() { close(c.gone) }) }

// emitLocked assigns the next sequence number, keeps the event in the backlog
// and fans it out to attached clients.
func (s *Session) emitLocked(kind string, fields map[string]any) {
	s.emitStreamLocked(kind, fields, false)
}

func (s *Session) emitStreamLocked(kind string, fields map[string]any, stream bool) {
	s.seq++
	event := make(map[string]any, len(fields)+3)
	maps.Copy(event, fields)
	event["k"], event["seq"], event["ts"] = kind, s.seq, time.Now().UnixMilli()
	data, err := json.Marshal(event)
	if err != nil {
		s.cfg.Logger.Error("encode sdk runner event", "kind", kind, "error", err)
		return
	}
	s.backlog = append(s.backlog, entry{seq: s.seq, stream: stream, data: data})
	if over := len(s.backlog) - s.cfg.Backlog; over > 0 {
		s.backlog = append([]entry(nil), s.backlog[over:]...)
	}
	for c := range s.clients {
		select {
		case c.send <- data:
		default:
			delete(s.clients, c)
			c.drop()
		}
	}
}

// attach registers a client and returns its hello frame, built under the same
// lock as fan-out so no event falls between the backlog and the live stream.
func (s *Session) attach(c *client, since int64) ([]byte, error) {
	s.mu.Lock()
	backlog := make([]json.RawMessage, 0, len(s.backlog))
	for _, e := range s.backlog {
		if e.seq > since {
			backlog = append(backlog, e.data)
		}
	}
	perms := make([]*Perm, 0, len(s.permOrder))
	for _, id := range s.permOrder {
		perms = append(perms, s.perms[id])
	}
	hello, err := json.Marshal(map[string]any{
		"k": "hello", "v": 1, "seq": s.seq, "ts": time.Now().UnixMilli(), "now": time.Now().UnixMilli(),
		"meta": s.meta, "backlog": backlog, "perms": perms, "runner": s.state,
	})
	if err == nil {
		s.clients[c] = struct{}{}
	}
	s.mu.Unlock()
	if err == nil {
		s.updateActivity()
	}
	return hello, err
}

func (s *Session) detach(c *client) {
	s.mu.Lock()
	delete(s.clients, c)
	s.mu.Unlock()
	c.drop()
	s.updateActivity()
}

// ItemKind classifies events delivered to the A2A turn.
type ItemKind int

const (
	ItemSDK ItemKind = iota
	ItemPerm
	ItemPermDone
	ItemExited
)

// Item is one conversation event for the A2A turn.
type Item struct {
	Kind   ItemKind
	Raw    json.RawMessage
	Perm   *Perm
	PermID string
}

// Subscription is an unbounded, ordered event queue for the A2A turn, so the
// bridge reader never blocks on a slow or parked turn.
type Subscription struct {
	mu     sync.Mutex
	items  []Item
	notify chan struct{}
}

func newSubscription() *Subscription {
	return &Subscription{notify: make(chan struct{}, 1)}
}

func (s *Subscription) push(item Item) {
	s.mu.Lock()
	s.items = append(s.items, item)
	s.mu.Unlock()
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

// Next returns the next item, waiting for one or for ctx.
func (s *Subscription) Next(ctx context.Context) (Item, error) {
	for {
		s.mu.Lock()
		if len(s.items) > 0 {
			item := s.items[0]
			s.items = s.items[1:]
			s.mu.Unlock()
			return item, nil
		}
		s.mu.Unlock()
		select {
		case <-s.notify:
		case <-ctx.Done():
			return Item{}, ctx.Err()
		}
	}
}
