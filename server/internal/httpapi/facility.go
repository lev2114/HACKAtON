package httpapi

import (
	"context"
	"math"
	"math/rand/v2"
	"sync"
	"time"

	"server/internal/hub"
	"server/internal/store"
	"server/internal/world"
)

type doorState struct {
	Open         bool
	Opened       time.Time
	ClosePending bool
}
type doorPose struct {
	Room string `json:"room"`
	Open bool   `json:"open"`
}
type roombaPose struct {
	ID      int     `json:"id"`
	X       float64 `json:"x"`
	Y       float64 `json:"y"`
	Z       float64 `json:"z"`
	Heading float64 `json:"heading"`
}
type cleaner struct {
	roombaPose
	node, target, previous int
	pause                  float64
}
type roomState struct {
	Room     string       `json:"room"`
	Revision uint64       `json:"revision"`
	Doors    []doorPose   `json:"doors"`
	Roombas  []roombaPose `json:"roombas"`
	NPCs     []npcPose    `json:"npcs"`
}
type facilityState struct {
	mu            sync.Mutex
	revision      uint64
	doors         map[string]*doorState
	cleaners      map[string][]*cleaner
	npcs          map[string][]*wanderingNPC
	conversations map[string]npcConversation
}

func doorKey(a, b string) string {
	if a > b {
		a, b = b, a
	}
	return a + ":" + b
}
func newFacility(w *world.World) *facilityState {
	f := &facilityState{doors: map[string]*doorState{}, cleaners: map[string][]*cleaner{}, npcs: map[string][]*wanderingNPC{}, conversations: map[string]npcConversation{}}
	for id, room := range w.Rooms {
		for _, d := range room.Doors {
			f.doors[doorKey(id, d.Room)] = &doorState{}
		}
		nav := w.Navigation[id]
		candidates := []int{}
		for i, links := range nav.Links {
			if len(links) > 0 {
				candidates = append(candidates, i)
			}
		}
		rand.Shuffle(len(candidates), func(i, j int) { candidates[i], candidates[j] = candidates[j], candidates[i] })
		for _, i := range candidates {
			p := nav.Points[i]
			clear := true
			for _, r := range f.cleaners[id] {
				if math.Hypot(r.X-p.X, r.Z-p.Z) < 0.7 {
					clear = false
				}
			}
			if !clear {
				continue
			}
			f.cleaners[id] = append(f.cleaners[id], &cleaner{roombaPose: roombaPose{ID: len(f.cleaners[id]), X: p.X, Y: p.Y + 0.01, Z: p.Z, Heading: rand.Float64() * 2 * math.Pi}, node: i, target: i, previous: -1})
			if len(f.cleaners[id]) == 2 {
				break
			}
		}
	}
	for _, npc := range w.NPCs {
		f.npcs[npc.RoomID] = append(f.npcs[npc.RoomID], newWanderingNPC(npc, w.NPCNavigation[npc.RoomID]))
	}
	return f
}

// Call with facility.mu held; all returned actor data are value copies.
func (s *Server) facilitySnapshot(room string) roomState {
	state := roomState{Room: room, Revision: s.facility.revision, Doors: []doorPose{}, Roombas: []roombaPose{}, NPCs: []npcPose{}}
	for _, d := range s.World.Rooms[room].Doors {
		state.Doors = append(state.Doors, doorPose{d.Room, s.facility.doors[doorKey(room, d.Room)].Open})
	}
	for _, r := range s.facility.cleaners[room] {
		state.Roombas = append(state.Roombas, r.roombaPose)
	}
	for _, npc := range s.facility.npcs[room] {
		state.NPCs = append(state.NPCs, npc.npcPose)
	}
	return state
}
func (s *Server) broadcastFacility(room string) {
	s.Hub.BroadcastToRoom(room, "", map[string]any{"type": "room_state", "state": s.facilitySnapshot(room)})
}
func (s *Server) doorwayOccupied(a, b string) bool {
	for _, id := range []string{a, b} {
		room := s.World.Rooms[id]
		for _, d := range room.Doors {
			if d.Room != a && d.Room != b {
				continue
			}
			length := math.Hypot(d.Position.X, d.Position.Z)
			for _, p := range s.Hub.PlayersInRoom(id, "") {
				x, z := p.X-room.Position.X-d.Position.X, p.Z-room.Position.Z-d.Position.Z
				if math.Abs(p.Y-room.Position.Y-d.Position.Y-1.6) < 0.75 && math.Abs((x*d.Position.X+z*d.Position.Z)/length) < 0.7 && math.Abs((x*d.Position.Z-z*d.Position.X)/length) < d.HalfWidth+0.3 {
					return true
				}
			}
		}
	}
	return false
}
func (s *Server) closeRoomDoors(room string) {
	for _, d := range s.World.Rooms[room].Doors {
		door := s.facility.doors[doorKey(room, d.Room)]
		if !door.Open {
			continue
		}
		door.ClosePending = true
		if s.doorwayOccupied(room, d.Room) {
			continue
		}
		door.Open = false
		door.ClosePending = false
		s.facility.revision++
		s.broadcastFacility(room)
		s.broadcastFacility(d.Room)
	}
}
func (s *Server) onDoorSet(client *hub.Client, player *store.Player, msg incoming) {
	s.facility.mu.Lock()
	defer s.facility.mu.Unlock()
	roomID := s.Hub.RoomOf(player.ID)
	room := s.World.Rooms[roomID]
	if msg.Room != roomID {
		return
	}
	for _, d := range room.Doors {
		if d.Room != msg.Door {
			continue
		}
		x, z := player.X-room.Position.X-d.Position.X, player.Z-room.Position.Z-d.Position.Z
		if math.Hypot(x, z) > 2.75 || math.Abs(player.Y-room.Position.Y-d.Position.Y-1.6) > 0.75 {
			return
		}
		if !msg.Open && s.doorwayOccupied(roomID, d.Room) {
			client.Send(map[string]any{"type": "error", "code": "door_occupied", "message": "Проём занят игроком"})
			return
		}
		door := s.facility.doors[doorKey(roomID, d.Room)]
		if door.Open != msg.Open {
			door.Open = msg.Open
			door.Opened = time.Now()
			s.facility.revision++
		}
		door.ClosePending = false
		s.broadcastFacility(roomID)
		s.broadcastFacility(d.Room)
		return
	}
}
func (s *Server) canUseDoor(roomID, target string, p *store.Player) bool {
	room := s.World.Rooms[roomID]
	for _, d := range room.Doors {
		if d.Room != target {
			continue
		}
		state := s.facility.doors[doorKey(roomID, target)]
		if state == nil || !state.Open || time.Since(state.Opened) < 350*time.Millisecond {
			return false
		}
		length := math.Hypot(d.Position.X, d.Position.Z)
		x, z := p.X-room.Position.X-d.Position.X, p.Z-room.Position.Z-d.Position.Z
		along := (x*d.Position.X + z*d.Position.Z) / length
		across := (x*d.Position.Z - z*d.Position.X) / length
		return along >= 1.15 && along <= 3.4 && math.Abs(across) <= d.HalfWidth+0.15 && math.Abs(p.Y-room.Position.Y-d.Position.Y-1.6) < 0.75
	}
	return false
}
func (s *Server) RunFacility(ctx context.Context) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.facility.mu.Lock()
			s.stepNPCs(time.Now())
			for roomID, robots := range s.facility.cleaners {
				nav := s.World.Navigation[roomID]
				players := s.Hub.PlayersInRoom(roomID, "")
				for _, r := range robots {
					if r.pause > 0 {
						r.pause -= 0.1
						continue
					}
					if r.node == r.target {
						links := nav.Links[r.node]
						if len(links) == 0 {
							continue
						}
						next := links[rand.IntN(len(links))]
						if len(links) > 1 && next == r.previous {
							for _, n := range links {
								if n != r.previous {
									next = n
									break
								}
							}
						}
						r.target = next
						if rand.Float64() < 0.08 {
							r.pause = 0.4 + rand.Float64()
							continue
						}
					}
					p := nav.Points[r.target]
					dx, dz := p.X-r.X, p.Z-r.Z
					distance := math.Hypot(dx, dz)
					angle := math.Atan2(dx, dz)
					diff := math.Atan2(math.Sin(angle-r.Heading), math.Cos(angle-r.Heading))
					r.Heading += math.Max(-0.22, math.Min(0.22, diff))
					if math.Abs(diff) > 0.4 {
						continue
					}
					step := math.Min(distance, 0.038)
					if distance < 1e-6 {
						r.previous = r.node
						r.node = r.target
						continue
					}
					x, z := r.X+dx/distance*step, r.Z+dz/distance*step
					blocked := false
					for _, other := range robots {
						if other != r && math.Hypot(other.X-x, other.Z-z) < 0.6 {
							blocked = true
						}
					}
					for _, player := range players {
						if math.Abs(player.Y-r.Y-1.6) < 0.5 && math.Hypot(player.X-x, player.Z-z) < 0.65 {
							blocked = true
						}
					}
					for _, npc := range s.facility.npcs[roomID] {
						if math.Hypot(npc.X-x, npc.Z-z) < 0.8 {
							blocked = true
						}
					}
					if blocked {
						r.target, r.node = r.node, r.target
						r.pause = 0.3
						continue
					}
					r.X, r.Z, r.Y = x, z, p.Y+0.01
					if step >= distance {
						r.previous = r.node
						r.node = r.target
					}
				}
			}
			for roomID, room := range s.World.Rooms {
				for _, d := range room.Doors {
					state := s.facility.doors[doorKey(roomID, d.Room)]
					if state.ClosePending && !s.doorwayOccupied(roomID, d.Room) {
						state.Open = false
						state.ClosePending = false
					}
				}
			}
			s.facility.revision++
			for room := range s.World.Rooms {
				if len(s.Hub.PlayersInRoom(room, "")) > 0 {
					s.broadcastFacility(room)
				}
			}
			s.facility.mu.Unlock()
		}
	}
}

func (s *Server) transitionError(client *hub.Client) {
	client.Send(map[string]any{"type": "error", "code": "door_closed", "message": "Открой дверь и дойди до середины коридора"})
}
