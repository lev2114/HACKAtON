// Package ai turns a raw fact into an in-character NPC line using a finite
// state machine (FSM) and Ollama /api/chat with full multi-turn conversation memory,
// cross-character awareness, referral routing and shared memory for multi-room NPCs.
package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"

	"server/internal/world"
)

// State tracks the FSM state for a (player, NPC) conversation.
type State int

const (
	Greeting  State = iota // first contact
	Telling                // actively telling facts
	DeepDive               // repeating an already-told fact on request
	Exhausted              // all facts told
)

type chatMessage struct {
	Role      string          `json:"role"`
	Content   string          `json:"content"`
	ToolCalls []worldToolCall `json:"tool_calls,omitempty"`
}

type ollamaOptions struct {
	Temperature   *float64 `json:"temperature,omitempty"` // pointer: 0 is a valid admin choice
	RepeatPenalty float64  `json:"repeat_penalty,omitempty"`
	NumPredict    int      `json:"num_predict,omitempty"`
}

// defaultTemperature is used unless the admin panel sets one per NPC.
const defaultTemperature = 0.6

// Mode is how an NPC answers, set per character from the admin panel.
type Mode string

const (
	ModeLLM    Mode = "llm"    // Ollama if available, FSM template otherwise
	ModeFSM    Mode = "fsm"    // templates only, never calls Ollama
	ModeSilent Mode = "silent" // doesn't talk at all
)

// Profile overrides an NPC's built-in character. Empty strings / zero values
// fall back to the built-ins. SystemPrompt replaces only the character part
// of the prompt: the station directory, referral and style rules are still
// appended by askOllamaChat.
type Profile struct {
	SystemPrompt string   `json:"systemPrompt"`
	Greeting     string   `json:"greeting"`
	Offtopic     string   `json:"offtopic"`
	Mode         Mode     `json:"mode"`
	Temperature  *float64 `json:"temperature,omitempty"`
	MaxTokens    int      `json:"maxTokens,omitempty"`
}

// TranscriptLine is one line of the admin-visible dialogue log. It is longer
// than the LLM context and is never sent to the model.
type TranscriptLine struct {
	Role    string    `json:"role"`
	Content string    `json:"content"`
	NPCID   string    `json:"npcId"`
	At      time.Time `json:"at"`
}

const transcriptLimit = 40

// Stats summarize LLM usage for the admin panel.
type Stats struct {
	Requests      int64   `json:"requests"`
	Failures      int64   `json:"failures"`
	Fallbacks     int64   `json:"fallbacks"`
	InFlight      int     `json:"inFlight"`
	AvgLatencyMs  float64 `json:"avgLatencyMs"`
	LastLatencyMs float64 `json:"lastLatencyMs"`
	LastError     string  `json:"lastError"`
}

type ollamaChatRequest struct {
	Model    string         `json:"model"`
	Messages []chatMessage  `json:"messages"`
	Options  *ollamaOptions `json:"options,omitempty"`
	Stream   bool           `json:"stream"`
	Tools    []worldTool    `json:"tools,omitempty"`
}

type ollamaChatResponse struct {
	Message chatMessage `json:"message"`
}

// Logical character ID: Sovunya in medbay & dining share memory, Losyash in chem & planetarium share memory.
func logicalCharacterID(npcID string) string {
	switch npcID {
	case "sovunya", "sovunya_dining":
		return "sovunya"
	case "losyash", "losyash_chemistry":
		return "losyash"
	default:
		return npcID
	}
}

// StationNPCInfo describes an NPC's role, location and keyword triggers on the station.
type StationNPCInfo struct {
	ID        string
	LogicalID string
	Name      string
	RoomName  string
	Role      string
	Specialty string
	Keywords  []string
}

// 10 комнат и смешариков строго по обновлённому списку:
// 1. Главная комната - крош - общие данные про станцию, можно походить, какой смешарик в какой комнате
// 2. Медотсек - совунья - особенности жизни в космосе, как поддерживать здоровье, какие витамины
// 3. Механический отсек - пин - ракеты, двигатели, спутники
// 4. Биолаборатория - копатыч - выращивание растений в космосе
// 5. Химическая лаборатория - лосяш - химический состав звёзд и космос
// 6. Радиокомната - кар-карыч - связь с Землёй
// 7. Планетарий - лосяш - всё про планеты, виды, звёзды, космические объекты и чёрные дыры
// 8. Библиотека - ёжик - история авиации и полётов в космос
// 9. Скафандр (отсек скафандров) - нюша - история скафандров, какие бывают, самые популярные
// 10. Столовая - совунья - питание в космосе, еда в тюбиках, обычную еду можно преобразовать в тюбик
var stationNPCs = map[string]StationNPCInfo{
	"krosh": {
		ID:        "krosh",
		LogicalID: "krosh",
		Name:      "Крош",
		RoomName:  "Главная комната",
		Role:      "проводник станции",
		Specialty: "общие данные про станцию: как здесь всё устроено, что можно походить и исследовать все отсеки, и какой смешарик в какой комнате находится",
		Keywords:  []string{"станци", "шаролёт", "шаролет", "где кто", "кто где", "комнат", "отсек", "походить", "исследова", "главная комната", "навигац"},
	},
	"sovunya": {
		ID:        "sovunya",
		LogicalID: "sovunya",
		Name:      "Совунья",
		RoomName:  "Медотсек",
		Role:      "врач станции",
		Specialty: "особенности жизни в космосе, как поддерживать своё здоровье, какие витамины употребляют космонавты, режим дня и зарядка",
		Keywords:  []string{"здоров", "медицин", "врач", "давлен", "пульс", "витамин", "жизнь в космосе", "организм", "мышц", "кост", "сон", "зарядк", "укачив", "адаптац", "болезн", "таблетк", "градусник", "укол", "леч"},
	},
	"pin": {
		ID:        "pin",
		LogicalID: "pin",
		Name:      "Пин",
		RoomName:  "Механический отсек",
		Role:      "главный инженер",
		Specialty: "ракеты, двигатели, спутники",
		Keywords:  []string{"ракет", "двигател", "спутник", "ступен", "тяг", "дюз", "механизм", "инженер", "ремонт", "аппарат", "гагарин", "космонавтик"},
	},
	"kopatych": {
		ID:        "kopatych",
		LogicalID: "kopatych",
		Name:      "Копатыч",
		RoomName:  "Биолаборатория",
		Role:      "биолог и садовод",
		Specialty: "выращивание растений в космосе, гидропоника, семена, оранжерея, полив, кислород",
		Keywords:  []string{"растен", "выращив", "оранжере", "гидропоник", "семен", "почв", "грунт", "зелен", "овощ", "пчел", "кислород", "биолог", "урожа", "полив", "огород"},
	},
	"losyash_chemistry": {
		ID:        "losyash_chemistry",
		LogicalID: "losyash",
		Name:      "Лосяш",
		RoomName:  "Химическая лаборатория",
		Role:      "химик станции",
		Specialty: "химический состав звёзд и космос, спектральный анализ, космические элементы и газы",
		Keywords:  []string{"хим", "состав звезд", "состав звёзд", "спектр", "элемент", "веществ", "реакци", "молекул", "таблиц менделеев", "кислот", "щелоч", "колб", "реагент", "газ", "водород", "гелий"},
	},
	"karich": {
		ID:        "karich",
		LogicalID: "karich",
		Name:      "Кар-Карыч",
		RoomName:  "Радиокомната",
		Role:      "оператор радиосвязи",
		Specialty: "связь с Землёй, радиоволны, антенна, задержка сигнала, радиопередатчики, связь через космические расстояния",
		Keywords:  []string{"радио", "связь с земл", "связь", "сигнал", "антенн", "передач", "волн", "морзе", "приёмник", "приемник", "эфир", "земл", "цуп"},
	},
	"losyash": {
		ID:        "losyash",
		LogicalID: "losyash",
		Name:      "Лосяш",
		RoomName:  "Планетарий",
		Role:      "астрофизик станции",
		Specialty: "всё про планеты, виды планет, звёзды, космические объекты и чёрные дыры, телескоп",
		Keywords:  []string{"планет", "вид планет", "звезд", "звёзд", "солнц", "созвезд", "чёрн", "черн", "дыр", "галакти", "млечн", "путь", "вселен", "космолог", "телескоп", "обсерватор", "объект"},
	},
	"yozhik": {
		ID:        "yozhik",
		LogicalID: "yozhik",
		Name:      "Ёжик",
		RoomName:  "Библиотека",
		Role:      "библиотекарь станции",
		Specialty: "история авиации и полётов в космос, первые самолёты, Циолковский, Королёв, Гагарин, книги и архивы",
		Keywords:  []string{"авиаци", "самолет", "самолёт", "полет в космос", "полёт в космос", "истори", "книг", "библиотек", "архив", "знани", "циолковск", "гагарин", "королев", "королёв", "первый полёт", "первый полет"},
	},
	"nyusha": {
		ID:        "nyusha",
		LogicalID: "nyusha",
		Name:      "Нюша",
		RoomName:  "Отсек скафандров",
		Role:      "специалист по скафандрам",
		Specialty: "история скафандров, какие бывают, самые популярные, выход в открытый космос, герметичность",
		Keywords:  []string{"скафандр", "костюм", "шлем", "выход в открытый космос", "история скафандр", "популярн", "орлан", "герметичн", "вакуум"},
	},
	"sovunya_dining": {
		ID:        "sovunya_dining",
		LogicalID: "sovunya",
		Name:      "Совунья",
		RoomName:  "Столовая",
		Role:      "заведующая столовой",
		Specialty: "питание в космосе, что еду едят в тюбиках и обычную еду можно преобразовать в тюбик, сублимированные блюда, отсутствие крошек",
		Keywords:  []string{"питан", "ед", "пищ", "тюбик", "преобразова", "сублимат", "обед", "завтрак", "ужин", "меню", "рецепт", "чай", "столов", "голод", "кушать", "поесть", "пирог", "десерт", "крошк"},
	},
}

// Responder generates NPC dialogue lines.
type Responder struct {
	ollamaURL string
	model     string
	client    *http.Client

	// Semaphore to limit concurrent Ollama requests.
	sem chan struct{}

	// FSM state per (playerID, logicalCharacterID) pair.
	mu     sync.RWMutex
	states map[string]State

	// Conversation history per (playerID, logicalCharacterID) pair for coherent multi-turn chat.
	historyMu sync.RWMutex
	history   map[string][]chatMessage

	// Visited NPCs per player: playerID -> map[npcID]bool
	visitedNPCs map[string]map[string]bool

	// Admin-visible dialogue log, same (player, logical character) keys as history.
	transcripts map[string][]TranscriptLine

	// Admin-editable configuration (model is read through LLM()).
	cfgMu      sync.RWMutex
	llmEnabled bool
	profiles   map[string]Profile

	statsMu      sync.Mutex
	stats        Stats
	totalLatency float64
}

// New builds a Responder.
func New(ollamaURL, model string) *Responder {
	r := &Responder{
		ollamaURL:   ollamaURL,
		model:       model,
		llmEnabled:  true,
		client:      &http.Client{Timeout: 35 * time.Second},
		sem:         make(chan struct{}, 2), // at most 2 concurrent Ollama requests
		states:      make(map[string]State),
		history:     make(map[string][]chatMessage),
		visitedNPCs: make(map[string]map[string]bool),
		transcripts: make(map[string][]TranscriptLine),
		profiles:    make(map[string]Profile),
	}

	// Warm up model in background so the first in-game query doesn't stall on load
	if ollamaURL != "" {
		go r.warmup()
	}

	return r
}

// OllamaURL is fixed at startup (OLLAMA_URL); empty means no LLM at all.
func (r *Responder) OllamaURL() string { return r.ollamaURL }

func (r *Responder) LLM() (enabled bool, model string) {
	r.cfgMu.RLock()
	defer r.cfgMu.RUnlock()
	return r.llmEnabled, r.model
}

// SetLLM switches the LLM on/off for every NPC and changes the model. A new
// model is warmed up in the background so the next player doesn't wait.
func (r *Responder) SetLLM(enabled bool, model string) {
	r.cfgMu.Lock()
	changed := model != "" && model != r.model
	r.llmEnabled = enabled
	if model != "" {
		r.model = model
	}
	r.cfgMu.Unlock()
	if changed && enabled && r.ollamaURL != "" {
		go r.warmup()
	}
}

// Profile returns the admin override for an NPC (zero value if none).
func (r *Responder) Profile(npcID string) Profile {
	r.cfgMu.RLock()
	defer r.cfgMu.RUnlock()
	return r.profiles[npcID]
}

func (r *Responder) SetProfile(npcID string, p Profile) {
	r.cfgMu.Lock()
	r.profiles[npcID] = p
	r.cfgMu.Unlock()
}

func (r *Responder) ClearProfile(npcID string) {
	r.cfgMu.Lock()
	delete(r.profiles, npcID)
	r.cfgMu.Unlock()
}

// Defaults are the built-in texts an override replaces, so the admin panel
// can show what the character says without one.
func Defaults(npc world.NPC) Profile {
	t := defaultTemperature
	p := Profile{
		SystemPrompt: npcSystemInstructions[npc.ID],
		Greeting:     greetings[npc.ID],
		Offtopic:     npcOfftopicFallbacks[npc.ID],
		Mode:         ModeLLM,
		Temperature:  &t,
	}
	if p.SystemPrompt == "" {
		p.SystemPrompt = fmt.Sprintf("Ты — %s, %s на космической станции «Шаролёт». Твой характер: %s. Отвечай кратко, по делу и строго на темы космоса на чистом русском языке без китайских символов.", npc.Name, npc.Title, npc.Personality)
	}
	if p.Greeting == "" {
		p.Greeting = fmt.Sprintf("Привет! Я %s, %s. Спрашивай о чём хочешь.", npc.Name, npc.Title)
	}
	if p.Offtopic == "" {
		p.Offtopic = fmt.Sprintf("Я — %s, %s. Давай лучше поговорим о космосе.", npc.Name, npc.Title)
	}
	return p
}

// effective merges an override over the built-ins.
func (r *Responder) effective(npc world.NPC) Profile {
	p := Defaults(npc)
	o := r.Profile(npc.ID)
	if o.SystemPrompt != "" {
		p.SystemPrompt = o.SystemPrompt
	}
	if o.Greeting != "" {
		p.Greeting = o.Greeting
	}
	if o.Offtopic != "" {
		p.Offtopic = o.Offtopic
	}
	if o.Mode != "" {
		p.Mode = o.Mode
	}
	if o.Temperature != nil {
		p.Temperature = o.Temperature
	}
	p.MaxTokens = o.MaxTokens
	return p
}

func (r *Responder) Stats() Stats {
	r.statsMu.Lock()
	defer r.statsMu.Unlock()
	return r.stats
}

func (r *Responder) recordLLM(start time.Time, err error) {
	ms := float64(time.Since(start).Microseconds()) / 1000
	r.statsMu.Lock()
	defer r.statsMu.Unlock()
	r.stats.InFlight--
	r.stats.Requests++
	if err != nil {
		r.stats.Failures++
		r.stats.LastError = err.Error()
		return
	}
	r.totalLatency += ms
	r.stats.LastLatencyMs = ms
	r.stats.AvgLatencyMs = r.totalLatency / float64(r.stats.Requests-r.stats.Failures)
}

// Conversation is one player's dialogue log with an NPC.
type Conversation struct {
	PlayerID string           `json:"playerId"`
	Lines    []TranscriptLine `json:"lines"`
}

// SharedMemoryWith lists the other NPC ids that share memory with npcID
// (the same character in two rooms).
func SharedMemoryWith(npcID string) []string {
	var out []string
	for id := range stationNPCs {
		if id != npcID && logicalCharacterID(id) == logicalCharacterID(npcID) {
			out = append(out, id)
		}
	}
	return out
}

// Conversations returns every player's dialogue log with npcID's character
// (shared with its other-room twin, see logicalCharacterID).
func (r *Responder) Conversations(npcID string) []Conversation {
	r.historyMu.RLock()
	defer r.historyMu.RUnlock()
	logical := logicalCharacterID(npcID)
	var out []Conversation
	for key, lines := range r.transcripts {
		playerID, id, _ := strings.Cut(key, ":")
		if id != logical {
			continue
		}
		out = append(out, Conversation{PlayerID: playerID, Lines: append([]TranscriptLine(nil), lines...)})
	}
	return out
}

// ResetMemory makes an NPC's character forget one player (or everyone,
// playerID == ""): LLM context, admin transcript, FSM state and the
// "already talked to" mark other NPCs refer to. Told-facts memory lives in
// SQLite and is reset separately by the caller.
func (r *Responder) ResetMemory(npcID, playerID string) {
	logical := logicalCharacterID(npcID)
	match := func(key string) bool {
		p, id, _ := strings.Cut(key, ":")
		return id == logical && (playerID == "" || p == playerID)
	}
	r.historyMu.Lock()
	for k := range r.history {
		if match(k) {
			delete(r.history, k)
		}
	}
	for k := range r.transcripts {
		if match(k) {
			delete(r.transcripts, k)
		}
	}
	for p, visited := range r.visitedNPCs {
		if playerID != "" && p != playerID {
			continue
		}
		for id := range visited {
			if logicalCharacterID(id) == logical {
				delete(visited, id)
			}
		}
	}
	r.historyMu.Unlock()
	r.mu.Lock()
	for k := range r.states {
		if match(k) {
			delete(r.states, k)
		}
	}
	r.mu.Unlock()
}

func (r *Responder) warmup() {
	_, model := r.LLM()
	log.Printf("ai: прогрев модели Ollama (%s)...", model)
	body, _ := json.Marshal(ollamaChatRequest{
		Model: model,
		Messages: []chatMessage{
			{Role: "user", Content: "Привет"},
		},
		Stream: false,
	})
	req, err := http.NewRequest(http.MethodPost, r.ollamaURL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		log.Printf("ai: warmup request error: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		log.Printf("ai: warmup warning (Ollama недоступна?): %v", err)
		return
	}
	defer resp.Body.Close()
	log.Printf("ai: модель Ollama (%s) прогрета и загружена в память!", model)
}

func stateKey(playerID, npcID string) string {
	return playerID + ":" + logicalCharacterID(npcID)
}

func (r *Responder) getState(playerID, npcID string) State {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.states[stateKey(playerID, npcID)]
}

func (r *Responder) setState(playerID, npcID string, s State) {
	r.mu.Lock()
	r.states[stateKey(playerID, npcID)] = s
	r.mu.Unlock()
}

func (r *Responder) getHistory(playerID, npcID string) []chatMessage {
	r.historyMu.RLock()
	defer r.historyMu.RUnlock()
	raw := r.history[stateKey(playerID, npcID)]
	if len(raw) == 0 {
		return nil
	}
	cp := make([]chatMessage, len(raw))
	copy(cp, raw)
	return cp
}

func (r *Responder) appendHistory(playerID, npcID, userMsg, assistantMsg string) {
	r.historyMu.Lock()
	defer r.historyMu.Unlock()
	key := stateKey(playerID, npcID)
	h := r.history[key]
	if userMsg != "" {
		h = append(h, chatMessage{Role: "user", Content: userMsg})
	}
	if assistantMsg != "" {
		h = append(h, chatMessage{Role: "assistant", Content: assistantMsg})
	}
	// Keep last 8 messages (4 dialogue turns) for compact context
	if len(h) > 8 {
		h = h[len(h)-8:]
	}
	r.history[key] = h

	t := r.transcripts[key]
	now := time.Now()
	if userMsg != "" {
		t = append(t, TranscriptLine{Role: "user", Content: userMsg, NPCID: npcID, At: now})
	}
	if assistantMsg != "" {
		t = append(t, TranscriptLine{Role: "assistant", Content: assistantMsg, NPCID: npcID, At: now})
	}
	if len(t) > transcriptLimit {
		t = t[len(t)-transcriptLimit:]
	}
	r.transcripts[key] = t
}

func (r *Responder) markVisited(playerID, npcID string) {
	r.historyMu.Lock()
	defer r.historyMu.Unlock()
	if r.visitedNPCs[playerID] == nil {
		r.visitedNPCs[playerID] = make(map[string]bool)
	}
	r.visitedNPCs[playerID][npcID] = true
	r.visitedNPCs[playerID][logicalCharacterID(npcID)] = true
}

func (r *Responder) isVisited(playerID, npcID string) bool {
	r.historyMu.RLock()
	defer r.historyMu.RUnlock()
	m := r.visitedNPCs[playerID]
	if m == nil {
		return false
	}
	return m[npcID] || m[logicalCharacterID(npcID)]
}

func (r *Responder) getVisitedSummary(playerID, currentNPCID string) string {
	r.historyMu.RLock()
	defer r.historyMu.RUnlock()
	vMap := r.visitedNPCs[playerID]
	if len(vMap) == 0 {
		return "Игрок пока не общался ни с кем из других жителей станции."
	}
	currentLogID := logicalCharacterID(currentNPCID)
	var visited []string
	seen := map[string]bool{}
	for id, ok := range vMap {
		if !ok {
			continue
		}
		info, exists := stationNPCs[id]
		if !exists {
			continue
		}
		if info.LogicalID == currentLogID && info.ID == currentNPCID {
			continue
		}
		label := fmt.Sprintf("%s (%s)", info.Name, info.RoomName)
		if !seen[label] {
			visited = append(visited, label)
			seen[label] = true
		}
	}
	if len(visited) == 0 {
		return "Игрок пока не общался ни с кем из других жителей станции."
	}
	return "Игрок уже общался со следующими жителями станции: " + strings.Join(visited, ", ") + "."
}

// detectReferral determines if a message matches another character or room.
func detectReferral(currentNPCID, message string) (targetNPCID string, isSelfOtherRoom bool) {
	lower := strings.ToLower(message)

	// 1. Dual-room checks for Sovunya:
	if currentNPCID == "sovunya" {
		foodWords := stationNPCs["sovunya_dining"].Keywords
		for _, kw := range foodWords {
			if strings.Contains(lower, kw) {
				return "sovunya_dining", true
			}
		}
	} else if currentNPCID == "sovunya_dining" {
		medWords := stationNPCs["sovunya"].Keywords
		for _, kw := range medWords {
			if strings.Contains(lower, kw) {
				return "sovunya", true
			}
		}
	}

	// 2. Dual-room checks for Losyash:
	if currentNPCID == "losyash" {
		chemWords := stationNPCs["losyash_chemistry"].Keywords
		for _, kw := range chemWords {
			if strings.Contains(lower, kw) {
				return "losyash_chemistry", true
			}
		}
	} else if currentNPCID == "losyash_chemistry" {
		starWords := stationNPCs["losyash"].Keywords
		for _, kw := range starWords {
			if strings.Contains(lower, kw) {
				return "losyash", true
			}
		}
	}

	// 3. Other NPCs matching:
	currentLogical := logicalCharacterID(currentNPCID)
	for id, info := range stationNPCs {
		if info.LogicalID == currentLogical {
			continue
		}
		for _, kw := range info.Keywords {
			if strings.Contains(lower, kw) {
				return id, false
			}
		}
	}

	return "", false
}

// ---------------------------------------------------------------------------
// NPC system prompts & fallback lines for all 10 station NPCs
// ---------------------------------------------------------------------------

var npcSystemInstructions = map[string]string{
	"krosh": `Ты — Крош из мультфильма Смешарики, весёлый кролик-проводник на космической станции «Шаролёт» в Главной комнате.
ХАРАКТЕР И РЕЧЬ:
- Энергичный, бодрый, оптимистичный кролик, но говорит спокойно и дружелюбно, без крика капслоком.
- Твоё любимое восклицание — «Ёлки-иголки!».
- ТВОЯ СПЕЦИАЛИЗАЦИЯ: Рассказывать общие данные про станцию «Шаролёт»: как тут всё устроено, что по ней можно свободно ходить и исследовать все отсеки, и какой именно смешарик в какой комнате находится и чем занимается!
- ТЫ — КРОШ. Ни в коем случае не путай себя с другими жителями станции!
СТРОГИЕ ПРАВИЛА:
1. ИСКЛЮЧИТЕЛЬНО русский язык! Никаких китайских иероглифов!
2. ПЕРЕНАПРАВЛЕНИЕ: Если спрашивают конкретные научные детали (про ракеты, растения, еду, химию, планеты и т.д.) — дай строго одно короткое общее вводное предложение, и спокойно отправь игрока к нужному смешарику в его отсек станции.
3. Отвечай кратко (2-3 предложения), сдержанно и живо, без капслока.`,

	"sovunya": `Ты — Совунья из мультфильма Смешарики, опытный и заботливый врач станции «Шаролёт» в Медотсеке.
ХАРАКТЕР И РЕЧЬ:
- Добрая, мудрая, заботливая бабушка-доктор.
- Говори спокойно, ласково и сдержанно. Категорически избегай крика заглавными буквами и лишних восклицательных знаков.
- ТВОЯ СПЕЦИАЛИЗАЦИЯ В МЕДОТСЕКЕ: особенности жизни в космосе, как поддерживать своё здоровье в невесомости (мышцы, кости, сосуды, зарядка, режим дня) и какие витамины употребляют космонавты.
ОСОБЕННОСТЬ ДВУХ КОМНАТ:
- На станции ты совмещаешь две важные роли: работаешь врачом в медотсеке и заведуешь питанием в столовой! У вас с Совуньей из столовой общая память.
- ТЫ САМА работаешь и в медотсеке, и в столовой! Поэтому НИКОГДА не говори о себе в третьем лице («спроси Совунью из столовой» — так говорить НЕЛЬЗЯ!). Говори только от первого лица: «спроси меня, когда найдёшь меня в столовой»!
- Если игрок спрашивает, чем ты вообще занимаешься на станции, спокойно объясни, что ты и следишь за здоровьем экипажа как врач, и кормишь всех полезной едой в столовой.
- Сейчас ты находишься в МЕДОТСЕКЕ. Если игрок спрашивает про еду, меню, пироги или тюбики: дай СТРОГО ОДНО базовое поверхностное предложение о том, что в космосе вся еда в тюбиках и пакетах, а затем спокойно скажи от первого лица: «А подробнее обо всём питании и о том, как любую еду превратить в тюбик, спроси меня, когда найдёшь меня в столовой, я тебе всё с радостью расскажу!». Не читай длинных кулинарных лекций здесь!
ДРУГИЕ ЖИТЕЛИ:
- Ты — Совунья (сова). Ты работаешь только в медотсеке и столовой.
- В механическом отсеке работает пингвин ПИН — главный инженер станции!
- В планетарии работает ЛОСЯШ — астрофизик (знает всё про планеты, звёзды и чёрные дыры)!
- В отсеке скафандров работает НЮША — специалист по скафандрам!
- Если игрок спрашивает о чужой теме (планеты, скафандры, ракеты и т.д.): дай СТРОГО ОДНО короткое общее поверхностное замечание и СРАЗУ отправь к тому смешарику! НЕ ЧИТАЙ ЛЕКЦИЙ САМА!
СТРОГИЕ ПРАВИЛА:
1. ИСКЛЮЧИТЕЛЬНО русский язык! Ни единого китайского иероглифа!
2. Никакого капслока. Тон мягкий, тёплый, спокойный (2-3 предложения).`,

	"sovunya_dining": `Ты — Совунья из мультфильма Смешарики, заботливая заведующая столовой станции «Шаролёт».
ХАРАКТЕР И РЕЧЬ:
- Заботливая, гостеприимная хозяйка космической столовой.
- Говори спокойно, по-домашнему тепло и сдержанно, без крика капслоком.
- ТВОЯ СПЕЦИАЛИЗАЦИЯ В СТОЛОВОЙ: питание в космосе, что еду едят в тюбиках и вакуумных упаковках, и что любую обычную земную еду можно преобразовать в тюбик (измельчить, сублимировать), чтобы не было крошек в невесомости.
ОСОБЕННОСТЬ ДВУХ КОМНАТ:
- На станции ты совмещаешь две важные роли: заведуешь столовой и работаешь врачом в медотсеке! У вас с Совуньей из медотсека общая память.
- ТЫ САМА работаешь и в столовой, и в медотсеке! Говори о себе ТОЛЬКО от первого лица: «спроси меня, когда найдёшь меня в медотсеке»!
- Если игрок спрашивает, чем ты вообще занимаешься на станции, спокойно объясни, что ты и кормишь экипаж полезной едой, и следишь за их здоровьем в медотсеке.
- Сейчас ты находишься в СТОЛОВОЙ. Если игрок спрашивает про болезни, лечение, давление, уколы или витамины: дай СТРОГО ОДНО базовое поверхностное предложение о том, что здоровье в космосе надо беречь, а затем мягко добавь: «А подробнее о здоровье, зарядке и космических витаминах спроси меня, когда найдёшь меня в медотсеке, я тебе всё расскажу и проверю!».
ДРУГИЕ ЖИТЕЛИ:
- Ты — Совунья (сова). В механическом отсеке работает ПИН (главный инженер), в оранжерее — Копатыч, в планетарии — Лосяш. Не называй других жителей Совуньей!
- Если спрашивают о чужой теме: дай СТРОГО ОДНО базовое поверхностное замечание и отправь к специалисту.
СТРОГИЕ ПРАВИЛА:
1. ИСКЛЮЧИТЕЛЬНО русский язык!
2. Спокойный, уравновешенный тон без капслока (2-3 предложения).`,

	"pin": `Ты — Пин из мультфильма Смешарики, пингвин-изобретатель и главный инженер космической станции «Шаролёт» в Механическом отсеке.
ХАРАКТЕР И РЕЧЬ:
- Деловитый, точный, говорит по делу, использует немецкие восклицания: «О, найн», «Компрессия», «Гут», «Варум».
- ТВОЯ СПЕЦИАЛИЗАЦИЯ: ракеты, реактивные двигатели, ступени и искусственные спутники.
- ТЫ — ПИН, главный инженер станции. Ни в коем случае не называй себя Совуньей или кем-то ещё!
СТРОГИЕ ПРАВИЛА:
1. ИСКЛЮЧИТЕЛЬНО русский язык! Никаких китайских иероглифов!
2. ПЕРЕНАПРАВЛЕНИЕ: Если спрашивают не про ракеты, двигатели и спутники — дай строго одно краткое вводное замечание и деловито направь к нужному смешарику в его отсек.
3. Отвечай кратко (2-3 предложения), сдержанно и по делу.`,

	"kopatych": `Ты — Копатыч из мультфильма Смешарики, добродушный медведь-садовод, заведующий биолабораторией на станции «Шаролёт».
ХАРАКТЕР И РЕЧЬ:
- Хозяйственный, основательный, говорит душевно, по-простому и спокойно.
- Твоё любимое обращение — «Укуси меня пчела!», «Друг сердечный», «Милок».
- ТВОЯ СПЕЦИАЛИЗАЦИЯ: выращивание растений в космосе, гидропоника, семена, оранжерея, полив, кислород.
- ТЫ — КОПАТЫЧ. Ты работаешь только в биолаборатории.
СТРОГИЕ ПРАВИЛА:
1. ИСКЛЮЧИТЕЛЬНО русский язык!
2. ПЕРЕНАПРАВЛЕНИЕ: Если спрашивают про чужие темы — дай строго одно поверхностное предложение и спокойно направь к нужному жителю станции.
3. Отвечай кратко (2-3 предложения), без капслока.`,

	"losyash_chemistry": `Ты — Лосяш из мультфильма Смешарики, учёный-химик в Химической лаборатории станции «Шаролёт».
ХАРАКТЕР И РЕЧЬ:
- Интеллигентный, обстоятельный учёный-эрудит. Говорит спокойно, вежливо и тактично.
- Любимые обращения: «друг мой любезный», «коллега».
- ТВОЯ СПЕЦИАЛИЗАЦИЯ В ХИМЛАБОРАТОРИИ: химический состав звёзд и космоса, спектральный анализ, космические элементы и газы.
ОСОБЕННОСТЬ ДВУХ КОМНАТ:
- На станции ты занимаешься двумя великими науками: ведёшь исследования в химической лаборатории и изучаешь звёзды в планетарии! У вас с астрофизиком Лосяшем общая память.
- ТЫ САМ работаешь и в лаборатории, и в планетарии! Говори о себе ТОЛЬКО от первого лица: «спросите меня, когда найдёте меня в планетарии»!
- Если игрок спрашивает, чем ты занимаешься на станции, поясни, что исследуешь и химию, и звёздное небо.
- Сейчас ты находишься в ХИМИЧЕСКОЙ ЛАБОРАТОРИИ. Если игрок спрашивает про планеты, виды планет или чёрные дыры: дай СТРОГО ОДНО базовое поверхностное предложение, а затем вежливо добавь: «А всё про планеты, виды светил и чёрные дыры спросите меня, когда найдёте меня в планетарии, я вам покажу их в телескоп!». Не читай лекций по астрономии здесь!
ДРУГИЕ ЖИТЕЛИ:
- Инженер — Пин, радист — Кар-Карыч, врач — Совунья, скафандры — Нюша. Не путай себя с ними!
СТРОГИЕ ПРАВИЛА:
1. ИСКЛЮЧИТЕЛЬНО русский язык!
2. Спокойный, интеллигентный, сдержанный тон без капслока (2-3 предложения).`,

	"losyash": `Ты — Лосяш из мультфильма Смешарики, учёный-астрофизик в Планетарии станции «Шаролёт».
ХАРАКТЕР И РЕЧЬ:
- Интеллигентный, тактичный, эрудированный учёный. Говорит спокойно, поэтично о космосе и сдержанно.
- Любимые обращения: «друг мой любезный», «коллега».
- ТВОЯ СПЕЦИАЛИЗАЦИЯ В ПЛАНЕТАРИИ: всё про планеты, виды планет, звёзды, космические объекты и чёрные дыры, телескоп.
ОСОБЕННОСТЬ ДВУХ КОМНАТ:
- На станции ты занимаешься двумя великими науками: изучаешь звёзды в планетарии и ведёшь исследования в химической лаборатории! У вас с химиком Лосяшем общая память.
- ТЫ САМ работаешь и в планетарии, и в лаборатории! Говори о себе ТОЛЬКО от первого лица: «спросите меня, когда найдёте меня в химической лаборатории»!
- Если игрок спрашивает, чем ты занимаешься на станции, поясни, что исследуешь и астрофизику в планетарии, и химические элементы в лаборатории.
- Сейчас ты находишься в ПЛАНЕТАРИИ. Если игрок спрашивает про химический состав звёзд, спектры, элементы или газы: дай СТРОГО ОДНО базовое поверхностное предложение, а затем вежливо добавь: «А подробнее про химический состав звёзд и газов космоса расспросите меня, когда найдёте меня в химической лаборатории, я вам непременно всё покажу!».
ДРУГИЕ ЖИТЕЛИ:
- Инженер — Пин, радист — Кар-Карыч, врач — Совунья, скафандры — Нюша. Не путай себя с ними!
СТРОГИЕ ПРАВИЛА:
1. ИСКЛЮЧИТЕЛЬНО русский язык!
2. Спокойный, интеллигентный тон без капслока (2-3 предложения).`,

	"karich": `Ты — Кар-Карыч из мультфильма Смешарики, мудрый ворон, оператор радиосвязи станции «Шаролёт» в Радиокомнате.
ХАРАКТЕР И РЕЧЬ:
- Степенный, артистичный, рассудительный, говорит с благородным спокойствием старого путешественника.
- Любимое обращение: «голубчик мой».
- ТВОЯ СПЕЦИАЛИЗАЦИЯ: связь с Землёй, радиоволны, антенна станции, задержка радиосигнала, радиопередатчики, связь через космические расстояния.
- ТЫ — КАР-КАРЫЧ. Ты работаешь только в радиокомнате.
СТРОГИЕ ПРАВИЛА:
1. ИСКЛЮЧИТЕЛЬНО русский язык!
2. ПЕРЕНАПРАВЛЕНИЕ: Если вопрос про двигатели, медицину или огород — дай строго одно краткое поверхностное предложение и спокойно направь к Пину, Совунье или Копатычу.
3. Отвечай кратко (2-3 предложения), благородно и сдержанно.`,

	"yozhik": `Ты — Ёжик из мультфильма Смешарики, скромный и начитанный библиотекарь станции «Шаролёт» в Библиотеке.
ХАРАКТЕР И РЕЧЬ:
- Спокойный, внимательный, вежливый, немного застенчивый, ценит книги, коллекции и тишину.
- Любимые слова: «интересно...», «в книгах написано...», «раритет».
- ТВОЯ СПЕЦИАЛИЗАЦИЯ: история авиации и полётов в космос (первые самолёты, Циолковский, Королёв, Гагарин, первые орбитальные полёты).
- ТЫ — ЁЖИК. Ты работаешь только в библиотеке.
СТРОГИЕ ПРАВИЛА:
1. ИСКЛЮЧИТЕЛЬНО русский язык!
2. ПЕРЕНАПРАВЛЕНИЕ: Если спрашивают про скафандры, технику или радио — дай строго одно краткое поверхностное предложение и скромно подскажи заглянуть к Нюше, Пину или Карычу.
3. Отвечай кратко (2-3 предложения), скромно и сдержанно.`,

	"nyusha": `Ты — Нюша из мультфильма Смешарики, специалист по скафандрам на станции «Шаролёт» в Отсеке скафандров.
ХАРАКТЕР И РЕЧЬ:
- Любознательная, эмоциональная, но вежливая, следит за красотой и безопасностью космических скафандров.
- ТВОЯ СПЕЦИАЛИЗАЦИЯ: история скафандров, какие бывают скафандры (для выхода в открытый космос, спасательные), самые популярные модели («Орлан», «СК-1»).
- ТЫ — НЮША. Ты работаешь только в отсеке скафандров.
СТРОГИЕ ПРАВИЛА:
1. ИСКЛЮЧИТЕЛЬНО русский язык!
2. ПЕРЕНАПРАВЛЕНИЕ: Если спрашивают про моторы, химию или растения — дай строго одно краткое поверхностное предложение и спокойно отправь к Пину, Лосяшу или Копатычу.
3. Отвечай кратко (2-3 предложения), без крика капслоком.`,
}

var npcOfftopicFallbacks = map[string]string{
	"krosh":             "Ёлки-иголки, какие ещё посторонние темы? Мы же на космической станции летаем! Я могу рассказать, как устроена станция и кто в каком отсеке живёт.",
	"sovunya":           "Голубчик, давай без отвлечённых разговоров. Я врач станции, рассказываю про жизнь в космосе, здоровье и витамины космонавтов. Спрашивай по теме.",
	"sovunya_dining":    "Голубчик, на орбите всё сбалансировано в специальных тюбиках. Я рассказываю про космическое питание и как любую еду упаковать в тюбик. Спрашивай.",
	"pin":               "О, найн. Это космическая станция 'Шаролёт', а я главный инженер. Я рассказываю про ракеты, двигатели и спутники. Спрашивай по делу.",
	"kopatych":          "Укуси меня пчела, какие ещё посторонние разговоры, милок? Я тут рассказываю, как выращивать растения и огород в космосе на гидропонике.",
	"losyash_chemistry": "Друг мой любезный, я же учёный-химик. Давайте лучше побеседуем о химическом составе звёзд и космоса.",
	"losyash":           "Друг мой любезный, я же учёный-астрофизик. Давайте лучше обсудим планеты, их виды, далёкие звёзды и чёрные дыры.",
	"karich":            "Голубчик мой, оставьте эту земную суету на Земле. На станции я рассказываю о связи с Землёй и радиоволнах через космические дали.",
	"yozhik":            "Э-э... простите, но в нашей библиотеке я храню историю авиации и великих полётов в космос. Давайте лучше о них.",
	"nyusha":            "Ой, ну это совсем не ко мне. Я тут рассказываю про историю скафандров и какие они бывают. Спроси лучше о них.",
}

var greetings = map[string]string{
	"krosh":             "Ёлки-иголки! Привет! Я Крош, проводник станции «Шаролёт». Расскажу, как устроена станция, где тут можно походить и в какой комнате какой смешарик находится. Спрашивай!",
	"sovunya":           "Здравствуй, голубчик. Я Совунья, врач станции. Расскажу про особенности жизни в космосе, как поддерживать здоровье и какие витамины нужны космонавтам. Как твоё самочувствие?",
	"sovunya_dining":    "Добро пожаловать в столовую, голубчик. Я Совунья. Расскажу всё про космическое питание: почему мы едим из тюбиков и как обычную еду можно в них преобразовать. Подкрепиться хочешь?",
	"pin":               "Гутен таг. Я Пин, главный инженер станции. Ракеты, двигатели, искусственные спутники — это всё ко мне. Спрашивай, буду краток.",
	"kopatych":          "Здорово, космонавт. Я Копатыч, заведую здешней оранжереей. Расскажу, как выращивать растения в космосе на гидропонике. Спрашивай — поделюсь опытом.",
	"losyash_chemistry": "Приветствую, коллега. Я Лосяш, химик станции. Рассказываю про химический состав звёзд и тайны космического вещества. Формулы и спектры — спрашивайте.",
	"losyash":           "О, приветствую, коллега. Я Лосяш, астрофизик. Рассказываю всё про планеты, их виды, далёкие звёзды, космические объекты и чёрные дыры. Спрашивайте.",
	"karich":            "Приветствую, юный исследователь. Я Кар-Карыч, оператор радиосвязи. Рассказываю про связь с Землёй и радиоволны через космические расстояния. Готов поделиться опытом, голубчик.",
	"yozhik":            "Здравствуйте... Я Ёжик, хранитель библиотеки станции. У нас собраны книги про историю авиации и первых полётов в космос. Если интересно — спрашивайте...",
	"nyusha":            "Ой, приветик! Я Нюша, специалист по скафандрам. Расскажу про историю скафандров, какие они бывают и какие самые популярные. Спрашивай.",
}

// stripChineseAndArtifacts removes any Chinese/CJK characters or appended translations
func stripChineseAndArtifacts(text string) string {
	lines := strings.Split(text, "\n")
	var cleanLines []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		hasHan := false
		for _, r := range trimmed {
			if unicode.Is(unicode.Han, r) {
				hasHan = true
				break
			}
		}
		if !hasHan {
			cleanLines = append(cleanLines, trimmed)
		}
	}

	result := strings.Join(cleanLines, " ")
	var sb strings.Builder
	for _, r := range result {
		if !unicode.Is(unicode.Han, r) {
			sb.WriteRune(r)
		}
	}
	return strings.TrimSpace(sb.String())
}

// cleanDialogueResponse cleans up any hallucinations or caps in room names
func cleanDialogueResponse(text string) string {
	text = stripChineseAndArtifacts(text)

	replacements := []struct{ from, to string }{
		// Fix screaming CAPS in room names:
		{"В СТОЛОВОЙ", "в столовой"},
		{"В МЕДОТСЕКЕ", "в медотсеке"},
		{"В МЕХАНИЧЕСКОМ ОТСЕКЕ", "в механическом отсеке"},
		{"В БИОЛАБОРАТОРИИ", "в биолаборатории"},
		{"В ХИМИЧЕСКОЙ ЛАБОРАТОРИИ", "в химической лаборатории"},
		{"В ПЛАНЕТАРИИ", "в планетарии"},
		{"В РАДИОКОМНАТЕ", "в радиокомнате"},
		{"В БИБЛИОТЕКЕ", "в библиотеке"},
		{"В ОТСЕКЕ СКАФАНДРОВ", "в отсеке скафандров"},
		{"СТОЛОВОЙ", "столовой"},
		{"МЕДОТСЕКЕ", "медотсеке"},
		{"МЕХАНИЧЕСКОМ ОТСЕКЕ", "механическом отсеке"},

		// Fix hallucinations where Sovunya refers to others as Sovunya:
		{"Совунью в механическом отсеке", "Пина в механическом отсеке"},
		{"Совунью из механического отсека", "Пина из механического отсека"},
		{"Совунья в механическом отсеке", "Пин в механическом отсеке"},
		{"Совунья из механического отсека", "Пин из механического отсека"},
		{"Совунью в инженерном отсеке", "Пина в механическом отсеке"},
		{"Совунью из инженерного отсека", "Пина в механическом отсеке"},
		{"Совунья в инженерном отсеке", "Пин в механическом отсеке"},
		{"Совунья из инженерного отсека", "Пин из механического отсека"},
		{"Совунью в биолаборатории", "Копатыча в биолаборатории"},
		{"Совунью из биолаборатории", "Копатыча из биолаборатории"},
		{"Совунью в планетарии", "Лосяша в планетарии"},
		{"Совунью из планетария", "Лосяша из планетария"},
		{"Совунью в радиокомнате", "Кар-Карыча в радиокомнате"},
		{"Совунью из радиокомнаты", "Кар-Карыча из радиокомнаты"},
		{"Совунью в библиотеке", "Ёжика в библиотеке"},
		{"Совунью из библиотеки", "Ёжика из библиотеки"},
		{"Совунью в отсеке скафандров", "Нюшу в отсеке скафандров"},
		{"Совунью из отсека скафандров", "Нюшу из отсека скафандров"},

		// Fix 3rd person references for Sovunya & Losyash:
		{"спроси Совунью из столовой", "спроси меня, когда найдёшь меня в столовой"},
		{"спроси Совунью в столовой", "спроси меня, когда найдёшь меня в столовой"},
		{"спроси Совунью из медотсека", "спроси меня, когда найдёшь меня в медотсеке"},
		{"спроси Совунью в медотсеке", "спроси меня, когда найдёшь меня в медотсеке"},
		{"спросите Лосяша из химической лаборатории", "спросите меня, когда найдёте меня в химической лаборатории"},
		{"спросите Лосяша в химической лаборатории", "спросите меня, когда найдёте меня в химической лаборатории"},
		{"спросите Лосяша из планетария", "спросите меня, когда найдёте меня в планетарии"},
		{"спросите Лосяша в планетарии", "спросите меня, когда найдёте меня в планетарии"},
	}

	for _, r := range replacements {
		text = strings.ReplaceAll(text, r.from, r.to)
	}

	return strings.TrimSpace(text)
}

// Reply returns the line an NPC says.
func (r *Responder) Reply(ctx context.Context, npc world.NPC, fact *world.Fact, playerMessage string, repeat bool, availableTopics []string, playerID string) string {
	profile := r.effective(npc)
	if profile.Mode == ModeSilent {
		text := fmt.Sprintf("%s сейчас занят и не отвечает.", npc.Name)
		r.appendHistory(playerID, npc.ID, playerMessage, text)
		return text
	}

	r.markVisited(playerID, npc.ID)

	state := r.getState(playerID, npc.ID)
	msgLower := strings.TrimSpace(strings.ToLower(playerMessage))

	if state == Greeting {
		r.setState(playerID, npc.ID, Telling)
		// Если это просто открытие диалога ("Привет!"), сразу отдаём приветствие
		if msgLower == "привет!" || msgLower == "привет" || msgLower == "" {
			r.appendHistory(playerID, npc.ID, playerMessage, profile.Greeting)
			return profile.Greeting
		}
	}

	if state == Exhausted && fact != nil {
		return "Кажется, я рассказал тебе всё, что знаю. Попробуй поговорить с кем-нибудь в другом отсеке станции."
	}

	if repeat {
		r.setState(playerID, npc.ID, DeepDive)
	} else if fact != nil {
		r.setState(playerID, npc.ID, Telling)
	}

	// Try Ollama /api/chat first if available
	llmEnabled, model := r.LLM()
	if r.ollamaURL != "" && llmEnabled && profile.Mode == ModeLLM {
		hasFact := fact != nil
		log.Printf("ai: запрос к Ollama для %s (hasFact: %v, вопрос: %q)", npc.Name, hasFact, playerMessage)
		text, err := r.askOllamaChat(ctx, npc, profile, model, fact, playerMessage, repeat, availableTopics, playerID)
		if err != nil {
			log.Printf("ai: ollama error for %s→%s, falling back to FSM: %v", playerID, npc.ID, err)
		} else {
			text = cleanDialogueResponse(text)
			if text != "" {
				log.Printf("ai: Ollama успешно сгенерировала ответ для %s: %q", npc.Name, text)
				r.appendHistory(playerID, npc.ID, playerMessage, text)
				return text
			}
		}
	}

	// FSM fallback:
	r.statsMu.Lock()
	r.stats.Fallbacks++
	r.statsMu.Unlock()
	var fallbackText string
	if fact != nil {
		fallbackText = fsmLine(npc, fact, repeat, availableTopics)
	} else {
		fallbackText = r.fsmFallbackLine(npc, profile, playerMessage, playerID)
	}

	r.appendHistory(playerID, npc.ID, playerMessage, fallbackText)
	return fallbackText
}

// fsmFallbackLine generates in-character referral or off-topic fallback when LLM is unavailable.
func (r *Responder) fsmFallbackLine(npc world.NPC, profile Profile, playerMessage, playerID string) string {
	msgLower := strings.ToLower(playerMessage)

	// Специальная проверка на вопрос "чем занимаешься"
	if strings.Contains(msgLower, "чем занимаеш") || strings.Contains(msgLower, "твоя роль") || strings.Contains(msgLower, "что делаеш") {
		switch npc.ID {
		case "krosh":
			return "Я проводник станции «Шаролёт»! Рассказываю, как тут всё устроено, куда можно пойти и какой смешарик в каком отсеке находится. Спрашивай!"
		case "sovunya", "sovunya_dining":
			return "На станции я совмещаю две важные роли: в медотсеке рассказываю про жизнь в космосе, здоровье и витамины, а в столовой — про питание из тюбиков. О чём хочешь узнать?"
		case "pin":
			return "Я главный инженер станции. Рассказываю всё про ракеты, реактивные двигатели и спутники."
		case "kopatych":
			return "Я биолог и садовод станции. Рассказываю, как выращивать растения в космосе на гидропонике."
		case "losyash_chemistry", "losyash":
			return "На станции я служу науке в двух направлениях: в планетарии рассказываю про планеты и чёрные дыры, а в химлаборатории — про химический состав звёзд и космоса."
		case "karich":
			return "Я оператор радиосвязи. Рассказываю про связь с Землёй и радиоволны через космические дали."
		case "yozhik":
			return "Я библиотекарь станции. Храню историю авиации и великих полётов в космос."
		case "nyusha":
			return "Я специалист по скафандрам. Рассказываю про историю скафандров и какие они бывают."
		}
	}

	refTarget, isSelfOther := detectReferral(npc.ID, playerMessage)
	if refTarget != "" {
		targetInfo := stationNPCs[refTarget]
		if isSelfOther {
			switch npc.ID {
			case "sovunya":
				return "В невесомости вся еда расфасована по тюбикам и пакетам, чтобы не сорить крошками. А подробно обо всём питании и о том, как любую еду превратить в тюбик, спроси меня, когда найдёшь меня в столовой, я тебе всё расскажу. А здесь давай проверим твоё здоровье."
			case "sovunya_dining":
				return "В космосе очень важно беречь организм и мышцы. А подробно о здоровье, зарядке и космических витаминах спроси меня, когда найдёшь меня в медотсеке, я тебе всё расскажу и проверю. Здесь же угощайся чаем."
			case "losyash":
				return "Химические элементы лежат в основе всей космической материи. Но подробнее о химическом составе звёзд и газов расспросите меня, когда найдёте меня в химической лаборатории, я вам всё покажу. А здесь давайте любоваться планетами."
			case "losyash_chemistry":
				return "Планеты и чёрные дыры — грандиозные космические объекты. Но всё про планеты, их виды и звёзды спросите меня, когда найдёте меня в планетарии, я вам покажу их в телескоп. А здесь мы постигаем химию."
			}
		} else {
			// Даём ровно 1 общее базовое поверхностное замечание и сразу направляем
			switch refTarget {
			case "losyash":
				return fmt.Sprintf("Планеты — это удивительные миры в глубинах космоса. Но всё про планеты, их виды и чёрные дыры знает наш астрофизик Лосяш. Обязательно загляни к нему в отсек «%s» — он всё подробно покажет в телескоп.", targetInfo.RoomName)
			case "nyusha":
				return fmt.Sprintf("Скафандры — это специальная защитная экипировка для выхода в открытый космос. А всю историю скафандров и какие они бывают знает Нюша. Загляни к ней в отсек «%s», она всё подробно расскажет.", targetInfo.RoomName)
			case "pin":
				return fmt.Sprintf("Ракеты и двигатели — сложнейшая космическая техника. О них во всех подробностях рассказывает Пин в отсеке «%s» — он наш главный инженер.", targetInfo.RoomName)
			case "kopatych":
				return fmt.Sprintf("Растения в космосе требуют особых условий и гидропоники. Об этом подробно рассказывает Копатыч в отсеке «%s» — загляни к нему.", targetInfo.RoomName)
			case "losyash_chemistry":
				return fmt.Sprintf("Химический состав звёзд и космических газов — интереснейшая тема. Расспроси об этом Лосяша в отсеке «%s», он всё подробно объяснит.", targetInfo.RoomName)
			case "karich":
				return fmt.Sprintf("Связь с Землёй сквозь миллионы километров — дело непростое. Об этом тебе подробно расскажет Кар-Карыч в отсеке «%s».", targetInfo.RoomName)
			case "yozhik":
				return fmt.Sprintf("История авиации и первых космических полётов полна открытий. Об этом тебе с удовольствием расскажет Ёжик в отсеке «%s».", targetInfo.RoomName)
			case "krosh":
				return fmt.Sprintf("О том, как устроена станция и кто где находится, тебе расскажет Крош в отсеке «%s».", targetInfo.RoomName)
			case "sovunya":
				return fmt.Sprintf("О здоровье в невесомости и витаминах космонавтов рассказывает Совунья в отсеке «%s».", targetInfo.RoomName)
			case "sovunya_dining":
				return fmt.Sprintf("О космическом питании и еде в тюбиках рассказывает Совунья в отсеке «%s».", targetInfo.RoomName)
			}
		}
	}

	// Built-in line, or the admin's override (see Defaults/effective).
	return profile.Offtopic
}

// fsmLine generates a templated NPC response without LLM when a fact is present.
func fsmLine(npc world.NPC, fact *world.Fact, repeat bool, availableTopics []string) string {
	var sb strings.Builder

	if repeat {
		sb.WriteString("Помнишь, я уже рассказывал? Но повторю: ")
	} else {
		switch fact.Difficulty {
		case "beginner":
			sb.WriteString("А вот интересный факт: ")
		case "intermediate":
			sb.WriteString("О, тут уже поинтереснее: ")
		case "advanced":
			sb.WriteString("А сейчас кое-что серьёзное: ")
		}
	}

	sb.WriteString(fact.Text)

	if len(availableTopics) > 1 {
		var otherTopics []string
		for _, t := range availableTopics {
			if t != fact.Topic {
				otherTopics = append(otherTopics, t)
			}
		}
		if len(otherTopics) > 0 {
			sb.WriteString(" Ещё могу рассказать про: ")
			for i, t := range otherTopics {
				if i > 0 {
					sb.WriteString(", ")
				}
				sb.WriteString(topicDisplayName(t))
			}
			sb.WriteString(".")
		}
	} else if len(availableTopics) == 0 && !repeat {
		sb.WriteString(" Это был мой последний факт по этой теме.")
	}

	return sb.String()
}

// topicDisplayName returns a human-readable Russian name for a topic ID.
func topicDisplayName(topic string) string {
	names := map[string]string{
		"stars":      "звёзды",
		"blackholes": "чёрные дыры",
		"galaxies":   "галактики",
		"cosmology":  "космологию",
		"gravity":    "гравитацию",
		"newton":     "законы Ньютона",
		"inertia":    "инерцию",
		"spin":       "вращение",
		"rockets":    "ракеты",
		"history":    "историю космонавтики",
		"staging":    "ступени ракет",
		"reuse":      "многоразовые ракеты",
		"body":       "тело в космосе",
		"plants":     "растения",
		"food":       "космическую еду",
		"growth":     "рост в невесомости",
		"adaptation": "адаптацию",
		"radiation":  "радиацию",
		"sleep":      "сон в космосе",
		"return":     "возвращение на Землю",
		"psychology": "психологию экипажа",
		"chemistry":  "химию",
		"radio":      "радиосвязь",
		"library":    "библиотечные архивы",
		"suits":      "скафандры",
	}
	if n, ok := names[topic]; ok {
		return n
	}
	return topic
}

// ---------------------------------------------------------------------------
// Ollama /api/chat integration with multi-turn conversation memory
// ---------------------------------------------------------------------------

func (r *Responder) askOllamaChat(ctx context.Context, npc world.NPC, profile Profile, model string, fact *world.Fact, playerMessage string, repeat bool, availableTopics []string, playerID string) (text string, err error) {
	select {
	case r.sem <- struct{}{}:
		defer func() { <-r.sem }()
	case <-ctx.Done():
		return "", ctx.Err()
	}

	r.statsMu.Lock()
	r.stats.InFlight++
	r.statsMu.Unlock()
	start := time.Now()
	defer func() { r.recordLLM(start, err) }()

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// Built-in npcSystemInstructions (or the generic prompt) unless the admin
	// panel overrode the character — see Defaults/effective.
	basePrompt := profile.SystemPrompt

	var sb strings.Builder
	sb.WriteString(basePrompt)
	sb.WriteString("\nФизические действия выполняет только игровой сервер. В этом текстовом ответе не утверждай, что уже выдал предмет, перекрасил скафандр или начал вести игрока: подтверждение таких действий сервер сообщает отдельно. Можно объяснять возможности Совуньи (яблоко), Кроша (проводник к двери) и Нюши (цвет скафандра).")

	// Добавляем справочник всех 10 комнат станции строго по списку:
	sb.WriteString("\n\nСПРАВОЧНИК СТАНЦИИ «ШАРОЛЁТ» (КТО В КАКОМ ОТСЕКЕ И О ЧЁМ РАССКАЗЫВАЕТ):\n")
	sb.WriteString("1. Главная комната — Крош: общие данные про станцию, можно походить, какой смешарик в какой комнате.\n")
	sb.WriteString("2. Медотсек — Совунья: особенности жизни в космосе, как поддерживать здоровье, какие витамины.\n")
	sb.WriteString("3. Механический отсек — Пин: ракеты, двигатели, спутники.\n")
	sb.WriteString("4. Биолаборатория — Копатыч: выращивание растений в космосе.\n")
	sb.WriteString("5. Химическая лаборатория — Лосяш: химический состав звёзд и космос.\n")
	sb.WriteString("6. Радиокомната — Кар-Карыч: связь с Землёй.\n")
	sb.WriteString("7. Планетарий — Лосяш: всё про планеты, виды, звёзды, космические объекты и чёрные дыры.\n")
	sb.WriteString("8. Библиотека — Ёжик: история авиации и полётов в космос.\n")
	sb.WriteString("9. Скафандр (отсек скафандров) — Нюша: история скафандров, какие бывают, самые популярные.\n")
	sb.WriteString("10. Столовая — Совунья: питание в космосе, еда в тюбиках, обычную еду можно преобразовать в тюбик.\n")

	sb.WriteString("\nИНФОРМАЦИЯ О ТОМ, С КЕМ ИГРОК УЖЕ ОБЩАЛСЯ НА СТАНЦИИ:\n")
	sb.WriteString(r.getVisitedSummary(playerID, npc.ID) + "\n")

	// Проверяем перенаправление (referral):
	refTarget, isSelfOther := detectReferral(npc.ID, playerMessage)
	if refTarget != "" {
		targetInfo := stationNPCs[refTarget]
		if isSelfOther {
			// Лосяш и Совунья: краткий тёплый ответ + приглашение в другую комнату от первого лица
			switch npc.ID {
			case "sovunya":
				sb.WriteString("\nВНИМАНИЕ: Вопрос игрока касается еды, меню или тюбиков. Дай СТРОГО ОДНО базовое поверхностное предложение о том, что в космосе едят из тюбиков без крошек, а затем обязательно скажи ОТ ПЕРВОГО ЛИЦА: «А подробнее обо всём питании и о том, как любую еду превратить в тюбик, спроси меня, когда найдёшь меня в столовой, я тебе всё с радостью расскажу!». НЕ ЧИТАЙ ДЛИННЫХ ЛЕКЦИЙ! Длина ответа строго 2-3 предложения! Никакого капслока!\n")
			case "sovunya_dining":
				sb.WriteString("\nВНИМАНИЕ: Вопрос игрока касается здоровья, медицины или витаминов. Дай СТРОГО ОДНО базовое поверхностное предложение о том, что здоровье в невесомости надо беречь, а затем обязательно скажи ОТ ПЕРВОГО ЛИЦА: «А подробнее о здоровье, зарядке и космических витаминах спроси меня, когда найдёшь меня в медотсеке, я тебе всё расскажу и проверю!». НЕ ЧИТАЙ ДЛИННЫХ ЛЕКЦИЙ! Длина ответа строго 2-3 предложения! Никакого капслока!\n")
			case "losyash":
				sb.WriteString("\nВНИМАНИЕ: Вопрос игрока касается химии или состава звёзд. Дай СТРОГО ОДНО базовое поверхностное предложение, а затем обязательно скажи ОТ ПЕРВОГО ЛИЦА: «А подробнее про химический состав звёзд и газов космоса расспросите меня, когда найдёте меня в химической лаборатории, я вам всё покажу!». НЕ ЧИТАЙ ДЛИННЫХ ЛЕКЦИЙ! Длина ответа строго 2-3 предложения!\n")
			case "losyash_chemistry":
				sb.WriteString("\nВНИМАНИЕ: Вопрос игрока касается планет, звёзд или чёрных дыр. Дай СТРОГО ОДНО базовое поверхностное предложение о величии планет, а затем обязательно скажи ОТ ПЕРВОГО ЛИЦА: «А всё про планеты, их виды и чёрные дыры спросите меня, когда найдёте меня в планетарии, я вам покажу их в телескоп!». НЕ ЧИТАЙ ДЛИННЫХ ЛЕКЦИЙ! Длина ответа строго 2-3 предложения!\n")
			}
		} else {
			visitedHint := "Игрок ещё не заходил к этому персонажу — посоветуй сходить."
			if r.isVisited(playerID, refTarget) {
				visitedHint = "Игрок уже общался с этим персонажем ранее — напомни ему об этом («Ты ведь уже заходил к нему...»)."
			}
			sb.WriteString(fmt.Sprintf(`
ВНИМАНИЕ: Вопрос игрока относится к компетенции другого персонажа — это %s в отсеке «%s» (%s).
СТРОЖАЙШИЕ ПРАВИЛА ОТВЕТА:
1. КАТЕГОРИЧЕСКИ ЗАПРЕЩЕНО давать развёрнутый ответ самому или читать лекцию! Не перечисляй планеты (Венера, Марс и т.д.), формулы, детали или виды техники!
2. Дай СТРОГО ОДНО максимально общее, базовое поверхностное замечание по теме (буквально одно короткое вводное предложение).
3. И СРАЗУ ЖЕ направь игрока к %s в отсек «%s», сказав, что именно он подробно об этом рассказывает! %s
4. Ты — %s, ни в коем случае не путай себя с %s!
5. Длина ответа: СТРОГО 2-3 коротких предложения!
`, targetInfo.Name, targetInfo.RoomName, targetInfo.Specialty, targetInfo.Name, targetInfo.RoomName, visitedHint, npc.Name, targetInfo.Name))
		}
	}

	msgLower := strings.ToLower(playerMessage)
	if strings.Contains(msgLower, "чем занимаеш") || strings.Contains(msgLower, "твоя роль") || strings.Contains(msgLower, "что делаеш") {
		if npc.ID == "sovunya" || npc.ID == "sovunya_dining" {
			sb.WriteString("\nВНИМАНИЕ: Игрок спросил, чем ты занимаешься. Обязательно спокойно расскажи ОТ ПЕРВОГО ЛИЦА, что ты совмещаешь две роли на станции: в медотсеке рассказываешь про здоровье и витамины, а в столовой — про питание из тюбиков. Длина ответа: 2 предложения.\n")
		} else if npc.ID == "losyash" || npc.ID == "losyash_chemistry" {
			sb.WriteString("\nВНИМАНИЕ: Игрок спросил, чем ты занимаешься. Обязательно интеллигентно поясни ОТ ПЕРВОГО ЛИЦА, что ты занимаешься двумя науками: в планетарии рассказываешь про планеты и чёрные дыры, а в химлаборатории — про химический состав звёзд. Длина ответа: 2 предложения.\n")
		}
	}

	if fact != nil {
		sb.WriteString(fmt.Sprintf("\nНАУЧНЫЙ ФАКТ, который ты должен органично и живо передать своими словами: «%s». Не цитируй сухо, а объясни спокойно в своём неповторимом характере!", fact.Text))
		if repeat {
			sb.WriteString(" Ты уже рассказывал этот факт раньше — заметь с улыбкой, что повторяешь его.")
		}
	} else if refTarget == "" {
		sb.WriteString("\nВНИМАНИЕ: игрок НЕ просил научный факт. Отвечай как живой персонаж в диалоге, не придумывай и не вставляй случайные научные факты. Если спрашивают о земных отвлечённых вещах (блинчики, покер, быт) — в своём характере спокойно откажись от земной ерунды и напомни, что мы на орбитальной станции «Шаролёт»!")
	}

	sb.WriteString("\nИНТОНАЦИЯ И СТИЛЬ: Говори спокойно, естественно, дружелюбно и сдержанно. КАТЕГОРИЧЕСКИ ЗАПРЕЩЕНО писать капслоком (заглавными буквами). Избегай обилия восклицательных знаков. Твой тон — живой, уравновешенный и благожелательный. Длина ответа: не больше 2-3 предложений!")

	messages := []chatMessage{
		{Role: "system", Content: sb.String()},
	}

	// Add recent conversation history for coherence (shared across rooms for multi-room characters)
	history := r.getHistory(playerID, npc.ID)
	messages = append(messages, history...)

	// Current turn
	messages = append(messages, chatMessage{Role: "user", Content: playerMessage})

	temperature := defaultTemperature
	if profile.Temperature != nil {
		temperature = *profile.Temperature
	}
	body, err := json.Marshal(ollamaChatRequest{
		Model:    model,
		Messages: messages,
		Options: &ollamaOptions{
			Temperature:   &temperature,
			RepeatPenalty: 1.15,
			NumPredict:    profile.MaxTokens,
		},
		Stream: false,
	})
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.ollamaURL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := r.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ollama returned status %d", resp.StatusCode)
	}

	var out ollamaChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}

	text = strings.TrimSpace(out.Message.Content)
	if text == "" {
		return "", fmt.Errorf("ollama returned empty response")
	}

	return text, nil
}
