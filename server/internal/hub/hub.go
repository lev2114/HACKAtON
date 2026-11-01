// Package hub tracks connected WebSocket clients and broadcasts messages to
// everyone in a given room. Only players physically "in" the same room get
// each other's movement updates.
package hub

import (
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type Client struct {
	Conn     *websocket.Conn
	PlayerID string
	Name     string

	// room/x/y/z are written by this client's own read-loop goroutine but
	// READ by every other connection's goroutine via BroadcastToRoom and
	// PlayersInRoom — unexported on purpose, so the only way to touch them
	// from outside this file is through Hub's locked methods below. Direct
	// field access here (within the hub package) still goes through h.mu.
	room    string
	x, y, z float64

	// LastPersistedMoveAt is read/written only from this client's own
	// read-loop goroutine (never concurrently), so it needs no locking —
	// see the throttling comment in httpapi.onMove.
	LastPersistedMoveAt time.Time

	ConnectedAt time.Time
	RemoteAddr  string

	send chan []byte

	// sendMu/closed make Send safe after Close: npc_talk replies are
	// produced on their own goroutine and may finish after the player left.
	sendMu sync.Mutex
	closed bool

	statsMu sync.Mutex
	stats   ClientStats
	pings   map[uint64]time.Time
	dropped int
}

// ClientStats is what the admin panel shows per connection. RTT is measured
// by the server (ping → pong round trip); the rest is self-reported by the
// client inside its pong.
type ClientStats struct {
	PingMs     float64   `json:"pingMs"`
	FPS        float64   `json:"fps"`
	FrameMs    float64   `json:"frameMs"`
	MemoryMB   float64   `json:"memoryMB"`
	DrawCalls  int       `json:"drawCalls"`
	Triangles  int       `json:"triangles"`
	Paused     bool      `json:"paused"`
	ChatOpen   bool      `json:"chatOpen"`
	GPU        string    `json:"gpu"`
	Dropped    int       `json:"droppedMessages"`
	LastReport time.Time `json:"lastReport"`
}

func NewClient(conn *websocket.Conn, playerID, name, room string) *Client {
	return &Client{
		Conn:        conn,
		PlayerID:    playerID,
		Name:        name,
		room:        room,
		send:        make(chan []byte, 16),
		ConnectedAt: time.Now(),
		RemoteAddr:  conn.RemoteAddr().String(),
		pings:       map[uint64]time.Time{},
	}
}

func (c *Client) Send(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		log.Printf("hub: marshal error for %s: %v", c.PlayerID, err)
		return
	}
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	if c.closed {
		return
	}
	select {
	case c.send <- data:
	default:
		c.statsMu.Lock()
		c.dropped++
		c.statsMu.Unlock()
		log.Printf("hub: dropped message for slow client %s", c.PlayerID)
	}
}

// SentPing remembers when ping id left, so the matching pong yields an RTT.
func (c *Client) SentPing(id uint64) {
	c.statsMu.Lock()
	defer c.statsMu.Unlock()
	c.pings[id] = time.Now()
	// A client that never answers must not grow this map forever.
	for k, t := range c.pings {
		if time.Since(t) > time.Minute {
			delete(c.pings, k)
		}
	}
}

// ReceivedPong records the round trip for ping id and the client's report.
// Unknown ids (already expired, or forged) only update the report.
func (c *Client) ReceivedPong(id uint64, report ClientStats) {
	c.statsMu.Lock()
	defer c.statsMu.Unlock()
	report.PingMs = c.stats.PingMs
	if sent, ok := c.pings[id]; ok {
		report.PingMs = float64(time.Since(sent).Microseconds()) / 1000
		delete(c.pings, id)
	}
	report.LastReport = time.Now()
	c.stats = report
}

func (c *Client) Stats() ClientStats {
	c.statsMu.Lock()
	defer c.statsMu.Unlock()
	s := c.stats
	s.Dropped = c.dropped
	return s
}

// WritePump must run in its own goroutine for the lifetime of the
// connection; it's the only goroutine allowed to write to the socket.
func (c *Client) WritePump() {
	for msg := range c.send {
		if err := c.Conn.WriteMessage(websocket.TextMessage, msg); err != nil {
			log.Printf("hub: write error for %s, write pump exiting: %v", c.PlayerID, err)
			return
		}
	}
}

// Close stops WritePump once it drains whatever is already buffered. Call
// it only after Unregister, so no other goroutine can still be calling Send.
func (c *Client) Close() {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	close(c.send)
}

type Hub struct {
	mu      sync.RWMutex
	clients map[string]*Client
}

func New() *Hub {
	return &Hub{clients: map[string]*Client{}}
}

func (h *Hub) Register(c *Client) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	// Replacing an existing connection lets its cleanup unregister the new
	// one, eventually persisting an empty room. Keep one socket per account.
	if _, exists := h.clients[c.PlayerID]; exists {
		return false
	}
	h.clients[c.PlayerID] = c
	return true
}

func (h *Hub) Unregister(playerID string) {
	h.mu.Lock()
	delete(h.clients, playerID)
	h.mu.Unlock()
}

// RoomOf returns the room a player is currently in, or "" if they're not
// connected (e.g. already disconnected — call this BEFORE Unregister if you
// need the room they were just in).
func (h *Hub) RoomOf(playerID string) string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if c, ok := h.clients[playerID]; ok {
		return c.room
	}
	return ""
}

// UpdatePosition is the only way room/x/y/z are allowed to change after a
// client is registered — every write goes through h.mu so BroadcastToRoom
// and PlayersInRoom (running on other connections' goroutines) never see a
// torn or stale value.
func (h *Hub) UpdatePosition(playerID, room string, x, y, z float64) {
	h.mu.Lock()
	if c, ok := h.clients[playerID]; ok {
		c.room, c.x, c.y, c.z = room, x, y, z
	}
	h.mu.Unlock()
}

// PlayerInfo is the live-position shape broadcast for "who's already here"
// (on room entry) and "who just moved" — see PROTOCOL.md.
type PlayerInfo struct {
	PlayerID string  `json:"playerId"`
	Name     string  `json:"name"`
	X        float64 `json:"x"`
	Y        float64 `json:"y"`
	Z        float64 `json:"z"`
}

// PlayersInRoom lists everyone currently in roomID except exceptPlayerID —
// used when a player enters a room, so their client can spawn meshes for
// players who were already standing there (player_joined only fires for
// people who arrive AFTER you, so without this you'd never see them).
func (h *Hub) PlayersInRoom(roomID, exceptPlayerID string) []PlayerInfo {
	h.mu.RLock()
	defer h.mu.RUnlock()
	var out []PlayerInfo
	for id, c := range h.clients {
		if id == exceptPlayerID || c.room != roomID {
			continue
		}
		out = append(out, PlayerInfo{PlayerID: id, Name: c.Name, X: c.x, Y: c.y, Z: c.z})
	}
	return out
}

func (h *Hub) Get(playerID string) (*Client, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	c, ok := h.clients[playerID]
	return c, ok
}

// ConnectedClient is one row of the admin panel's live client table.
type ConnectedClient struct {
	PlayerInfo
	Room        string      `json:"room"`
	RemoteAddr  string      `json:"remoteAddr"`
	ConnectedAt time.Time   `json:"connectedAt"`
	Stats       ClientStats `json:"stats"`
}

func (h *Hub) Snapshot() []ConnectedClient {
	h.mu.RLock()
	clients := make([]*Client, 0, len(h.clients))
	rows := make([]ConnectedClient, 0, len(h.clients))
	for id, c := range h.clients {
		clients = append(clients, c)
		rows = append(rows, ConnectedClient{
			PlayerInfo:  PlayerInfo{PlayerID: id, Name: c.Name, X: c.x, Y: c.y, Z: c.z},
			Room:        c.room,
			RemoteAddr:  c.RemoteAddr,
			ConnectedAt: c.ConnectedAt,
		})
	}
	h.mu.RUnlock()
	// Stats take the client's own lock — read them outside h.mu.
	for i, c := range clients {
		rows[i].Stats = c.Stats()
	}
	return rows
}

func (h *Hub) Count() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

func (h *Hub) BroadcastAll(v any) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, c := range h.clients {
		c.Send(v)
	}
}

// BroadcastToRoom sends v to every connected client currently in roomID,
// except exceptPlayerID (pass "" to include everyone).
func (h *Hub) BroadcastToRoom(roomID, exceptPlayerID string, v any) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for id, c := range h.clients {
		if id == exceptPlayerID || c.room != roomID {
			continue
		}
		c.Send(v)
	}
}
