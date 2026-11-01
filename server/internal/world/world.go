// Package world loads the static station data (rooms, NPCs, facts) from
// JSON files and answers the two questions the rest of the server needs:
// "what does this room look like" (Chunk) and "what should this NPC say
// next to this specific player" (PickFact).
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

type Size3 struct {
	W float64 `json:"w"`
	H float64 `json:"h"`
	D float64 `json:"d"`
}

type Room struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Position   Vec3     `json:"position"`
	Size       Size3    `json:"size"`
	Color      string   `json:"color"`
	NpcID      *string  `json:"npcId"`
	ConnectsTo []string `json:"connectsTo"`
	// Model is an optional URL (served by the client's static files) to a
	// .glb scene that replaces the procedural box geometry for this room.
	Model      string  `json:"model,omitempty"`
	ModelScale float64 `json:"modelScale,omitempty"`
	Radius     float64 `json:"radius,omitempty"`
	Spawn      *Vec3   `json:"spawn,omitempty"`
	Doors      []Door  `json:"doors,omitempty"`
	// Walkmesh enables client-side floors, stairs and wall collision from GLB.
	Collision string `json:"collision,omitempty"`
}

// Door positions are local to the room, matching its authored GLB coordinates.
type Door struct {
	Room      string  `json:"room"`
	Position  Vec3    `json:"position"`
	HalfWidth float64 `json:"halfWidth"`
	ModelDoor string  `json:"modelDoor,omitempty"`
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

type World struct {
	Rooms map[string]Room
	NPCs  map[string]NPC
	Facts map[string][]Fact // keyed by NPC factBank id
}

func Load(dir string) (*World, error) {
	w := &World{}

	if err := loadJSON(filepath.Join(dir, "rooms.json"), &w.Rooms); err != nil {
		return nil, fmt.Errorf("rooms.json: %w", err)
	}
	if err := loadJSON(filepath.Join(dir, "npcs.json"), &w.NPCs); err != nil {
		return nil, fmt.Errorf("npcs.json: %w", err)
	}
	if err := loadJSON(filepath.Join(dir, "facts.json"), &w.Facts); err != nil {
		return nil, fmt.Errorf("facts.json: %w", err)
	}

	return w, nil
}

func loadJSON(path string, out any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

type NPCStub struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Title    string `json:"title"`
	Position Vec3   `json:"position"`
	Model    string `json:"model,omitempty"`
}

type RoomStub struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Position Vec3   `json:"position"`
	Size     Size3  `json:"size"`
}

type ChunkData struct {
	Room      Room       `json:"room"`
	NPCs      []NPCStub  `json:"npcs"`
	Neighbors []RoomStub `json:"neighbors"`
}

func (w *World) Chunk(roomID string) (*ChunkData, bool) {
	room, ok := w.Rooms[roomID]
	if !ok {
		return nil, false
	}

	var npcs []NPCStub
	for _, n := range w.NPCs {
		if n.RoomID == roomID {
			npcs = append(npcs, NPCStub{ID: n.ID, Name: n.Name, Title: n.Title, Position: n.Position, Model: n.Model})
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
// Keyword-based topic matching
// ---------------------------------------------------------------------------

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
	"chemistry":  {"хими", "элемент", "веществ", "реакци", "молекул", "таблиц менделеев"},
	"radio":      {"радио", "связь", "сигнал", "антенн", "передач", "волн"},
	"library":    {"книг", "библиотек", "архив", "знани", "журнал", "коллекци"},
	"suits":      {"скафандр", "герметичн", "шлем", "выход в открытый космос", "костюм"},
}

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
// If the player asks small-talk or an off-topic question,
// it returns fact = nil so the NPC can respond conversationally or decline off-topic.
func (w *World) PickFact(npcID string, message string, told map[string]bool) (fact *Fact, isRepeat bool, availableTopics []string) {
	npc, ok := w.NPCs[npcID]
	if !ok {
		return nil, false, nil
	}
	bank := w.Facts[npc.FactBank]
	if len(bank) == 0 {
		return nil, false, nil
	}

	npcTopics := map[string]bool{}
	for _, f := range bank {
		npcTopics[f.Topic] = true
	}

	topicSet := map[string]bool{}
	for _, f := range bank {
		if !told[f.ID] {
			topicSet[f.Topic] = true
		}
	}
	for t := range topicSet {
		availableTopics = append(availableTopics, t)
	}

	matchedTopics := matchTopics(message)

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

	// 2. Topic-matched but already told
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
		return &bank[len(bank)-1], true, availableTopics
	}

	// 4. Return nil for conversational speech or off-topic questions
	return nil, false, availableTopics
}
