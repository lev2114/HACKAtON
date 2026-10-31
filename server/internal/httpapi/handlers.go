// Package httpapi wires the login endpoint and the WebSocket endpoint
// together with the world, store, hub and ai packages. This is the only
// package that knows the wire protocol described in PROTOCOL.md.
package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/gorilla/websocket"

	"server/internal/ai"
	"server/internal/hub"
	"server/internal/store"
	"server/internal/world"
)

type Server struct {
	World *world.World
	Store *store.Store
	Hub   *hub.Hub
	AI    *ai.Responder

	upgrader websocket.Upgrader
}

func New(w *world.World, st *store.Store, h *hub.Hub, responder *ai.Responder) *Server {
	return &Server{
		World: w,
		Store: st,
		Hub:   h,
		AI:    responder,
		upgrader: websocket.Upgrader{
			ReadBufferSize:  1024,
			WriteBufferSize: 1024,
			// LAN hackathon demo: client and server may be on different
			// hosts/ports, so we don't restrict origins.
			CheckOrigin: func(r *http.Request) bool { return true },
		},
	}
}

func randomID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand failing means the OS is broken; nothing to recover
	}
	return hex.EncodeToString(b)
}

// --- POST /api/login ---

type loginRequest struct {
	Name string `json:"name"`
}

type loginResponse struct {
	PlayerID string `json:"playerId"`
	Token    string `json:"token"`
	Room     string `json:"room"`
}

func (s *Server) Login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}

	player := store.Player{
		ID:    randomID(),
		Name:  req.Name,
		Token: randomID(),
		Room:  "hub",
	}
	if err := s.Store.UpsertPlayer(player); err != nil {
		log.Printf("login: store error: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(loginResponse{PlayerID: player.ID, Token: player.Token, Room: player.Room})
}

// --- GET /ws?token=... ---

func (s *Server) WS(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if token == "" {
		http.Error(w, "missing token", http.StatusUnauthorized)
		return
	}

	player, err := s.Store.PlayerByToken(token)
	if err != nil {
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return
	}

	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("ws: upgrade error: %v", err)
		return
	}

	client := hub.NewClient(conn, player.ID, player.Name, player.Room)
	s.Hub.Register(client)
	go client.WritePump()

	defer func() {
		s.Hub.Unregister(player.ID)
		client.Close()
		if err := s.Store.SavePosition(player.ID, client.Room, player.X, player.Y, player.Z); err != nil {
			log.Printf("disconnect: save error for %s: %v", player.ID, err)
		}
		s.Hub.BroadcastToRoom(client.Room, player.ID, map[string]any{
			"type": "player_left", "playerId": player.ID,
		})
		conn.Close()
	}()

	chunk, _ := s.World.Chunk(player.Room)
	client.Send(map[string]any{
		"type":     "welcome",
		"playerId": player.ID,
		"name":     player.Name,
		"position": world.Vec3{X: player.X, Y: player.Y, Z: player.Z},
		"chunk":    chunk,
	})
	s.Hub.BroadcastToRoom(player.Room, player.ID, map[string]any{
		"type": "player_joined", "playerId": player.ID, "name": player.Name,
		"x": player.X, "y": player.Y, "z": player.Z,
	})

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		s.handleMessage(client, player, data)
	}
}

type incoming struct {
	Type    string  `json:"type"`
	Room    string  `json:"room"`
	X       float64 `json:"x"`
	Y       float64 `json:"y"`
	Z       float64 `json:"z"`
	NpcID   string  `json:"npcId"`
	Message string  `json:"message"`
}

func (s *Server) handleMessage(client *hub.Client, player *store.Player, raw []byte) {
	var msg incoming
	if err := json.Unmarshal(raw, &msg); err != nil {
		log.Printf("ws: bad message from %s: %v", player.ID, err)
		return
	}

	switch msg.Type {
	case "move":
		s.onMove(client, player, msg)
	case "chunk_request":
		s.onChunkRequest(client, player, msg)
	case "npc_talk":
		s.onNpcTalk(client, player, msg)
	default:
		log.Printf("ws: unknown message type %q from %s", msg.Type, player.ID)
	}
}

// moveSaveInterval throttles how often a player's position hits SQLite.
// Movement is broadcast to other players on every single "move" (that must
// stay snappy), but persisting every move to disk is both unnecessary (we
// only need a "good enough" position to resume from) and actively harmful:
// each SQLite write is a blocking disk fsync, and this handler runs in the
// same goroutine that reads the socket — a write-per-move burst (e.g. a
// player holding a key for a few seconds, ~20 moves/sec) can't drain the
// socket fast enough, which backs up until the browser gives up on the
// connection as unresponsive and closes it out from under us.
const moveSaveInterval = 1 * time.Second

func (s *Server) onMove(client *hub.Client, player *store.Player, msg incoming) {
	player.X, player.Y, player.Z = msg.X, msg.Y, msg.Z

	if now := time.Now(); now.Sub(client.LastPersistedMoveAt) >= moveSaveInterval {
		client.LastPersistedMoveAt = now
		if err := s.Store.SavePosition(player.ID, client.Room, msg.X, msg.Y, msg.Z); err != nil {
			log.Printf("move: save error for %s: %v", player.ID, err)
		}
	}

	s.Hub.BroadcastToRoom(client.Room, player.ID, map[string]any{
		"type": "player_moved", "playerId": player.ID, "x": msg.X, "y": msg.Y, "z": msg.Z,
	})
}

func (s *Server) onChunkRequest(client *hub.Client, player *store.Player, msg incoming) {
	chunk, ok := s.World.Chunk(msg.Room)
	if !ok {
		client.Send(map[string]any{"type": "error", "code": "unknown_room", "message": "нет такой комнаты: " + msg.Room})
		return
	}

	oldRoom := client.Room
	client.Room = msg.Room
	s.Hub.SetRoom(player.ID, msg.Room)
	if err := s.Store.SavePosition(player.ID, msg.Room, player.X, player.Y, player.Z); err != nil {
		log.Printf("chunk_request: save error for %s: %v", player.ID, err)
	}

	client.Send(map[string]any{"type": "chunk_data", "chunk": chunk})
	s.Hub.BroadcastToRoom(oldRoom, player.ID, map[string]any{"type": "player_left", "playerId": player.ID})
	s.Hub.BroadcastToRoom(msg.Room, player.ID, map[string]any{
		"type": "player_joined", "playerId": player.ID, "name": player.Name,
		"x": player.X, "y": player.Y, "z": player.Z,
	})
}

func (s *Server) onNpcTalk(client *hub.Client, player *store.Player, msg incoming) {
	npc, ok := s.World.NPCs[msg.NpcID]
	if !ok {
		client.Send(map[string]any{"type": "error", "code": "unknown_npc", "message": "нет такого NPC: " + msg.NpcID})
		return
	}

	told, err := s.Store.ToldFacts(player.ID, npc.ID)
	if err != nil {
		log.Printf("npc_talk: store error for %s/%s: %v", player.ID, npc.ID, err)
		told = map[string]bool{}
	}

	fact, hasFact := s.World.NextUntoldFact(npc.ID, told)
	repeat := hasFact && told[fact.ID]

	text := s.AI.Reply(context.Background(), npc, fact, msg.Message, repeat)

	if hasFact && !repeat {
		if err := s.Store.MarkToldFact(player.ID, npc.ID, fact.ID); err != nil {
			log.Printf("npc_talk: mark told error for %s/%s: %v", player.ID, npc.ID, err)
		}
	}

	client.Send(map[string]any{
		"type": "npc_response", "npcId": npc.ID, "npcName": npc.Name, "text": text,
	})
}
