package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"server/internal/world"
	"strings"
	"time"
)

// A proposal, never executable code. httpapi checks the live conversation.
type WorldAction struct{ Kind, Room, Color string }

var SuitPalette = map[string]string{
	"white": "#ffffff", "red": "#ef5350", "orange": "#ff9800", "yellow": "#ffdf55",
	"green": "#66bb6a", "cyan": "#4dd0e1", "blue": "#4f83ff", "purple": "#ab68dc", "pink": "#f48fb1", "black": "#505766",
}

var actionRooms = []struct {
	id      string
	aliases []string
}{
	{"room_medbay", []string{"мед", "больниц", "врач", "совун", "medbay", "medical"}},
	{"room_rockets", []string{"механ", "электр", "инженер", "пин", "mechanical", "electrical"}},
	{"room_greenhouse", []string{"биол", "оранжере", "теплиц", "копатыч", "bio lab", "greenhouse"}},
	{"room_chemistry", []string{"хим", "chem"}},
	{"room_radio", []string{"радио", "карыч", "radio"}},
	{"room_stars", []string{"планетар", "обсерватор", "звезд", "лосяш", "planetarium", "observatory"}},
	{"room_library", []string{"библиотек", "ежик", "library"}},
	{"room_gravity", []string{"скафандр", "нюш", "suit", "gravity"}},
	{"room_dining", []string{"столов", "кухн", "кафе", "cafeteria", "dining", "canteen"}},
}

func containsAny(text string, words ...string) bool {
	for _, word := range words {
		if strings.Contains(text, word) {
			return true
		}
	}
	return false
}

func actionColor(text string) string {
	for _, entry := range []struct {
		name  string
		words []string
	}{
		{"red", []string{"красн", "red"}}, {"orange", []string{"оранж", "orange"}},
		{"yellow", []string{"желт", "yellow"}}, {"green", []string{"зелен", "green"}},
		{"cyan", []string{"голуб", "бирюз", "cyan", "turquoise"}}, {"blue", []string{"син", "blue"}},
		{"purple", []string{"фиолет", "сирен", "purple", "violet"}}, {"pink", []string{"розов", "pink"}},
		{"white", []string{"бел", "white"}}, {"black", []string{"черн", "black"}},
	} {
		if containsAny(text, entry.words...) {
			return entry.name
		}
	}
	return ""
}

func offlineWorldAction(npcID, text string) *WorldAction {
	switch npcID {
	case "sovunya", "sovunya_dining":
		if containsAny(text, "я голод", "голоден", "голодна", "проголод", "хочу есть", "хочу кушать", "накорм", "i'm hungry", "i am hungry", "im hungry", "feed me") ||
			(containsAny(text, "дай", "дайте", "можно", "хочу", "угости", "поделись", "пожалуйста", "give", "want", "please", "can i have") && containsAny(text, "яблок", "яблоч", "ед", "кушат", "поесть", "перекус", "apple", "food", "snack")) || strings.Trim(text, " .!?") == "яблоко" || strings.Trim(text, " .!?") == "apple" {
			return &WorldAction{Kind: "give_apple"}
		}
	case "krosh":
		if containsAny(text, "проводи", "провед", "отвед", "покаж", "где", "как пройти", "как попасть", "как дойти", "lead", "show", "guide", "where", "take me") {
			for _, room := range actionRooms {
				if containsAny(text, room.aliases...) {
					return &WorldAction{Kind: "guide_to_door", Room: room.id}
				}
			}
			if containsAny(text, "проводи", "провед", "отвед", "двер", "lead", "guide", "door", "take me") {
				return &WorldAction{Kind: "guide_to_door"}
			}
		}
	case "nyusha":
		color := actionColor(text)
		if (color != "" && len(strings.Fields(text)) <= 3 && !containsAny(text, "почему", "какой", "какие", "зачем", "why", "what")) ||
			(containsAny(text, "помен", "измени", "смен", "перекрас", "покрас", "сделай", "хочу", "change", "paint", "make", "want") && containsAny(text, "цвет", "скафандр", "костюм", "color", "colour", "suit")) {
			return &WorldAction{Kind: "set_suit_color", Color: color}
		}
	}
	return nil
}

type worldToolCall struct {
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}
type worldTool struct {
	Type     string            `json:"type"`
	Function worldToolFunction `json:"function"`
}
type worldToolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

func worldTools(npcID string) []worldTool {
	function := worldToolFunction{Parameters: map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}}
	switch npcID {
	case "sovunya", "sovunya_dining":
		function.Name, function.Description = "give_apple", "Give this player one apple only if they ask for food, an apple, or say they are hungry."
	case "krosh":
		rooms := []string{}
		for _, room := range actionRooms {
			rooms = append(rooms, room.id)
		}
		function.Name, function.Description = "guide_to_door", "Guide this player to the requested room door in the lobby."
		function.Parameters["properties"] = map[string]any{"room": map[string]any{"type": "string", "enum": rooms}}
		function.Parameters["required"] = []string{"room"}
	case "nyusha":
		function.Name, function.Description = "set_suit_color", "Recolor this player's spacesuit only when they request a specific color."
		function.Parameters["properties"] = map[string]any{"color": map[string]any{"type": "string", "enum": []string{"white", "red", "orange", "yellow", "green", "cyan", "blue", "purple", "pink", "black"}}}
		function.Parameters["required"] = []string{"color"}
	default:
		return nil
	}
	return []worldTool{{Type: "function", Function: function}}
}

func (r *Responder) PlanWorldAction(ctx context.Context, npc world.NPC, message, playerID string) (action *WorldAction, err error) {
	profile := r.effective(npc)
	tools := worldTools(npc.ID)
	text := strings.ReplaceAll(strings.ToLower(message), "ё", "е")
	words := " " + strings.NewReplacer(",", " ", ".", " ", "!", " ", "?", " ", ";", " ", ":", " ").Replace(text)
	if profile.Mode == ModeSilent || len(tools) == 0 || containsAny(words, " не давай", " не надо", " не нужно", " не хочу", " не голод", " не меняй", " не перекраш", " don't", " do not", " not hungry") {
		return nil, nil
	}
	if action := offlineWorldAction(npc.ID, text); action != nil {
		return action, nil
	}
	enabled, model := r.LLM()
	if !enabled || r.ollamaURL == "" || profile.Mode != ModeLLM || !containsAny(text, "хочу", "можешь", "сможешь", "дай", "покажи", "сделай", "проводи", "где", "пожалуйста", "поесть", "перекус", "help", "where", "can you", "would", "want", "give", "show", "lead", "make", "making", "paint") {
		return nil, nil
	}
	select {
	case r.sem <- struct{}{}:
		defer func() { <-r.sem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	r.statsMu.Lock()
	r.stats.InFlight++
	r.statsMu.Unlock()
	started := time.Now()
	defer func() { r.recordLLM(started, err) }()
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	prompt := "You select a game action, never execute it. Use at most one supplied tool only if the latest user explicitly requests it. For questions about facts, negations or unclear arguments, call no tool. Room IDs: room_medbay=медблок, room_rockets=механический/электрический, room_greenhouse=биолаб, room_chemistry=химлаб, room_radio=радиорубка, room_stars=планетарий, room_library=библиотека, room_gravity=скафандры, room_dining=столовая."
	messages := []chatMessage{{Role: "system", Content: prompt}}
	messages = append(messages, r.getHistory(playerID, npc.ID)...)
	messages = append(messages, chatMessage{Role: "user", Content: message})
	body, err := json.Marshal(ollamaChatRequest{Model: model, Messages: messages, Tools: tools, Stream: false})
	if err != nil {
		return nil, fmt.Errorf("encode action request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.ollamaURL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create action request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("plan world action: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("action planner HTTP %d", resp.StatusCode)
	}
	var result ollamaChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode action response: %w", err)
	}
	if len(result.Message.ToolCalls) != 1 {
		return nil, nil
	}
	call := result.Message.ToolCalls[0]
	if call.Function.Name != tools[0].Function.Name {
		return nil, nil
	}
	var args struct {
		Room  string `json:"room"`
		Color string `json:"color"`
	}
	decoder := json.NewDecoder(bytes.NewReader(call.Function.Arguments))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&args); err != nil {
		return nil, fmt.Errorf("invalid action arguments: %w", err)
	}
	return &WorldAction{Kind: call.Function.Name, Room: args.Room, Color: args.Color}, nil
}

func (r *Responder) RecordWorldAction(npcID, playerID, message, result string) {
	r.markVisited(playerID, npcID)
	r.setState(playerID, npcID, Telling)
	r.appendHistory(playerID, npcID, message, result)
}
