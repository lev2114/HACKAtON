// Package world loads the static station data (rooms, NPCs, facts) from
// JSON files and answers the two questions the rest of the server needs:
// "what does this room look like" (Chunk) and "what should this NPC say
// next to this specific player" (NextUntoldFact).
package world

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	// Empty string = client falls back to the procedural box+floor.
	Model string `json:"model,omitempty"`
}

type NPC struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	RoomID      string `json:"roomId"`
	Title       string `json:"title"`
	Personality string `json:"personality"`
	Position    Vec3   `json:"position"`
	FactBank    string `json:"factBank"`
}

type Fact struct {
	ID         string `json:"id"`
	Topic      string `json:"topic"`
	Difficulty string `json:"difficulty"`
	Text       string `json:"text"`
}

// World holds the full station dataset. In the "no more than 10% cached on
// client" rule, THIS is the 100%: clients only ever see one Room (via Chunk)
// at a time and individual Facts one at a time (via NextUntoldFact), never
// the full Facts map.
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

// NPCStub is the client-visible, trimmed-down view of an NPC: enough to
// render a label and a body, nothing about what it knows.
type NPCStub struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Title    string `json:"title"`
	Position Vec3   `json:"position"`
}

// RoomStub is the minimal shape of a *neighbouring* room: just enough for
// the client to know where a connected room sits and how big it is, so it
// can open a doorway-shaped gap in the collision and auto-load that room
// when the player walks into it. No NPCs, no facts — a neighbour is a
// signpost, not room content.
type RoomStub struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Position Vec3   `json:"position"`
	Size     Size3  `json:"size"`
}

// ChunkData is exactly what gets sent to a client for one room. This is the
// unit of client-side caching: a client holds at most its current room (+
// its immediate neighbours' footprints), never the rest of the station and
// never any Facts.
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
			npcs = append(npcs, NPCStub{ID: n.ID, Name: n.Name, Title: n.Title, Position: n.Position})
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

// NextUntoldFact picks the next fact an NPC hasn't told this player yet.
// If every fact has already been told, it returns the last one again so the
// NPC has something to say, and the caller can flag it as a repeat.
func (w *World) NextUntoldFact(npcID string, told map[string]bool) (*Fact, bool) {
	bank := w.Facts[npcID]
	if len(bank) == 0 {
		return nil, false
	}
	for _, f := range bank {
		if !told[f.ID] {
			return &f, true
		}
	}
	return &bank[len(bank)-1], true
}
