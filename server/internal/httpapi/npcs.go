package httpapi

import (
	"math"
	"math/rand/v2"
	"time"

	"server/internal/hub"
	"server/internal/store"
	"server/internal/world"
)

type npcPose struct {
	ID        string  `json:"id"`
	X         float64 `json:"x"`
	Y         float64 `json:"y"`
	Z         float64 `json:"z"`
	Heading   float64 `json:"heading"`
	TalkingTo string  `json:"talkingTo"`
}

type wanderingNPC struct {
	npcPose
	node, target int
	route        []int
	pause        float64
	blockedFor   float64
}

type npcConversation struct {
	npcID   string
	expires time.Time
}

func newWanderingNPC(npc world.NPC, nav world.RoombaNavigation) *wanderingNPC {
	closest, distance := 0, math.Inf(1)
	for i, point := range nav.Points {
		if len(nav.Links[i]) == 0 {
			continue
		}
		if d := math.Hypot(point.X-npc.Position.X, point.Z-npc.Position.Z); d < distance {
			closest, distance = i, d
		}
	}
	point := nav.Points[closest]
	return &wanderingNPC{
		npcPose: npcPose{ID: npc.ID, X: point.X, Y: point.Y + 1, Z: point.Z, Heading: rand.Float64() * 2 * math.Pi},
		node:    closest, target: closest, pause: 1 + rand.Float64()*2,
	}
}

// Pick a reachable destination a few metres away, then follow its connected
// graph edges. Every edge was sampled against full-height GLB collision offline.
func npcRoute(nav world.RoombaNavigation, start int) []int {
	previous := make([]int, len(nav.Points))
	for i := range previous {
		previous[i] = -1
	}
	previous[start] = start
	queue, candidates := []int{start}, []int{}
	for head := 0; head < len(queue); head++ {
		i := queue[head]
		for _, next := range nav.Links[i] {
			if previous[next] != -1 {
				continue
			}
			previous[next] = i
			queue = append(queue, next)
			distance := math.Hypot(nav.Points[next].X-nav.Points[start].X, nav.Points[next].Z-nav.Points[start].Z)
			if distance >= 2 && distance <= 7 {
				candidates = append(candidates, next)
			}
		}
	}
	if len(candidates) == 0 {
		candidates = queue[1:]
	}
	if len(candidates) == 0 {
		return nil
	}
	goal := candidates[rand.IntN(len(candidates))]
	path := []int{}
	for goal != start {
		path = append(path, goal)
		goal = previous[goal]
	}
	for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
		path[i], path[j] = path[j], path[i]
	}
	return path
}

func turnNPC(npc *wanderingNPC, heading float64) float64 {
	difference := math.Atan2(math.Sin(heading-npc.Heading), math.Cos(heading-npc.Heading))
	npc.Heading += math.Max(-0.22, math.Min(0.22, difference))
	return difference
}

func nearNPC(npc *wanderingNPC, x, y, z float64) bool {
	return math.Abs(y-npc.Y-0.6) < 0.75 && math.Hypot(x-npc.X, z-npc.Z) <= 3.4
}

// All actor and conversation access holds facility.mu. Positions come from the
// Hub's own locked snapshot, never from another socket's mutable store.Player.
func (s *Server) stepNPCs(now time.Time) {
	for playerID, conversation := range s.facility.conversations {
		npc, _ := s.World.NPC(conversation.npcID)
		if !now.Before(conversation.expires) || s.Hub.RoomOf(playerID) != npc.RoomID {
			delete(s.facility.conversations, playerID)
		}
	}
	for roomID, actors := range s.facility.npcs {
		nav := s.World.NPCNavigation[roomID]
		players := s.Hub.PlayersInRoom(roomID, "")
		for _, npc := range actors {
			focus := ""
			var targetPlayer hub.PlayerInfo
			for _, player := range players {
				conversation, active := s.facility.conversations[player.PlayerID]
				if !active || conversation.npcID != npc.ID {
					continue
				}
				if !nearNPC(npc, player.X, player.Y, player.Z) {
					delete(s.facility.conversations, player.PlayerID)
					continue
				}
				if focus == "" || player.PlayerID == npc.TalkingTo || (focus != npc.TalkingTo && player.PlayerID < focus) {
					focus, targetPlayer = player.PlayerID, player
				}
			}
			npc.TalkingTo = focus
			if focus != "" {
				turnNPC(npc, math.Atan2(targetPlayer.X-npc.X, targetPlayer.Z-npc.Z))
				continue
			}
			if npc.pause > 0 {
				npc.pause -= 0.1
				continue
			}
			if npc.node == npc.target {
				if len(npc.route) == 0 {
					npc.route = npcRoute(nav, npc.node)
				}
				if len(npc.route) == 0 {
					continue
				}
				npc.target, npc.route = npc.route[0], npc.route[1:]
			}
			point := nav.Points[npc.target]
			dx, dz := point.X-npc.X, point.Z-npc.Z
			distance := math.Hypot(dx, dz)
			if distance < 1e-6 {
				npc.node = npc.target
				continue
			}
			if math.Abs(turnNPC(npc, math.Atan2(dx, dz))) > 0.35 {
				continue
			}
			step := math.Min(distance, 0.055) // 0.55 m/s at 10 Hz.
			x, z := npc.X+dx/distance*step, npc.Z+dz/distance*step
			blocked := false
			for _, player := range players {
				if math.Abs(player.Y-npc.Y-0.6) < 0.75 && math.Hypot(player.X-x, player.Z-z) < 0.85 {
					blocked = true
				}
			}
			for _, other := range actors {
				if other != npc && math.Hypot(other.X-x, other.Z-z) < 1.1 {
					blocked = true
				}
			}
			for _, robot := range s.facility.cleaners[roomID] {
				if math.Hypot(robot.X-x, robot.Z-z) < 0.8 {
					blocked = true
				}
			}
			if blocked {
				npc.blockedFor += 0.1
				if npc.blockedFor > 1.5 {
					// Reverse only along the current checked edge, then re-plan.
					npc.target, npc.node = npc.node, npc.target
					npc.route = nil
					npc.blockedFor = 0
				}
				continue
			}
			npc.blockedFor = 0
			npc.X, npc.Z = x, z
			npc.Y += (point.Y + 1 - npc.Y) * step / distance
			if step >= distance {
				npc.node = npc.target
				if len(npc.route) == 0 {
					npc.pause = 1 + rand.Float64()*3
				}
			}
		}
	}
}

func (s *Server) conversationNPC(player *store.Player, npcID string) *wanderingNPC {
	roomID := s.Hub.RoomOf(player.ID)
	for _, npc := range s.facility.npcs[roomID] {
		if npc.ID == npcID && nearNPC(npc, player.X, player.Y, player.Z) {
			return npc
		}
	}
	return nil
}

func (s *Server) onNpcConversation(client *hub.Client, player *store.Player, msg incoming) {
	s.facility.mu.Lock()
	defer s.facility.mu.Unlock()
	if !msg.Active {
		if s.facility.conversations[player.ID].npcID == msg.NpcID {
			delete(s.facility.conversations, player.ID)
		}
		return
	}
	if s.conversationNPC(player, msg.NpcID) == nil {
		client.Send(map[string]any{"type": "error", "code": "npc_out_of_reach", "message": "Подойди ближе к персонажу в своей комнате"})
		return
	}
	s.facility.conversations[player.ID] = npcConversation{npcID: msg.NpcID, expires: time.Now().Add(15 * time.Second)}
}

func (s *Server) canTalkToNPC(player *store.Player, npcID string) bool {
	s.facility.mu.Lock()
	defer s.facility.mu.Unlock()
	conversation, ok := s.facility.conversations[player.ID]
	return ok && conversation.npcID == npcID && time.Now().Before(conversation.expires) && s.conversationNPC(player, npcID) != nil
}
