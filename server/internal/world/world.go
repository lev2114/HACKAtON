// Package world loads rooms, NPCs, and facts from disk, and serves them
// as chunks to the client. The client is only allowed to cache the chunk
// for the room the player is currently in (≤10% of total world data).
package world

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Vec3 struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	Z float64 `json:"z"`
}

type Size struct {
	W float64 `json:"w"`
	H float64 `json:"h"`
	D float64 `json:"d"`
}

type Doorway struct {
	Room      string  `json:"room"`
	Position  Vec3    `json:"position"`
	HalfWidth float64 `json:"halfWidth"`
	ModelDoor string  `json:"modelDoor,omitempty"`
}

type Room struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Position   Vec3      `json:"position"`
	Size       Size      `json:"size"`
	Color      string    `json:"color"`
	Model      string    `json:"model,omitempty"`
	ModelScale float64   `json:"modelScale,omitempty"`
	Radius     float64   `json:"radius,omitempty"`
	Spawn      *Vec3     `json:"spawn,omitempty"`
	Doors      []Doorway `json:"doors,omitempty"`
	NPCID      string    `json:"npcId"`
	ConnectsTo []string  `json:"connectsTo"`
}

type NPC struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	RoomID      string `json:"roomId"`
	Title       string `json:"title"`
	Personality string `json:"personality"`
	Position    Vec3   `json:"position"`
	FactBank    string `json:"factBank"`
	Model       string `json:"model,omitempty"`
}

type Fact struct {
	ID         string `json:"id"`
	Topic      string `json:"topic"`
	Difficulty string `json:"difficulty"`
	Text       string `json:"text"`
}

// World holds the static dataset loaded once at server startup.
type World struct {
	Rooms map[string]Room
	NPCs  map[string]NPC
	Facts map[string][]Fact // npcId -> facts
}

func Load(dir string) (*World, error) {
	roomsPath := filepath.Join(dir, "rooms.json")
	npcsPath := filepath.Join(dir, "npcs.json")
	factsPath := filepath.Join(dir, "facts.json")

	rooms, err := loadJSON[map[string]Room](roomsPath)
	if err != nil {
		return nil, fmt.Errorf("rooms: %w", err)
	}

	npcs, err := loadJSON[map[string]NPC](npcsPath)
	if err != nil {
		return nil, fmt.Errorf("npcs: %w", err)
	}

	facts, err := loadJSON[map[string][]Fact](factsPath)
	if err != nil {
		return nil, fmt.Errorf("facts: %w", err)
	}

	return &World{Rooms: rooms, NPCs: npcs, Facts: facts}, nil
}

func loadJSON[T any](path string) (T, error) {
	var zero T
	data, err := os.ReadFile(path)
	if err != nil {
		return zero, err
	}
	var out T
	if err := json.Unmarshal(data, &out); err != nil {
		return zero, err
	}
	return out, nil
}

// ChunkData is what gets sent to the client when entering a room.
// Note: facts are deliberately excluded — they are sent strictly on demand
// via npc_talk to enforce the ≤10% client cache limit.
type ChunkData struct {
	Room      Room       `json:"room"`
	NPCs      []NPCStub  `json:"npcs"`
	Neighbors []RoomStub `json:"neighbors"`
}

// RoomStub gives neighboring rooms' bounds for client-side wall collision
// without leaking their NPCs or facts.
type RoomStub struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Position Vec3   `json:"position"`
	Size     Size   `json:"size"`
}

type NPCStub struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Title    string `json:"title"`
	Position Vec3   `json:"position"`
	Model    string `json:"model,omitempty"`
}

// Chunk returns the minimal data needed to render roomID and talk to its NPC.
func (w *World) Chunk(roomID string) (*ChunkData, bool) {
	room, ok := w.Rooms[roomID]
	if !ok {
		return nil, false
	}

	var npcs []NPCStub
	for _, npc := range w.NPCs {
		if npc.RoomID == roomID {
			npcs = append(npcs, NPCStub{
				ID:       npc.ID,
				Name:     npc.Name,
				Title:    npc.Title,
				Position: npc.Position,
				Model:    npc.Model,
			})
		}
	}

	var neighbors []RoomStub
	for _, id := range room.ConnectsTo {
		nb, ok := w.Rooms[id]
		if !ok {
			continue
		}
		neighbors = append(neighbors, RoomStub{ID: nb.ID, Name: nb.Name, Position: nb.Position, Size: nb.Size})
	}

	return &ChunkData{Room: room, NPCs: npcs, Neighbors: neighbors}, true
}

// ---------------------------------------------------------------------------
// Keyword-based topic matching (FSM brain)
// ---------------------------------------------------------------------------

// TopicKeywords maps Russian keywords the player might type to fact topic IDs.
// Simple substring matching — no ML, works offline, fast.
var TopicKeywords = map[string][]string{
	"stars":      {"звезд", "звёзд", "солнц", "полярн", "светил"},
	"blackholes": {"чёрн", "черн", "дыр", "горизонт событ", "сингуляр"},
	"galaxies":   {"галакти", "млечн", "путь", "андромед"},
	"cosmology":  {"вселен", "расширен", "хаббл", "большой взрыв", "big bang"},
	"gravity":    {"гравит", "невесом", "притяжен"},
	"newton":     {"ньютон", "третий закон"},
	"inertia":    {"инерц"},
	"spin":       {"вращен", "момент импульс", "закрутит"},
	"rockets":    {"ракет", "реактивн", "дюз"},
	"history":    {"гагарин", "спутник-1", "спутник 1", "первый космонавт", "1957", "1961"},
	"staging":    {"ступен", "многоступ", "сброс бак"},
	"reuse":      {"многоразов", "посадк ракет", "возврат ракет"},
	"body":       {"тело в космосе", "лицо", "отёк", "отек", "кости в космосе", "мышцы в космосе"},
	"plants":     {"растен", "оранжере", "гидропоник", "выращив"},
	"food":       {"тюбик", "сублимат", "космическ ед", "еда космонавт", "крошк"},
	"growth":     {"позвоноч", "подраст", "вырасти в космосе", "рост в невесом"},
	"adaptation": {"адаптац", "болезнь движения", "укачив"},
	"radiation":  {"радиац", "облучен"},
	"sleep":      {"сон в космосе", "спать в космосе", "режим сна", "сутки на орбит"},
	"return":     {"реабилитац", "возвращен на земл", "после полёт", "после полет"},
	"psychology": {"психолог", "изоляц", "экипаж"},
}

// matchTopics extracts topic IDs from a player's message by simple keyword
// substring matching. Returns matched topics in order found.
func matchTopics(message string) []string {
	lower := strings.ToLower(message)
	var matched []string
	seen := map[string]bool{}
	for topic, keywords := range TopicKeywords {
		for _, kw := range keywords {
			if strings.Contains(lower, kw) && !seen[topic] {
				matched = append(matched, topic)
				seen[topic] = true
				break
			}
		}
	}
	return matched
}

// isGeneralFactRequest checks if the player explicitly asked for a fact or general space info
func isGeneralFactRequest(message string) bool {
	lower := strings.ToLower(strings.TrimSpace(message))
	triggers := []string{
		"расскажи что-нибудь",
		"расскажи что нибудь",
		"расскажи что-то",
		"расскажи что то",
		"расскажи факт",
		"какой-нибудь факт",
		"какой нибудь факт",
		"что интересного",
		"что знаешь",
		"поделись фактом",
		"интересный факт",
		"удиви меня",
		"о космосе",
		"про космос",
		"ещё факт",
		"еще факт",
		"следующий факт",
	}
	for _, t := range triggers {
		if strings.Contains(lower, t) {
			return true
		}
	}
	return false
}

// PickFact selects the best fact for an NPC to tell this player.
// It returns a fact ONLY if:
//  1. The player's question matches a topic that THIS NPC specializes in.
//  2. The player explicitly asks to tell a fact or something interesting.
//
// If the player asks small-talk ("как дела?") or an off-topic question ("как испечь блинчики?"),
// it returns fact = nil so the NPC can respond conversationally or decline off-topic.
func (w *World) PickFact(npcID string, message string, told map[string]bool) (fact *Fact, isRepeat bool, availableTopics []string) {
	bank := w.Facts[npcID]
	if len(bank) == 0 {
		return nil, false, nil
	}

	// Topics of THIS NPC
	npcTopics := map[string]bool{}
	for _, f := range bank {
		npcTopics[f.Topic] = true
	}

	// Collect topics that still have untold facts for this NPC
	topicSet := map[string]bool{}
	for _, f := range bank {
		if !told[f.ID] {
			topicSet[f.Topic] = true
		}
	}
	for t := range topicSet {
		availableTopics = append(availableTopics, t)
	}

	// Try to match player's question to a topic
	matchedTopics := matchTopics(message)

	// Keep only topics this NPC actually specializes in
	var relevantTopics []string
	for _, t := range matchedTopics {
		if npcTopics[t] {
			relevantTopics = append(relevantTopics, t)
		}
	}

	// 1. Topic-matched untold fact for this NPC
	for _, topic := range relevantTopics {
		for i := range bank {
			if bank[i].Topic == topic && !told[bank[i].ID] {
				return &bank[i], false, availableTopics
			}
		}
	}

	// 2. Topic-matched but already told (repeat, but relevant to the question)
	for _, topic := range relevantTopics {
		for i := range bank {
			if bank[i].Topic == topic {
				return &bank[i], true, availableTopics
			}
		}
	}

	// 3. Explicit request for a general fact
	if isGeneralFactRequest(message) {
		for i := range bank {
			if !told[bank[i].ID] {
				return &bank[i], false, availableTopics
			}
		}
		// All told — repeat last fact
		return &bank[len(bank)-1], true, availableTopics
	}

	// 4. Neither topic match nor general fact request:
	// Return nil so the NPC can answer small talk or decline off-topic questions.
	return nil, false, availableTopics
}
