package httpapi

import (
	"log"
	"math"
	"server/internal/ai"
	"server/internal/hub"
	"server/internal/store"
	"server/internal/world"
	"time"
)

// Call with facility.mu held, preserving publication order for DB mutations.
func (s *Server) publishPlayerState(playerID, room string) (store.WorldState, error) {
	state, err := s.Store.WorldState(playerID)
	if err != nil {
		return state, err
	}
	s.facility.revision++
	state.Revision = s.facility.revision
	s.Hub.SetWorldState(playerID, state.SuitColor, state.Apples)
	s.Hub.BroadcastToRoom(room, "", map[string]any{"type": "player_state", "state": state})
	return state, nil
}

func (s *Server) executeWorldAction(client *hub.Client, definition world.NPC, msg incoming, action *ai.WorldAction) string {
	s.facility.mu.Lock()
	defer s.facility.mu.Unlock()
	current, connected := s.Hub.Get(client.PlayerID)
	player, exists := s.Hub.Player(client.PlayerID)
	conversation := s.facility.conversations[client.PlayerID]
	room := s.Hub.RoomOf(client.PlayerID)
	if !connected || current != client || !exists || room != definition.RoomID || conversation.id != msg.ConversationID || msg.ConversationID == 0 || conversation.npcID != definition.ID || !time.Now().Before(conversation.expires) {
		return "Разговор уже закончился. Подойди и попроси меня ещё раз."
	}
	var npc *wanderingNPC
	for _, actor := range s.facility.npcs[room] {
		if actor.ID == definition.ID {
			npc = actor
			break
		}
	}
	if npc == nil || !nearNPC(npc, player.X, player.Y, player.Z) {
		return "Подойди поближе, пожалуйста."
	}
	changed := false
	var err error
	switch action.Kind {
	case "give_apple":
		if definition.ID != "sovunya" && definition.ID != "sovunya_dining" {
			return "Я не могу выдавать еду. Попроси Совунью."
		}
		changed, err = s.Store.GiveApple(player.PlayerID)
		if err == nil && !changed {
			return "У тебя уже есть запас яблок, или я только что дала тебе одно. Съешь яблоко клавишей F и подожди немного."
		}
	case "set_suit_color":
		if definition.ID != "nyusha" {
			return "Перекрашиванием скафандров занимается Нюша."
		}
		color, allowed := ai.SuitPalette[action.Color]
		if !allowed {
			return "Какой цвет хочешь? Белый, красный, оранжевый, жёлтый, зелёный, голубой, синий, фиолетовый, розовый или чёрный?"
		}
		err = s.Store.SetSuitColor(player.PlayerID, color)
		changed = err == nil
	case "guide_to_door":
		if definition.ID != "krosh" || room != "hub" {
			return "Проводник Крош ждёт тебя в главной комнате."
		}
		if action.Room == "" {
			return "Куда тебя проводить? Назови отсек: например, «Проводи в медблок» или «Покажи библиотеку»."
		}
		if npc.GuidePlayer != "" && npc.GuidePlayer != player.PlayerID {
			return "Сейчас я провожаю другого игрока. Подожди немного, и пойдём вместе!"
		}
		name, ok := s.startGuide(npc, room, player.PlayerID, action.Room)
		if !ok {
			return "Я не нашёл доступную дверь к этому отсеку. Назови один из девяти отсеков станции."
		}
		s.facility.revision++
		s.broadcastFacility(room)
		return "Ёлки-иголки, покажу! Закрой чат (Esc) и иди за мной. Я проведу тебя к двери: «" + name + "». Если отстанешь, я подожду."
	default:
		return "Такое действие мне недоступно."
	}
	if err != nil {
		log.Printf("world action %s/%s: %v", definition.ID, action.Kind, err)
		return "Не удалось выполнить действие. Попробуй ещё раз немного позже."
	}
	if changed {
		if _, err := s.publishPlayerState(player.PlayerID, room); err != nil {
			log.Printf("publish world action: %v", err)
			return "Действие сохранено, но не удалось обновить состояние. Переподключись, пожалуйста."
		}
		s.Hub.BroadcastToRoom(room, "", map[string]any{
			"type": "npc_action", "room": room, "npcId": definition.ID, "playerId": player.PlayerID, "action": action.Kind,
			"from": world.Vec3{X: npc.X, Y: npc.Y + 0.1, Z: npc.Z}, "to": world.Vec3{X: player.X, Y: player.Y - 0.35, Z: player.Z},
		})
	}
	if action.Kind == "give_apple" {
		return "Держи яблочко! Оно уже у тебя. Закрой чат и нажми F, когда захочешь его съесть."
	}
	return "Готово! Я перекрасила твой скафандр. Цвет виден и другим игрокам, и на твоём индикаторе скафандра."
}

func (s *Server) onUseItem(client *hub.Client, player *store.Player, msg incoming) {
	if msg.Item != "apple" {
		return
	}
	s.facility.mu.Lock()
	defer s.facility.mu.Unlock()
	consumed, err := s.Store.EatApple(player.ID)
	if err != nil {
		log.Printf("use apple: %v", err)
		client.Send(map[string]any{"type": "error", "code": "item_error", "message": "Не удалось съесть яблоко"})
		return
	}
	if !consumed {
		client.Send(map[string]any{"type": "error", "code": "item_empty", "message": "Яблок нет. Попроси Совунью угостить тебя."})
		return
	}
	if _, err := s.publishPlayerState(player.ID, s.Hub.RoomOf(player.ID)); err != nil {
		log.Printf("publish use apple: %v", err)
	}
}

func clearGuide(npc *wanderingNPC) {
	npc.GuidePlayer, npc.GuideRoom, npc.GuideArrived = "", "", false
	npc.route = nil
}

func routeTo(nav world.RoombaNavigation, start, goal int) []int {
	previous := make([]int, len(nav.Points))
	for i := range previous {
		previous[i] = -1
	}
	previous[start] = start
	queue := []int{start}
	for head := 0; head < len(queue) && previous[goal] == -1; head++ {
		for _, next := range nav.Links[queue[head]] {
			if previous[next] == -1 {
				previous[next] = queue[head]
				queue = append(queue, next)
			}
		}
	}
	if previous[goal] == -1 {
		return nil
	}
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

func (s *Server) startGuide(npc *wanderingNPC, roomID, owner, destination string) (string, bool) {
	room := s.World.Rooms[roomID]
	var goalX, goalZ float64
	found := false
	for _, door := range room.Doors {
		if door.Room != destination {
			continue
		}
		length := math.Hypot(door.Position.X, door.Position.Z)
		goalX = room.Position.X + door.Position.X*(1-2.2/length)
		goalZ = room.Position.Z + door.Position.Z*(1-2.2/length)
		found = true
		break
	}
	if !found {
		return "", false
	}
	nav := s.World.NPCNavigation[roomID]
	seen := map[int]bool{npc.target: true}
	queue := []int{npc.target}
	for head := 0; head < len(queue); head++ {
		for _, next := range nav.Links[queue[head]] {
			if !seen[next] {
				seen[next] = true
				queue = append(queue, next)
			}
		}
	}
	goal, best := npc.target, math.Inf(1)
	for _, i := range queue {
		p := nav.Points[i]
		if distance := math.Hypot(p.X-goalX, p.Z-goalZ); distance < best {
			goal, best = i, distance
		}
	}
	if best > 2.5 {
		return "", false
	}
	npc.GuidePlayer, npc.GuideRoom, npc.GuideArrived = owner, destination, false
	npc.guideGoal, npc.guideUntil, npc.pause = goal, time.Now().Add(180*time.Second), 0
	npc.route = routeTo(nav, npc.target, goal)
	return s.World.Rooms[destination].Name, true
}
