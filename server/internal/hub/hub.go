// Package hub tracks connected WebSocket clients and broadcasts messages to
// everyone in a given room. Only players physically "in" the same room get
// each other's movement updates.
package hub

import (
	"encoding/json"
	"log"
	"sync"

	"github.com/gorilla/websocket"
)

type Client struct {
	Conn     *websocket.Conn
	PlayerID string
	Name     string
	Room     string

	send chan []byte
}

func NewClient(conn *websocket.Conn, playerID, name, room string) *Client {
	return &Client{
		Conn:     conn,
		PlayerID: playerID,
		Name:     name,
		Room:     room,
		send:     make(chan []byte, 16),
	}
}

func (c *Client) Send(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		log.Printf("hub: marshal error for %s: %v", c.PlayerID, err)
		return
	}
	select {
	case c.send <- data:
	default:
		log.Printf("hub: dropped message for slow client %s", c.PlayerID)
	}
}

// WritePump must run in its own goroutine for the lifetime of the
// connection; it's the only goroutine allowed to write to the socket.
func (c *Client) WritePump() {
	for msg := range c.send {
		if err := c.Conn.WriteMessage(websocket.TextMessage, msg); err != nil {
			return
		}
	}
}

// Close stops WritePump once it drains whatever is already buffered. Call
// it only after Unregister, so no other goroutine can still be calling Send.
func (c *Client) Close() {
	close(c.send)
}

type Hub struct {
	mu      sync.RWMutex
	clients map[string]*Client
}

func New() *Hub {
	return &Hub{clients: map[string]*Client{}}
}

func (h *Hub) Register(c *Client) {
	h.mu.Lock()
	h.clients[c.PlayerID] = c
	h.mu.Unlock()
}

func (h *Hub) Unregister(playerID string) {
	h.mu.Lock()
	delete(h.clients, playerID)
	h.mu.Unlock()
}

func (h *Hub) SetRoom(playerID, room string) {
	h.mu.Lock()
	if c, ok := h.clients[playerID]; ok {
		c.Room = room
	}
	h.mu.Unlock()
}

// BroadcastToRoom sends v to every connected client currently in roomID,
// except exceptPlayerID (pass "" to include everyone).
func (h *Hub) BroadcastToRoom(roomID, exceptPlayerID string, v any) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for id, c := range h.clients {
		if id == exceptPlayerID || c.Room != roomID {
			continue
		}
		c.Send(v)
	}
}
