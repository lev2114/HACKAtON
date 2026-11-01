package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"runtime"
	"sort"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"server/internal/ai"
	"server/internal/hub"
	"server/internal/store"
	"server/internal/world"
)

// Settings are the global knobs editable from the admin panel. Walk/sprint
// speed are sent to every game client (see the "settings" WS message).
type Settings struct {
	WalkSpeed       float64 `json:"walkSpeed"`
	SprintSpeed     float64 `json:"sprintSpeed"`
	LLMEnabled      bool    `json:"llmEnabled"`
	Model           string  `json:"model"`
	CleanersEnabled bool    `json:"cleanersEnabled"`
	CleanerSpeed    float64 `json:"cleanerSpeed"`
}

func defaultSettings(model string) Settings {
	return Settings{WalkSpeed: 4, SprintSpeed: 7, LLMEnabled: true, Model: model, CleanersEnabled: true, CleanerSpeed: 1}
}

func (st Settings) validate() error {
	switch {
	case st.WalkSpeed < 1 || st.WalkSpeed > 15:
		return errors.New("скорость ходьбы должна быть от 1 до 15")
	case st.SprintSpeed < st.WalkSpeed || st.SprintSpeed > 25:
		return errors.New("скорость бега должна быть не меньше ходьбы и не больше 25")
	case st.CleanerSpeed < 0 || st.CleanerSpeed > 5:
		return errors.New("скорость уборщиков должна быть от 0 до 5")
	case strings.TrimSpace(st.Model) == "" || len(st.Model) > 100:
		return errors.New("укажи модель Ollama")
	}
	return nil
}

func (s *Server) currentSettings() Settings {
	s.settingsMu.RLock()
	defer s.settingsMu.RUnlock()
	return s.settings
}

func (s *Server) clientSettings() map[string]any {
	st := s.currentSettings()
	return map[string]any{"walkSpeed": st.WalkSpeed, "sprintSpeed": st.SprintSpeed}
}

// applySettings pushes settings into every subsystem. It does not persist.
func (s *Server) applySettings(st Settings) {
	st.Model = strings.TrimSpace(st.Model)
	s.settingsMu.Lock()
	s.settings = st
	s.settingsMu.Unlock()
	s.AI.SetLLM(st.LLMEnabled, st.Model)
	s.setCleaners(st.CleanersEnabled, st.CleanerSpeed)
	s.Hub.BroadcastAll(map[string]any{"type": "settings", "settings": s.clientSettings()})
}

// npcOverride is what the admin panel persists per character.
type npcOverride struct {
	Name        string     `json:"name"`
	Title       string     `json:"title"`
	Personality string     `json:"personality"`
	Profile     ai.Profile `json:"profile"`
}

func (o npcOverride) validate() error {
	p := o.Profile
	switch {
	case strings.TrimSpace(o.Name) == "" || len(o.Name) > 100:
		return errors.New("имя персонажа не может быть пустым")
	case len(o.Title) > 200 || len(o.Personality) > 1000:
		return errors.New("титул или характер слишком длинные")
	case len(p.SystemPrompt) > 8000 || len(p.Greeting) > 1000 || len(p.Offtopic) > 1000:
		return errors.New("промпт или реплики слишком длинные")
	case p.Mode != "" && p.Mode != ai.ModeLLM && p.Mode != ai.ModeFSM && p.Mode != ai.ModeSilent:
		return errors.New("неизвестный режим ИИ")
	case p.Temperature != nil && (*p.Temperature < 0 || *p.Temperature > 2):
		return errors.New("температура должна быть от 0 до 2")
	case p.MaxTokens < 0 || p.MaxTokens > 2048:
		return errors.New("длина ответа должна быть от 0 до 2048 токенов")
	}
	return nil
}

func (s *Server) applyNPCOverride(id string, o npcOverride) bool {
	n, ok := s.World.BaseNPC(id)
	if !ok {
		return false
	}
	n.Name = strings.TrimSpace(o.Name)
	n.Title = strings.TrimSpace(o.Title)
	n.Personality = strings.TrimSpace(o.Personality)
	s.World.SetNPC(n)
	s.AI.SetProfile(id, o.Profile)
	return true
}

var validDifficulty = map[string]bool{"beginner": true, "intermediate": true, "advanced": true}

func validateFacts(bank string, facts []world.Fact) error {
	if len(facts) > 200 {
		return errors.New("слишком много фактов в одном банке (максимум 200)")
	}
	seen := map[string]bool{}
	for i, f := range facts {
		switch {
		case strings.TrimSpace(f.Text) == "" || len(f.Text) > 1000:
			return fmt.Errorf("факт #%d: текст пустой или длиннее 1000 символов", i+1)
		case strings.TrimSpace(f.Topic) == "" || len(f.Topic) > 50:
			return fmt.Errorf("факт #%d: укажи тему", i+1)
		case !validDifficulty[f.Difficulty]:
			return fmt.Errorf("факт #%d: сложность — beginner, intermediate или advanced", i+1)
		case f.ID == "":
			return fmt.Errorf("факт #%d: нет id", i+1)
		case seen[f.ID]:
			return fmt.Errorf("факт #%d: повторяющийся id %q", i+1, f.ID)
		}
		seen[f.ID] = true
	}
	return nil
}

// LoadAdminOverrides applies every saved admin edit on top of world-data.
// A broken row is logged and skipped rather than blocking startup.
func (s *Server) LoadAdminOverrides() error {
	settings, err := s.Store.AdminConfig("settings")
	if err != nil {
		return fmt.Errorf("load settings: %w", err)
	}
	st := s.currentSettings()
	if raw, ok := settings["global"]; ok {
		if err := json.Unmarshal([]byte(raw), &st); err != nil || st.validate() != nil {
			log.Printf("admin: ignoring invalid saved settings: %v", err)
			st = s.currentSettings()
		}
	}
	s.applySettings(st)

	npcs, err := s.Store.AdminConfig("npc")
	if err != nil {
		return fmt.Errorf("load npc overrides: %w", err)
	}
	for id, raw := range npcs {
		var o npcOverride
		if err := json.Unmarshal([]byte(raw), &o); err != nil || o.validate() != nil || !s.applyNPCOverride(id, o) {
			log.Printf("admin: ignoring invalid override for npc %s", id)
		}
	}

	facts, err := s.Store.AdminConfig("facts")
	if err != nil {
		return fmt.Errorf("load fact overrides: %w", err)
	}
	for bank, raw := range facts {
		var list []world.Fact
		if err := json.Unmarshal([]byte(raw), &list); err != nil || validateFacts(bank, list) != nil || !s.World.SetFactBank(bank, list) {
			log.Printf("admin: ignoring invalid override for fact bank %s", bank)
		}
	}
	log.Printf("admin: применено правок — npc: %d, банков фактов: %d, глобальные настройки: %v", len(npcs), len(facts), len(settings) > 0)
	return nil
}

// --- sessions ---

type adminSession struct {
	PlayerID string
	Name     string
	Expires  time.Time
}

const adminSessionTTL = 12 * time.Hour

// AdminNames are accounts granted the admin role via ADMIN_USERS. Set
// before serving; read-only afterwards.
var AdminNames = map[string]bool{}

// GrantConfiguredAdmins marks every ADMIN_USERS account that already
// exists. Accounts registered later get the role in Register.
func (s *Server) GrantConfiguredAdmins() {
	for name := range AdminNames {
		if err := s.Store.SetAdmin(name, true); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				log.Printf("admin: %q ещё не зарегистрирован — получит права при регистрации", name)
				continue
			}
			log.Printf("admin: grant %q: %v", name, err)
		}
	}
}

// AdminLogin is separate from /api/login on purpose: it issues an
// in-memory panel session and leaves the player's game token alone, so the
// same account can play and run the panel at once.
func (s *Server) AdminLogin(w http.ResponseWriter, r *http.Request) {
	if allowed, retryAfter := s.adminLimiter.Allow(clientIP(r)); !allowed {
		writeJSONError(w, http.StatusTooManyRequests, rateLimitMessage(retryAfter))
		return
	}
	req, err := decodeAuthRequest(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	player, err := s.Store.PlayerByName(req.Name)
	if err != nil || bcrypt.CompareHashAndPassword([]byte(player.PasswordHash), []byte(req.Password)) != nil {
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			log.Printf("admin login: lookup error: %v", err)
		}
		writeJSONError(w, http.StatusUnauthorized, "неверное имя или пароль")
		return
	}
	if !player.IsAdmin {
		writeJSONError(w, http.StatusForbidden, "у этого аккаунта нет прав администратора")
		return
	}
	token := randomID()
	s.adminMu.Lock()
	for t, sess := range s.adminSessions {
		if time.Now().After(sess.Expires) {
			delete(s.adminSessions, t)
		}
	}
	s.adminSessions[token] = adminSession{PlayerID: player.ID, Name: player.Name, Expires: time.Now().Add(adminSessionTTL)}
	s.adminMu.Unlock()
	writeJSON(w, map[string]string{"token": token, "name": player.Name})
}

func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		s.adminMu.Lock()
		sess, found := s.adminSessions[token]
		if found && time.Now().After(sess.Expires) {
			delete(s.adminSessions, token)
			found = false
		}
		s.adminMu.Unlock()
		if !ok || !found {
			writeJSONError(w, http.StatusUnauthorized, "нужно войти в панель администратора")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		next(w, r)
	}
}

// RegisterAdminRoutes mounts the panel API. Everything except login needs a
// panel session (Authorization: Bearer <token>).
func (s *Server) RegisterAdminRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/admin/login", s.AdminLogin)
	routes := map[string]http.HandlerFunc{
		"GET /api/admin/metrics":                s.adminMetrics,
		"GET /api/admin/config":                 s.adminConfig,
		"PUT /api/admin/settings":               s.adminPutSettings,
		"DELETE /api/admin/settings":            s.adminResetSettings,
		"POST /api/admin/doors":                 s.adminDoor,
		"POST /api/admin/announce":              s.adminAnnounce,
		"POST /api/admin/players/{id}/teleport": s.adminTeleport,
		"POST /api/admin/players/{id}/kick":     s.adminKick,
		"PUT /api/admin/npcs/{id}":              s.adminPutNPC,
		"DELETE /api/admin/npcs/{id}":           s.adminResetNPC,
		"GET /api/admin/npcs/{id}/memory":       s.adminNPCMemory,
		"DELETE /api/admin/npcs/{id}/memory":    s.adminResetNPCMemory,
		"GET /api/admin/facts/{bank}":           s.adminGetFacts,
		"PUT /api/admin/facts/{bank}":           s.adminPutFacts,
		"DELETE /api/admin/facts/{bank}":        s.adminResetFacts,
	}
	for pattern, h := range routes {
		mux.HandleFunc(pattern, s.requireAdmin(h))
	}
}

// --- helpers ---

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("admin: encode response: %v", err)
	}
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeJSONError(w, http.StatusBadRequest, "некорректный JSON: "+err.Error())
		return false
	}
	return true
}

func (s *Server) persist(w http.ResponseWriter, kind, key string, v any) bool {
	raw, err := json.Marshal(v)
	if err == nil {
		err = s.Store.SetAdminConfig(kind, key, string(raw))
	}
	if err != nil {
		log.Printf("admin: persist %s/%s: %v", kind, key, err)
		writeJSONError(w, http.StatusInternalServerError, "не удалось сохранить изменения")
		return false
	}
	return true
}

func (s *Server) unpersist(w http.ResponseWriter, kind, key string) bool {
	if err := s.Store.DeleteAdminConfig(kind, key); err != nil {
		log.Printf("admin: delete %s/%s: %v", kind, key, err)
		writeJSONError(w, http.StatusInternalServerError, "не удалось сбросить изменения")
		return false
	}
	return true
}

// --- metrics ---

type serverMetrics struct {
	UptimeSec       float64        `json:"uptimeSec"`
	Goroutines      int            `json:"goroutines"`
	HeapMB          float64        `json:"heapMB"`
	SysMB           float64        `json:"sysMB"`
	NumGC           uint32         `json:"numGC"`
	Clients         int            `json:"clients"`
	PlayersPerRoom  map[string]int `json:"playersPerRoom"`
	AvgFPS          float64        `json:"avgFps"`
	MinFPS          float64        `json:"minFps"`
	AvgPingMs       float64        `json:"avgPingMs"`
	MaxPingMs       float64        `json:"maxPingMs"`
	OllamaURL       string         `json:"ollamaUrl"`
	LLMEnabled      bool           `json:"llmEnabled"`
	Model           string         `json:"model"`
	LLM             ai.Stats       `json:"llm"`
	Cleaners        int            `json:"cleaners"`
	CleanersEnabled bool           `json:"cleanersEnabled"`
	ServerTime      time.Time      `json:"serverTime"`
}

func (s *Server) adminMetrics(w http.ResponseWriter, r *http.Request) {
	clients := s.Hub.Snapshot()
	sort.Slice(clients, func(i, j int) bool { return clients[i].Name < clients[j].Name })

	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	llmEnabled, model := s.AI.LLM()
	st := s.currentSettings()
	m := serverMetrics{
		UptimeSec:       time.Since(s.startedAt).Seconds(),
		Goroutines:      runtime.NumGoroutine(),
		HeapMB:          float64(mem.HeapAlloc) / (1 << 20),
		SysMB:           float64(mem.Sys) / (1 << 20),
		NumGC:           mem.NumGC,
		Clients:         len(clients),
		PlayersPerRoom:  map[string]int{},
		OllamaURL:       s.AI.OllamaURL(),
		LLMEnabled:      llmEnabled,
		Model:           model,
		LLM:             s.AI.Stats(),
		Cleaners:        s.cleanerCount(),
		CleanersEnabled: st.CleanersEnabled,
		ServerTime:      time.Now(),
	}
	reporting := 0
	m.MinFPS = math.Inf(1)
	for _, c := range clients {
		m.PlayersPerRoom[c.Room]++
		if c.Stats.LastReport.IsZero() {
			continue
		}
		reporting++
		m.AvgFPS += c.Stats.FPS
		m.AvgPingMs += c.Stats.PingMs
		m.MinFPS = math.Min(m.MinFPS, c.Stats.FPS)
		m.MaxPingMs = math.Max(m.MaxPingMs, c.Stats.PingMs)
	}
	if reporting > 0 {
		m.AvgFPS /= float64(reporting)
		m.AvgPingMs /= float64(reporting)
	} else {
		m.MinFPS = 0
	}
	writeJSON(w, map[string]any{"server": m, "clients": clients, "doors": s.doorList()})
}

// --- config (rooms, npcs, fact banks, settings) ---

type adminNPC struct {
	world.NPC
	RoomName   string     `json:"roomName"`
	Profile    ai.Profile `json:"profile"`
	Defaults   ai.Profile `json:"defaults"`
	Base       world.NPC  `json:"base"`
	Overridden bool       `json:"overridden"`
	// The same character in another room shares dialogue memory (ai.logicalCharacterID).
	SharedMemoryWith []string `json:"sharedMemoryWith"`
}

func (s *Server) adminConfig(w http.ResponseWriter, r *http.Request) {
	overrides, err := s.Store.AdminConfig("npc")
	if err != nil {
		log.Printf("admin config: %v", err)
	}
	factOverrides, err := s.Store.AdminConfig("facts")
	if err != nil {
		log.Printf("admin config: %v", err)
	}

	var npcs []adminNPC
	for _, n := range s.World.NPCList() {
		base, _ := s.World.BaseNPC(n.ID)
		_, overridden := overrides[n.ID]
		npcs = append(npcs, adminNPC{
			NPC: n, RoomName: s.World.Rooms[n.RoomID].Name,
			Profile: s.AI.Profile(n.ID), Defaults: ai.Defaults(base), Base: base,
			Overridden: overridden, SharedMemoryWith: ai.SharedMemoryWith(n.ID),
		})
	}

	type roomInfo struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	rooms := make([]roomInfo, 0, len(s.World.Rooms))
	for id, room := range s.World.Rooms {
		rooms = append(rooms, roomInfo{ID: id, Name: room.Name})
	}
	sort.Slice(rooms, func(i, j int) bool { return rooms[i].ID < rooms[j].ID })

	type bankInfo struct {
		ID         string `json:"id"`
		Count      int    `json:"count"`
		Overridden bool   `json:"overridden"`
	}
	var banks []bankInfo
	for _, id := range s.World.FactBankIDs() {
		facts, _ := s.World.FactBank(id)
		_, overridden := factOverrides[id]
		banks = append(banks, bankInfo{ID: id, Count: len(facts), Overridden: overridden})
	}

	writeJSON(w, map[string]any{
		"settings":  s.currentSettings(),
		"defaults":  defaultSettings(s.baseModel),
		"npcs":      npcs,
		"rooms":     rooms,
		"factBanks": banks,
	})
}

// --- settings ---

func (s *Server) adminPutSettings(w http.ResponseWriter, r *http.Request) {
	var st Settings
	if !readJSON(w, r, &st) {
		return
	}
	if err := st.validate(); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !s.persist(w, "settings", "global", st) {
		return
	}
	s.applySettings(st)
	writeJSON(w, s.currentSettings())
}

func (s *Server) adminResetSettings(w http.ResponseWriter, r *http.Request) {
	if !s.unpersist(w, "settings", "global") {
		return
	}
	s.applySettings(defaultSettings(s.baseModel))
	writeJSON(w, s.currentSettings())
}

// --- live world ---

func (s *Server) adminDoor(w http.ResponseWriter, r *http.Request) {
	var req struct {
		A    string `json:"a"`
		B    string `json:"b"`
		Open bool   `json:"open"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if !s.forceDoor(req.A, req.B, req.Open) {
		writeJSONError(w, http.StatusNotFound, "нет двери между этими комнатами")
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) adminAnnounce(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text string `json:"text"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	text := strings.TrimSpace(req.Text)
	if text == "" || len([]rune(text)) > 300 {
		writeJSONError(w, http.StatusBadRequest, "объявление должно быть от 1 до 300 символов")
		return
	}
	s.Hub.BroadcastAll(map[string]any{"type": "announcement", "text": text})
	writeJSON(w, map[string]int{"recipients": s.Hub.Count()})
}

func (s *Server) adminTeleport(w http.ResponseWriter, r *http.Request) {
	playerID := r.PathValue("id")
	var req struct {
		Room string `json:"room"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	client, ok := s.Hub.Get(playerID)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "игрок не подключён")
		return
	}
	room, exists := s.World.Rooms[req.Room]
	chunk, _ := s.World.Chunk(req.Room)
	if !exists || chunk == nil {
		writeJSONError(w, http.StatusBadRequest, "нет такой комнаты")
		return
	}
	pos := world.Vec3{X: room.Position.X, Y: room.Position.Y + 1.6, Z: room.Position.Z}
	if room.Spawn != nil {
		pos = world.Vec3{X: room.Position.X + room.Spawn.X, Y: room.Position.Y + room.Spawn.Y, Z: room.Position.Z + room.Spawn.Z}
	}

	s.facility.mu.Lock()
	defer s.facility.mu.Unlock()
	oldRoom := s.Hub.RoomOf(playerID)
	delete(s.facility.conversations, playerID) // same as a normal room change
	s.Hub.UpdatePosition(playerID, req.Room, pos.X, pos.Y, pos.Z)
	if oldRoom != req.Room {
		s.closeRoomDoors(oldRoom)
	}
	if err := s.Store.SavePosition(playerID, req.Room, pos.X, pos.Y, pos.Z); err != nil {
		log.Printf("admin teleport: save %s: %v", playerID, err)
	}
	client.Send(map[string]any{
		"type":     "chunk_data",
		"chunk":    chunk,
		"state":    s.facilitySnapshot(req.Room),
		"players":  s.Hub.PlayersInRoom(req.Room, playerID),
		"position": pos,
	})
	s.Hub.BroadcastToRoom(oldRoom, playerID, map[string]any{"type": "player_left", "playerId": playerID})
	s.Hub.BroadcastToRoom(req.Room, playerID, map[string]any{
		"type": "player_joined", "playerId": playerID, "name": client.Name,
		"x": pos.X, "y": pos.Y, "z": pos.Z,
	})
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) adminKick(w http.ResponseWriter, r *http.Request) {
	client, ok := s.Hub.Get(r.PathValue("id"))
	if !ok {
		writeJSONError(w, http.StatusNotFound, "игрок не подключён")
		return
	}
	kickClient(client)
	writeJSON(w, map[string]bool{"ok": true})
}

// kickClient tells the client why, then closes the socket a moment later so
// the message has a chance to go out first. The read loop then runs the
// normal disconnect cleanup.
func kickClient(c *hub.Client) {
	c.Send(map[string]any{"type": "error", "code": "kicked", "message": "Администратор отключил тебя от станции."})
	time.AfterFunc(300*time.Millisecond, func() { c.Conn.Close() })
}

// --- NPC agents ---

func (s *Server) adminPutNPC(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.World.BaseNPC(id); !ok {
		writeJSONError(w, http.StatusNotFound, "нет такого персонажа")
		return
	}
	var o npcOverride
	if !readJSON(w, r, &o) {
		return
	}
	if err := o.validate(); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !s.persist(w, "npc", id, o) {
		return
	}
	s.applyNPCOverride(id, o)
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) adminResetNPC(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	base, ok := s.World.BaseNPC(id)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "нет такого персонажа")
		return
	}
	if !s.unpersist(w, "npc", id) {
		return
	}
	s.World.SetNPC(base)
	s.AI.ClearProfile(id)
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) adminNPCMemory(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.World.NPC(id); !ok {
		writeJSONError(w, http.StatusNotFound, "нет такого персонажа")
		return
	}
	names, err := s.Store.PlayerNames()
	if err != nil {
		log.Printf("admin memory: names: %v", err)
	}
	counts, err := s.Store.ToldFactCounts(id)
	if err != nil {
		log.Printf("admin memory: counts: %v", err)
	}

	type row struct {
		PlayerID  string              `json:"playerId"`
		Name      string              `json:"name"`
		ToldFacts int                 `json:"toldFacts"`
		Lines     []ai.TranscriptLine `json:"lines"`
	}
	byPlayer := map[string]*row{}
	for _, c := range s.AI.Conversations(id) {
		byPlayer[c.PlayerID] = &row{PlayerID: c.PlayerID, Name: names[c.PlayerID], Lines: c.Lines}
	}
	for playerID, n := range counts {
		if byPlayer[playerID] == nil {
			byPlayer[playerID] = &row{PlayerID: playerID, Name: names[playerID], Lines: []ai.TranscriptLine{}}
		}
		byPlayer[playerID].ToldFacts = n
	}
	out := make([]row, 0, len(byPlayer))
	for _, r := range byPlayer {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, out)
}

// adminResetNPCMemory makes the NPC forget a player (?player=id) or
// everyone: dialogue context, transcript, FSM state and told facts.
func (s *Server) adminResetNPCMemory(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.World.NPC(id); !ok {
		writeJSONError(w, http.StatusNotFound, "нет такого персонажа")
		return
	}
	playerID := r.URL.Query().Get("player")
	s.AI.ResetMemory(id, playerID)
	if err := s.Store.ResetToldFacts(id, playerID); err != nil {
		log.Printf("admin memory reset %s: %v", id, err)
		writeJSONError(w, http.StatusInternalServerError, "не удалось сбросить память фактов")
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// --- facts ---

func (s *Server) adminGetFacts(w http.ResponseWriter, r *http.Request) {
	bank := r.PathValue("bank")
	facts, ok := s.World.FactBank(bank)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "нет такого банка фактов")
		return
	}
	writeJSON(w, facts)
}

func (s *Server) adminPutFacts(w http.ResponseWriter, r *http.Request) {
	bank := r.PathValue("bank")
	if _, ok := s.World.FactBank(bank); !ok {
		writeJSONError(w, http.StatusNotFound, "нет такого банка фактов")
		return
	}
	var facts []world.Fact
	if !readJSON(w, r, &facts) {
		return
	}
	for i := range facts {
		facts[i].Text = strings.TrimSpace(facts[i].Text)
		facts[i].Topic = strings.TrimSpace(facts[i].Topic)
		if facts[i].ID == "" {
			facts[i].ID = bank + "_" + randomID()[:8]
		}
	}
	if err := validateFacts(bank, facts); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !s.persist(w, "facts", bank, facts) {
		return
	}
	s.World.SetFactBank(bank, facts)
	writeJSON(w, facts)
}

func (s *Server) adminResetFacts(w http.ResponseWriter, r *http.Request) {
	bank := r.PathValue("bank")
	base, ok := s.World.BaseFactBank(bank)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "нет такого банка фактов")
		return
	}
	if !s.unpersist(w, "facts", bank) {
		return
	}
	s.World.SetFactBank(bank, base)
	writeJSON(w, base)
}
