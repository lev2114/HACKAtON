// Package httpapi wires the login endpoint and the WebSocket endpoint
// together with the world, store, hub and ai packages. This is the only
// package that knows the wire protocol described in PROTOCOL.md.
package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/gorilla/websocket"
	"golang.org/x/crypto/bcrypt"

	"server/internal/ai"
	"server/internal/hub"
	"server/internal/ratelimit"
	"server/internal/store"
	"server/internal/world"
)

type Server struct {
	World    *world.World
	Store    *store.Store
	Hub      *hub.Hub
	AI       *ai.Responder
	facility *facilityState

	upgrader websocket.Upgrader

	// Keyed by client IP. Brute-forcing a password is the realistic threat
	// here (both endpoints take a name+password over plain HTTP-shaped
	// requests) — these just make that impractically slow, not impossible.
	// Hackathon-scale: in-memory, per-process, no cleanup of old keys.
	loginLimiter    *ratelimit.Limiter
	registerLimiter *ratelimit.Limiter
	adminLimiter    *ratelimit.Limiter

	settingsMu sync.RWMutex
	settings   Settings

	adminMu       sync.Mutex
	adminSessions map[string]adminSession

	startedAt time.Time
	baseModel string // OLLAMA_MODEL at startup — "reset settings" target
}

func New(w *world.World, st *store.Store, h *hub.Hub, responder *ai.Responder) *Server {
	_, model := responder.LLM()
	return &Server{
		World:         w,
		Store:         st,
		Hub:           h,
		AI:            responder,
		facility:      newFacility(w),
		settings:      defaultSettings(model),
		adminSessions: map[string]adminSession{},
		adminLimiter:  ratelimit.New(8, 30*time.Second),
		startedAt:     time.Now(),
		baseModel:     model,
		upgrader: websocket.Upgrader{
			ReadBufferSize:  1024,
			WriteBufferSize: 1024,
			// LAN hackathon demo: client and server may be on different
			// hosts/ports, so we don't restrict origins.
			CheckOrigin: func(r *http.Request) bool { return true },
		},
		loginLimiter:    ratelimit.New(8, 30*time.Second),
		registerLimiter: ratelimit.New(5, 60*time.Second),
	}
}

// clientIP strips the port off RemoteAddr for use as a rate-limit key. Falls
// back to the raw value if it isn't a "host:port" pair (shouldn't happen via
// net/http, but better than panicking on malformed input).
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func rateLimitMessage(retryAfter time.Duration) string {
	return fmt.Sprintf("слишком много попыток, попробуй снова через %d сек", int(retryAfter.Seconds())+1)
}

func randomID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand failing means the OS is broken; nothing to recover
	}
	return hex.EncodeToString(b)
}

// --- POST /api/register, POST /api/login ---

type authRequest struct {
	Name     string `json:"name"`
	Password string `json:"password"`
}

type authResponse struct {
	PlayerID string `json:"playerId"`
	Token    string `json:"token"`
	Room     string `json:"room"`
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}

func decodeAuthRequest(r *http.Request) (authRequest, error) {
	var req authRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return req, fmt.Errorf("bad request body")
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		return req, fmt.Errorf("имя не может быть пустым")
	}
	if req.Password == "" {
		return req, fmt.Errorf("пароль не может быть пустым")
	}
	return req, nil
}

const passwordPolicyMessage = "пароль должен быть не короче 8 символов и содержать цифру, строчную и заглавную букву"

// validatePasswordPolicy only applies to NEW passwords (registration). Login
// deliberately doesn't re-check it — accounts created before this policy
// existed still have short/weak-by-today's-rules passwords, and login has to
// keep accepting those or every existing player gets locked out.
func validatePasswordPolicy(password string) error {
	if len(password) < 8 {
		return errors.New(passwordPolicyMessage)
	}
	var hasDigit, hasLower, hasUpper bool
	for _, r := range password {
		switch {
		case unicode.IsDigit(r):
			hasDigit = true
		case unicode.IsLower(r):
			hasLower = true
		case unicode.IsUpper(r):
			hasUpper = true
		}
	}
	if !hasDigit || !hasLower || !hasUpper {
		return errors.New(passwordPolicyMessage)
	}
	return nil
}

func (s *Server) Register(w http.ResponseWriter, r *http.Request) {
	if allowed, retryAfter := s.registerLimiter.Allow(clientIP(r)); !allowed {
		writeJSONError(w, http.StatusTooManyRequests, rateLimitMessage(retryAfter))
		return
	}

	req, err := decodeAuthRequest(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validatePasswordPolicy(req.Password); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	if _, err := s.Store.PlayerByName(req.Name); err == nil {
		writeJSONError(w, http.StatusConflict, "это имя уже занято, выбери другое или войди")
		return
	} else if !errors.Is(err, store.ErrNotFound) {
		log.Printf("register: lookup error: %v", err)
		writeJSONError(w, http.StatusInternalServerError, "внутренняя ошибка сервера")
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		log.Printf("register: hash error: %v", err)
		writeJSONError(w, http.StatusInternalServerError, "внутренняя ошибка сервера")
		return
	}

	player := store.Player{
		ID:           randomID(),
		Name:         req.Name,
		PasswordHash: string(hash),
		Token:        randomID(),
		Room:         "hub",
	}
	if room, ok := s.World.Rooms[player.Room]; ok && room.Spawn != nil {
		player.X = room.Position.X + room.Spawn.X
		player.Y = room.Position.Y + room.Spawn.Y
		player.Z = room.Position.Z + room.Spawn.Z
	}
	if err := s.Store.CreatePlayer(player); err != nil {
		log.Printf("register: store error: %v", err)
		writeJSONError(w, http.StatusInternalServerError, "внутренняя ошибка сервера")
		return
	}
	if AdminNames[player.Name] {
		if err := s.Store.SetAdmin(player.Name, true); err != nil {
			log.Printf("register: grant admin %q: %v", player.Name, err)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(authResponse{PlayerID: player.ID, Token: player.Token, Room: player.Room})
}

func (s *Server) Login(w http.ResponseWriter, r *http.Request) {
	if allowed, retryAfter := s.loginLimiter.Allow(clientIP(r)); !allowed {
		writeJSONError(w, http.StatusTooManyRequests, rateLimitMessage(retryAfter))
		return
	}

	req, err := decodeAuthRequest(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	player, err := s.Store.PlayerByName(req.Name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSONError(w, http.StatusUnauthorized, "неверное имя или пароль")
			return
		}
		log.Printf("login: lookup error: %v", err)
		writeJSONError(w, http.StatusInternalServerError, "внутренняя ошибка сервера")
		return
	}

	if err := bcrypt.CompareHashAndPassword([]byte(player.PasswordHash), []byte(req.Password)); err != nil {
		writeJSONError(w, http.StatusUnauthorized, "неверное имя или пароль")
		return
	}

	token := randomID()
	if err := s.Store.UpdateToken(player.ID, token); err != nil {
		log.Printf("login: token update error: %v", err)
		writeJSONError(w, http.StatusInternalServerError, "внутренняя ошибка сервера")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(authResponse{PlayerID: player.ID, Token: token, Room: player.Room})
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
	if _, valid := s.World.Rooms[player.Room]; !valid || math.IsNaN(player.X) || math.IsNaN(player.Y) || math.IsNaN(player.Z) || math.IsInf(player.X, 0) || math.IsInf(player.Y, 0) || math.IsInf(player.Z, 0) {
		room, exists := s.World.Rooms["hub"]
		if !exists {
			http.Error(w, "starting room unavailable", http.StatusInternalServerError)
			return
		}
		player.Room = "hub"
		player.X, player.Y, player.Z = room.Position.X, room.Position.Y+1.6, room.Position.Z
		if room.Spawn != nil {
			player.X += room.Spawn.X
			player.Y = room.Position.Y + room.Spawn.Y
			player.Z += room.Spawn.Z
		}
		if err := s.Store.SavePosition(player.ID, player.Room, player.X, player.Y, player.Z); err != nil {
			log.Printf("ws: recover room for %s: %v", player.ID, err)
			http.Error(w, "could not restore player room", http.StatusInternalServerError)
			return
		}
	}

	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("ws: upgrade error: %v", err)
		return
	}

	client := hub.NewClient(conn, player.ID, player.Name, player.Room)
	if !s.Hub.Register(client) {
		if err := conn.WriteJSON(map[string]any{"type": "error", "code": "session_in_use", "message": "Этот аккаунт уже подключён. Выйди в другом окне или используй другой аккаунт."}); err != nil {
			log.Printf("ws: duplicate session: %v", err)
		}
		conn.Close()
		return
	}
	s.Hub.UpdatePosition(player.ID, player.Room, player.X, player.Y, player.Z)
	go client.WritePump()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	var replyBusy atomic.Bool
	stopPing := make(chan struct{})
	go s.pingLoop(client, stopPing)

	defer func() {
		cancel()
		close(stopPing)
		room := s.Hub.RoomOf(player.ID) // перед Unregister — потом хаб его уже не помнит
		if _, valid := s.World.Rooms[room]; !valid {
			room = player.Room
		}
		s.Hub.Unregister(player.ID)
		s.facility.mu.Lock()
		delete(s.facility.conversations, player.ID)
		s.closeRoomDoors(room)
		s.facility.mu.Unlock()
		client.Close()
		if err := s.Store.SavePosition(player.ID, room, player.X, player.Y, player.Z); err != nil {
			log.Printf("disconnect: save error for %s: %v", player.ID, err)
		}
		s.Hub.BroadcastToRoom(room, player.ID, map[string]any{
			"type": "player_left", "playerId": player.ID,
		})
		conn.Close()
	}()

	chunk, _ := s.World.Chunk(player.Room)
	s.facility.mu.Lock()
	playerState, err := s.Store.WorldState(player.ID)
	if err != nil {
		s.facility.mu.Unlock()
		log.Printf("ws: load world state: %v", err)
		client.Send(map[string]any{"type": "error", "code": "world_state", "message": "Не удалось загрузить предметы и скафандр"})
		return
	}
	playerState.Revision = s.facility.revision
	s.Hub.SetWorldState(player.ID, playerState.SuitColor, playerState.Apples)
	client.Send(map[string]any{
		"type":        "welcome",
		"playerId":    player.ID,
		"name":        player.Name,
		"position":    world.Vec3{X: player.X, Y: player.Y, Z: player.Z},
		"chunk":       chunk,
		"state":       s.facilitySnapshot(player.Room),
		"settings":    s.clientSettings(),
		"playerState": playerState,
		// Кто уже стоит в этой комнате — без этого видно только тех, кто
		// присоединился/подвинулся ПОСЛЕ тебя, а не тех, кто был раньше.
		"players": s.Hub.PlayersInRoom(player.Room, player.ID),
	})
	s.facility.mu.Unlock()
	s.Hub.BroadcastToRoom(player.Room, player.ID, map[string]any{
		"type": "player_joined", "playerId": player.ID, "name": player.Name,
		"x": player.X, "y": player.Y, "z": player.Z,
		"suitColor": playerState.SuitColor, "apples": playerState.Apples,
	})

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		s.handleMessage(ctx, &replyBusy, client, player, data)
	}
}

type incoming struct {
	Item           string  `json:"item"`
	ConversationID uint64  `json:"-"`
	Active         bool    `json:"active"`
	RequestID      uint64  `json:"requestId"`
	Door           string  `json:"door"`
	Open           bool    `json:"open"`
	Type           string  `json:"type"`
	Room           string  `json:"room"`
	X              float64 `json:"x"`
	Y              float64 `json:"y"`
	Z              float64 `json:"z"`
	NpcID          string  `json:"npcId"`
	Message        string  `json:"message"`
	Text           string  `json:"text"`

	// pong
	ID        uint64  `json:"id"`
	FPS       float64 `json:"fps"`
	FrameMs   float64 `json:"frameMs"`
	MemoryMB  float64 `json:"memoryMB"`
	DrawCalls int     `json:"drawCalls"`
	Triangles int     `json:"triangles"`
	Paused    bool    `json:"paused"`
	ChatOpen  bool    `json:"chatOpen"`
	GPU       string  `json:"gpu"`
}

const pingInterval = 2 * time.Second

// pingLoop measures RTT per connection: the server timestamps each ping and
// the client answers with a pong carrying the same id plus its own FPS etc.
func (s *Server) pingLoop(client *hub.Client, stop <-chan struct{}) {
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()
	var id uint64
	for {
		id++
		client.SentPing(id)
		client.Send(map[string]any{"type": "ping", "id": id})
		select {
		case <-stop:
			return
		case <-ticker.C:
		}
	}
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

func (s *Server) onPong(client *hub.Client, msg incoming) {
	clamp := func(v, max float64) float64 {
		if math.IsNaN(v) || v < 0 {
			return 0
		}
		return math.Min(v, max)
	}
	client.ReceivedPong(msg.ID, hub.ClientStats{
		FPS:       clamp(msg.FPS, 1000),
		FrameMs:   clamp(msg.FrameMs, 10000),
		MemoryMB:  clamp(msg.MemoryMB, 1e6),
		DrawCalls: max(msg.DrawCalls, 0),
		Triangles: max(msg.Triangles, 0),
		Paused:    msg.Paused,
		ChatOpen:  msg.ChatOpen,
		GPU:       truncate(msg.GPU, 160),
	})
}

func (s *Server) handleMessage(ctx context.Context, replyBusy *atomic.Bool, client *hub.Client, player *store.Player, raw []byte) {
	var msg incoming
	if err := json.Unmarshal(raw, &msg); err != nil {
		log.Printf("ws: bad message from %s: %v", player.ID, err)
		return
	}

	switch msg.Type {
	case "use_item":
		s.onUseItem(client, player, msg)
	case "chat":
		s.onPlayerChat(client, player, msg)
	case "npc_conversation":
		s.onNpcConversation(client, player, msg)
	case "door_set":
		s.onDoorSet(client, player, msg)
	case "move":
		s.onMove(client, player, msg)
	case "chunk_request":
		s.onChunkRequest(client, player, msg)
	case "npc_talk":
		if !s.canTalkToNPC(player, msg.NpcID) {
			client.Send(map[string]any{"type": "error", "code": "npc_out_of_reach", "message": "Подойди ближе и начни разговор заново", "requestId": msg.RequestID})
			return
		}
		if !replyBusy.CompareAndSwap(false, true) {
			client.Send(map[string]any{"type": "error", "code": "npc_busy", "message": "Дождись предыдущего ответа и отправь вопрос ещё раз", "requestId": msg.RequestID})
			return
		}
		s.facility.mu.Lock()
		msg.ConversationID = s.facility.conversations[player.ID].id
		s.facility.mu.Unlock()
		playerCopy := *player // Read loop can keep updating player while AI runs.
		go func() {
			defer replyBusy.Store(false)
			s.onNpcTalk(ctx, client, &playerCopy, msg)
		}()
	case "pong":
		s.onPong(client, msg)
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
	room := s.Hub.RoomOf(player.ID)
	s.Hub.UpdatePosition(player.ID, room, msg.X, msg.Y, msg.Z)

	if now := time.Now(); now.Sub(client.LastPersistedMoveAt) >= moveSaveInterval {
		client.LastPersistedMoveAt = now
		if err := s.Store.SavePosition(player.ID, room, msg.X, msg.Y, msg.Z); err != nil {
			log.Printf("move: save error for %s: %v", player.ID, err)
		}
	}

	s.Hub.BroadcastToRoom(room, player.ID, map[string]any{
		"type": "player_moved", "playerId": player.ID, "x": msg.X, "y": msg.Y, "z": msg.Z,
	})
}

func (s *Server) onChunkRequest(client *hub.Client, player *store.Player, msg incoming) {
	s.facility.mu.Lock()
	defer s.facility.mu.Unlock()
	oldRoom := s.Hub.RoomOf(player.ID)
	if !s.canUseDoor(oldRoom, msg.Room, player) {
		s.transitionError(client)
		return
	}
	chunk, ok := s.World.Chunk(msg.Room)
	if !ok {
		client.Send(map[string]any{"type": "error", "code": "unknown_room", "message": "нет такой комнаты: " + msg.Room})
		return
	}

	for _, door := range chunk.Room.Doors {
		if door.Room != oldRoom {
			continue
		}
		length := math.Hypot(door.Position.X, door.Position.Z)
		player.X = chunk.Room.Position.X + door.Position.X - door.Position.X/length*1.4
		player.Y = chunk.Room.Position.Y + door.Position.Y + 1.6
		player.Z = chunk.Room.Position.Z + door.Position.Z - door.Position.Z/length*1.4
		break
	}
	player.Room = msg.Room
	delete(s.facility.conversations, player.ID)
	s.Hub.UpdatePosition(player.ID, msg.Room, player.X, player.Y, player.Z)
	s.closeRoomDoors(oldRoom)
	if err := s.Store.SavePosition(player.ID, msg.Room, player.X, player.Y, player.Z); err != nil {
		log.Printf("chunk_request: save error for %s: %v", player.ID, err)
	}

	client.Send(map[string]any{
		"type":    "chunk_data",
		"chunk":   chunk,
		"state":   s.facilitySnapshot(msg.Room),
		"players": s.Hub.PlayersInRoom(msg.Room, player.ID),
	})
	s.Hub.BroadcastToRoom(oldRoom, player.ID, map[string]any{"type": "player_left", "playerId": player.ID})
	appearance, _ := s.Hub.Player(player.ID)
	s.Hub.BroadcastToRoom(msg.Room, player.ID, map[string]any{
		"type": "player_joined", "playerId": player.ID, "name": player.Name,
		"x": player.X, "y": player.Y, "z": player.Z,
		"suitColor": appearance.SuitColor, "apples": appearance.Apples,
	})
}

func (s *Server) onNpcTalk(ctx context.Context, client *hub.Client, player *store.Player, msg incoming) {
	npc, ok := s.World.NPC(msg.NpcID)
	if !ok {
		s.Hub.SendToClient(client, map[string]any{"type": "error", "code": "unknown_npc", "message": "нет такого NPC: " + msg.NpcID, "requestId": msg.RequestID})
		return
	}
	proposal, planErr := s.AI.PlanWorldAction(ctx, npc, msg.Message, player.ID)
	if ctx.Err() != nil {
		return
	}
	if planErr != nil {
		log.Printf("npc action planner %s: %v", npc.ID, planErr)
	}
	if proposal != nil {
		text := s.executeWorldAction(client, npc, msg, proposal)
		s.AI.RecordWorldAction(npc.ID, player.ID, msg.Message, text)
		s.Hub.SendToClient(client, map[string]any{"type": "npc_response", "npcId": npc.ID, "npcName": npc.Name, "text": text, "requestId": msg.RequestID})
		return
	}

	told, err := s.Store.ToldFacts(player.ID, npc.ID)
	if err != nil {
		log.Printf("npc_talk: store error for %s/%s: %v", player.ID, npc.ID, err)
		told = map[string]bool{}
	}

	// Use keyword-aware PickFact instead of simple NextUntoldFact
	fact, isRepeat, availableTopics := s.World.PickFact(npc.ID, msg.Message, told)

	text := s.AI.Reply(ctx, npc, fact, msg.Message, isRepeat, availableTopics, player.ID)
	if ctx.Err() != nil {
		return
	}

	if fact != nil && !isRepeat {
		if err := s.Store.MarkToldFact(player.ID, npc.ID, fact.ID); err != nil {
			log.Printf("npc_talk: mark told error for %s/%s: %v", player.ID, npc.ID, err)
		}
	}

	s.Hub.SendToClient(client, map[string]any{
		"type": "npc_response", "npcId": npc.ID, "npcName": npc.Name, "text": text, "requestId": msg.RequestID,
	})
}

func (s *Server) onPlayerChat(client *hub.Client, player *store.Player, msg incoming) {
	text := strings.TrimSpace(msg.Text)
	if text == "" {
		text = strings.TrimSpace(msg.Message)
	}
	if text == "" {
		return
	}
	if len([]rune(text)) > 200 {
		text = string([]rune(text)[:200])
	}

	cleanText := FilterProfanity(text)
	room := s.Hub.RoomOf(player.ID)
	if room == "" {
		room = player.Room
	}

	payload := map[string]any{
		"type":      "player_chat",
		"playerId":  player.ID,
		"name":      player.Name,
		"text":      cleanText,
		"room":      room,
		"timestamp": time.Now().UnixMilli(),
	}

	s.Hub.BroadcastAll(payload)
}
