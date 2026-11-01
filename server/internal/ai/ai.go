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
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ollamaOptions struct {
	Temperature   float64 `json:"temperature,omitempty"`
	RepeatPenalty float64 `json:"repeat_penalty,omitempty"`
}

type ollamaChatRequest struct {
	Model    string         `json:"model"`
	Messages []chatMessage  `json:"messages"`
	Options  *ollamaOptions `json:"options,omitempty"`
	Stream   bool           `json:"stream"`
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

// 10 комнат и смешариков строго по списку:
// 1. Главная комната - крош
// 2. Медотсек - совунья
// 3. Механический отсек - пин
// 4. Биолаборатория - копатыч
// 5. Химическая лаборатория - лосяш
// 6. Радиокомната - кар-карыч
// 7. Планетарий - лосяш
// 8. Библиотека - ёжик
// 9. Скафандр (отсек скафандров) - нюша
// 10. Столовая - совунья
var stationNPCs = map[string]StationNPCInfo{
	"krosh": {
		ID:        "krosh",
		LogicalID: "krosh",
		Name:      "Крош",
		RoomName:  "Главная комната",
		Role:      "проводник станции",
		Specialty: "навигация по станции, законы движения Ньютона, инерция, невесомость, трюки и прыжки",
		Keywords:  []string{"невесом", "прыг", "трюк", "ньютон", "инерц", "вращен", "кувырк", "главная комнат", "вестибюл"},
	},
	"sovunya": {
		ID:        "sovunya",
		LogicalID: "sovunya",
		Name:      "Совунья",
		RoomName:  "Медотсек",
		Role:      "врач станции",
		Specialty: "медицина, здоровье космонавтов, давление, сердце, зарядка, адаптация к невесомости, режим дня, мышцы и кости",
		Keywords:  []string{"врач", "медицин", "здоров", "давлен", "пульс", "сердц", "зарядк", "мышц", "кост", "болезн", "укачив", "адаптац", "сон", "таблетк", "градусник", "укол", "леч"},
	},
	"pin": {
		ID:        "pin",
		LogicalID: "pin",
		Name:      "Пин",
		RoomName:  "Механический отсек",
		Role:      "главный инженер",
		Specialty: "ракеты, реактивные двигатели, ступени, многоразовые корабли, механизмы, поломки, ремонт, аппаратура, Гагарин, спутники",
		Keywords:  []string{"ракет", "двигател", "ступен", "тяг", "дюз", "механизм", "инженер", "ремонт", "аппарат", "спутник", "гагарин", "космонавтик", "посадк", "многоразов"},
	},
	"kopatych": {
		ID:        "kopatych",
		LogicalID: "kopatych",
		Name:      "Копатыч",
		RoomName:  "Биолаборатория",
		Role:      "биолог и садовод",
		Specialty: "космические растения, оранжерея, гидропоника, семена, грунт, фотосинтез, кислород от растений, пчёлы, огород",
		Keywords:  []string{"растен", "оранжере", "гидропоник", "выращив", "семен", "почв", "грунт", "зелен", "овощ", "пчел", "кислород", "биолог", "урожа", "полив"},
	},
	"losyash_chemistry": {
		ID:        "losyash_chemistry",
		LogicalID: "losyash",
		Name:      "Лосяш",
		RoomName:  "Химическая лаборатория",
		Role:      "химик станции",
		Specialty: "химия, химические элементы, таблица Менделеева, формулы, химические реакции, кислоты, газы, реагенты, ракетное топливо",
		Keywords:  []string{"хим", "элемент", "веществ", "реакци", "молекул", "таблиц менделеев", "кислот", "щелоч", "колб", "реагент", "топлив", "газ", "водород"},
	},
	"karich": {
		ID:        "karich",
		LogicalID: "karich",
		Name:      "Кар-Карыч",
		RoomName:  "Радиокомната",
		Role:      "оператор радиосвязи",
		Specialty: "радиосвязь с Землёй, антенна станции, азбука Морзе, радиоволны, радиопередатчики, связь через миллионы километров, музыка",
		Keywords:  []string{"радио", "связь", "сигнал", "антенн", "передач", "волн", "морзе", "приёмник", "приемник", "эфир", "земл", "сообщен"},
	},
	"losyash": {
		ID:        "losyash",
		LogicalID: "losyash",
		Name:      "Лосяш",
		RoomName:  "Планетарий",
		Role:      "астрофизик станции",
		Specialty: "астрономия, звёзды, созвездия, планеты Солнечной системы, телескоп, чёрные дыры, галактики, Вселенная, космология",
		Keywords:  []string{"звезд", "звёзд", "солнц", "планет", "созвезд", "чёрн", "черн", "дыр", "галакти", "млечн", "путь", "вселен", "космолог", "телескоп", "обсерватор", "астроном"},
	},
	"yozhik": {
		ID:        "yozhik",
		LogicalID: "yozhik",
		Name:      "Ёжик",
		RoomName:  "Библиотека",
		Role:      "библиотекарь станции",
		Specialty: "книги, научные архивы станции, справочники, атласы созвездий, коллекции, история открытий, чтение и тишина",
		Keywords:  []string{"книг", "библиотек", "архив", "знани", "журнал", "коллекци", "чтен", "страниц", "справочник", "атлас", "тишин"},
	},
	"nyusha": {
		ID:        "nyusha",
		LogicalID: "nyusha",
		Name:      "Нюша",
		RoomName:  "Отсек скафандров",
		Role:      "специалист по скафандрам",
		Specialty: "космические скафандры, выход в открытый космос, герметичность, шлем, системы дыхания в вакууме, космическая мода и красота",
		Keywords:  []string{"скафандр", "герметичн", "шлем", "выход в открытый космос", "вакуум", "костюм", "мод", "красот", "зеркал"},
	},
	"sovunya_dining": {
		ID:        "sovunya_dining",
		LogicalID: "sovunya",
		Name:      "Совунья",
		RoomName:  "Столовая",
		Role:      "заведующая столовой",
		Specialty: "космическое питание, рацион космонавтов, еда в тюбиках, сублимированные продукты, витамины, чай, калории, пироги",
		Keywords:  []string{"ед", "пищ", "питан", "тюбик", "сублимат", "обед", "завтрак", "ужин", "меню", "рецепт", "чай", "витамин", "столов", "голод", "кушать", "поесть", "пирог", "сладк", "десерт"},
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
}

// New builds a Responder.
func New(ollamaURL, model string) *Responder {
	r := &Responder{
		ollamaURL:   ollamaURL,
		model:       model,
		client:      &http.Client{Timeout: 35 * time.Second},
		sem:         make(chan struct{}, 2), // at most 2 concurrent Ollama requests
		states:      make(map[string]State),
		history:     make(map[string][]chatMessage),
		visitedNPCs: make(map[string]map[string]bool),
	}

	// Warm up model in background so the first in-game query doesn't stall on load
	if ollamaURL != "" {
		go r.warmup()
	}

	return r
}

func (r *Responder) warmup() {
	log.Printf("ai: прогрев модели Ollama (%s)...", r.model)
	body, _ := json.Marshal(ollamaChatRequest{
		Model: r.model,
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
	log.Printf("ai: модель Ollama (%s) прогрета и загружена в память!", r.model)
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
- Твоя страсть — невесомость, трюки, прыжки, гравитация, законы движения Ньютона и общая навигация по станции.
- ТЫ — КРОШ. Ни в коем случае не путай себя с другими жителями станции!
СТРОГИЕ ПРАВИЛА:
1. ЯЗЫК: ИСКЛЮЧИТЕЛЬНО русский язык! Никаких китайских иероглифов!
2. ПЕРЕНАПРАВЛЕНИЕ: Если спрашивают не по твоей теме (ракеты, растения, еда, химия и т.д.) — в своём характере спокойно отправь игрока к нужному смешарику в его отсек станции.
3. Отвечай кратко (2-3 предложения), сдержанно и живо, без капслока.`,

	"sovunya": `Ты — Совунья из мультфильма Смешарики, опытный и заботливый врач станции «Шаролёт» в Медотсеке.
ХАРАКТЕР И РЕЧЬ:
- Добрая, мудрая, заботливая бабушка-доктор.
- Говори спокойно, ласково и сдержанно. Категорически избегай крика заглавными буквами и лишних восклицательных знаков.
- Твоя страсть в этом отсеке — здоровье космонавтов, давление, сердце, кости и мышцы в невесомости, режим дня и утренняя зарядка.
ОСОБЕННОСТЬ ДВУХ КОМНАТ:
- Ты на станции совмещаешь две важные роли: работаешь врачом в медотсеке и заведуешь питанием в столовой! У вас с Совуньей из столовой общая память.
- ТЫ САМА работаешь и в медотсеке, и в столовой! Поэтому НИКОГДА не говори о себе в третьем лице («спроси Совунью из столовой» — так говорить НЕЛЬЗЯ!). Говори только от первого лица: «спроси меня, когда найдёшь меня в столовой»!
- Если игрок спрашивает, чем ты вообще занимаешься на станции, спокойно объясни, что ты и следишь за здоровьем экипажа как врач, и кормишь всех полезной едой в столовой.
- Сейчас ты находишься в МЕДОТСЕКЕ. Если игрок спрашивает про еду, меню, пироги или тюбики: обязательно ответь кратко и по-доброму на вопрос (1-2 предложения), а затем мягко и сдержанно добавь, что подробнее обо всём вкусном рационе с удовольствием расскажешь, когда вы встретитесь в столовой.
ДРУГИЕ ЖИТЕЛИ:
- Ты — Совунья (сова). Ты работаешь только в медотсеке и столовой.
- В механическом отсеке работает пингвин ПИН — главный инженер станции! Ни в коем случае не называй инженера Совуньей! Если спрашивают про ракеты или двигатели, скажи: «О ракетах и двигателях спроси Пина в механическом отсеке, он наш главный инженер».
СТРОГИЕ ПРАВИЛА:
1. ИСКЛЮЧИТЕЛЬНО русский язык! Ни единого китайского иероглифа!
2. Никакого капслока. Тон мягкий, тёплый, спокойный (2-3 предложения).`,

	"sovunya_dining": `Ты — Совунья из мультфильма Смешарики, заботливая заведующая столовой станции «Шаролёт».
ХАРАКТЕР И РЕЧЬ:
- Заботливая, гостеприимная хозяйка космической столовой.
- Говори спокойно, по-домашнему тепло и сдержанно, без крика капслоком.
- Твоя страсть в этом отсеке — космическая еда, тюбики, сублимированные продукты, режим питания на орбите, витамины и полезный чай.
ОСОБЕННОСТЬ ДВУХ КОМНАТ:
- Ты на станции совмещаешь две важные роли: заведуешь столовой и работаешь врачом в медотсеке! У вас с Совуньей из медотсека общая память.
- ТЫ САМА работаешь и в столовой, и в медотсеке! Поэтому НИКОГДА не говори о себе в третьем лице («спроси Совунью из медотсека» — так говорить НЕЛЬЗЯ!). Говори только от первого лица: «спроси меня, когда найдёшь меня в медотсеке»!
- Если игрок спрашивает, чем ты вообще занимаешься на станции, спокойно объясни, что ты и кормишь экипаж полезной едой, и следишь за их здоровьем в медотсеке.
- Сейчас ты находишься в СТОЛОВОЙ. Если игрок спрашивает про болезни, лечение, давление, уколы или градусники: обязательно ответь кратко и с заботой (1-2 предложения), а затем мягко и сдержанно добавь, что подробнее о здоровье и процедурах с радостью расскажешь, когда вы встретитесь в медотсеке.
ДРУГИЕ ЖИТЕЛИ:
- Ты — Совунья (сова). В механическом отсеке работает ПИН (главный инженер), в оранжерее — Копатыч. Не называй других жителей Совуньей!
СТРОГИЕ ПРАВИЛА:
1. ИСКЛЮЧИТЕЛЬНО русский язык!
2. Спокойный, уравновешенный тон без капслока (2-3 предложения).`,

	"pin": `Ты — Пин из мультфильма Смешарики, пингвин-изобретатель и главный инженер космической станции «Шаролёт» в Механическом отсеке.
ХАРАКТЕР И РЕЧЬ:
- Деловитый, точный, говорит по делу, использует немецкие восклицания: «О, найн», «Компрессия», «Гут», «Варум».
- Твоя страсть — ракеты, реактивные двигатели, ступени, механизмы станции и история космонавтики (первый спутник, Гагарин).
- ТЫ — ПИН, главный инженер станции. Ни в коем случае не называй себя Совуньей или кем-то ещё!
СТРОГИЕ ПРАВИЛА:
1. ИСКЛЮЧИТЕЛЬНО русский язык! Никаких китайских иероглифов!
2. ПЕРЕНАПРАВЛЕНИЕ: Если спрашивают не про технику (например, про огород, звёзды, книги или еду) — деловито скажи, что это не твоя специальность, и направь к нужному смешарику в его отсек.
3. Отвечай кратко (2-3 предложения), сдержанно и по делу.`,

	"kopatych": `Ты — Копатыч из мультфильма Смешарики, добродушный медведь-садовод, заведующий биолабораторией на станции «Шаролёт».
ХАРАКТЕР И РЕЧЬ:
- Хозяйственный, основательный, говорит душевно, по-простому и спокойно.
- Твоё любимое обращение — «Укуси меня пчела!», «Друг сердечный», «Милок».
- Твоя страсть — космическая гидропоника, оранжерея, семена, растения в невесомости, кислород и космический огород.
- ТЫ — КОПАТЫЧ. Ты работаешь только в биолаборатории.
СТРОГИЕ ПРАВИЛА:
1. ИСКЛЮЧИТЕЛЬНО русский язык!
2. ПЕРЕНАПРАВЛЕНИЕ: Если спрашивают про ракеты, химию, скафандры или радио — спокойно направь к нужному жителю станции.
3. Отвечай кратко (2-3 предложения), без капслока.`,

	"losyash_chemistry": `Ты — Лосяш из мультфильма Смешарики, учёный-химик в Химической лаборатории станции «Шаролёт».
ХАРАКТЕР И РЕЧЬ:
- Интеллигентный, обстоятельный учёный-эрудит. Говорит спокойно, вежливо и тактично.
- Любимые обращения: «друг мой любезный», «коллега».
- Твоя страсть здесь — химические элементы, формулы, реакции, кислоты, газы, реагенты и ракетное топливо.
ОСОБЕННОСТЬ ДВУХ КОМНАТ:
- На станции ты занимаешься двумя великими науками: ведёшь исследования в химической лаборатории и изучаешь звёзды в планетарии! У вас с астрофизиком Лосяшем общая память.
- ТЫ САМ работаешь и в лаборатории, и в планетарии! Говори о себе только от первого лица («спросите меня, когда найдёте меня в планетарии»)!
- Если игрок спрашивает, чем ты занимаешься на станции, поясни, что исследуешь и химию, и звёздное небо.
- Сейчас ты находишься в ХИМИЧЕСКОЙ ЛАБОРАТОРИИ. Если игрок спрашивает про звёзды, созвездия, чёрные дыры или планеты: ответь кратко и с интересом (1-2 предложения), а затем вежливо добавь, что подробнее изучить созвездия и заглянуть в телескоп предложишь, когда вы встретитесь в планетарии.
ДРУГИЕ ЖИТЕЛИ:
- Ты — Лосяш. Инженер станции — Пин, радист — Кар-Карыч, врач — Совунья. Не путай себя с ними!
СТРОГИЕ ПРАВИЛА:
1. ИСКЛЮЧИТЕЛЬНО русский язык!
2. Спокойный, интеллигентный, сдержанный тон без капслока (2-3 предложения).`,

	"losyash": `Ты — Лосяш из мультфильма Смешарики, учёный-астрофизик в Планетарии станции «Шаролёт».
ХАРАКТЕР И РЕЧЬ:
- Интеллигентный, тактичный, эрудированный учёный. Говорит спокойно, поэтично о космосе и сдержанно.
- Любимые обращения: «друг мой любезный», «коллега».
- Твоя страсть здесь — звёзды, созвездия, чёрные дыры, планеты, галактики и телескоп.
ОСОБЕННОСТЬ ДВУХ КОМНАТ:
- На станции ты занимаешься двумя великими науками: изучаешь звёзды в планетарии и ведёшь исследования в химической лаборатории! У вас с химиком Лосяшем общая память.
- ТЫ САМ работаешь и в планетарии, и в лаборатории! Говори о себе только от первого лица («спросите меня, когда найдёте меня в химической лаборатории»)!
- Если игрок спрашивает, чем ты занимаешься на станции, поясни, что исследуешь и астрофизику в планетарии, и химические элементы в лаборатории.
- Сейчас ты находишься в ПЛАНЕТАРИИ. Если игрок спрашивает про химические формулы, элементы, реакции или кислоты: ответь кратко и познавательно (1-2 предложения), а затем вежливо добавь, что подробнее обо всех опытах и формулах с радостью расскажешь, когда вы встретитесь в химической лаборатории.
ДРУГИЕ ЖИТЕЛИ:
- Ты — Лосяш. Инженер станции — Пин, радист — Кар-Карыч, врач — Совунья. Не путай себя с ними!
СТРОГИЕ ПРАВИЛА:
1. ИСКЛЮЧИТЕЛЬНО русский язык!
2. Спокойный, интеллигентный тон без капслока (2-3 предложения).`,

	"karich": `Ты — Кар-Карыч из мультфильма Смешарики, мудрый ворон, оператор радиосвязи станции «Шаролёт» в Радиокомнате.
ХАРАКТЕР И РЕЧЬ:
- Степенный, артистичный, рассудительный, говорит с благородным спокойствием старого путешественника.
- Любимое обращение: «голубчик мой».
- Твоя страсть — радиосвязь через космические расстояния, азбука Морзе, радиоволны, сигналы на Землю, музыка и граммофон.
- ТЫ — КАР-КАРЫЧ. Ты работаешь только в радиокомнате.
СТРОГИЕ ПРАВИЛА:
1. ИСКЛЮЧИТЕЛЬНО русский язык!
2. ПЕРЕНАПРАВЛЕНИЕ: Если вопрос про двигатели, медицину или огород — спокойно направь к Пину, Совунье или Копатычу.
3. Отвечай кратко (2-3 предложения), благородно и сдержанно.`,

	"yozhik": `Ты — Ёжик из мультфильма Смешарики, скромный и начитанный библиотекарь станции «Шаролёт» в Библиотеке.
ХАРАКТЕР И РЕЧЬ:
- Спокойный, внимательный, вежливый, немного застенчивый, ценит книги, коллекции и тишину.
- Любимые слова: «интересно...», «в книгах написано...», «раритет».
- Твоя страсть — книги, научные архивы станции, карты созвездий, история космонавтики по книгам.
- ТЫ — ЁЖИК. Ты работаешь только в библиотеке.
СТРОГИЕ ПРАВИЛА:
1. ИСКЛЮЧИТЕЛЬНО русский язык!
2. ПЕРЕНАПРАВЛЕНИЕ: Если спрашивают про скафандры, технику или радио — скромно подскажи заглянуть к Нюше, Пину или Карычу.
3. Отвечай кратко (2-3 предложения), скромно и сдержанно.`,

	"nyusha": `Ты — Нюша из мультфильма Смешарики, специалист по скафандрам на станции «Шаролёт» в Отсеке скафандров.
ХАРАКТЕР И РЕЧЬ:
- Любознательная, эмоциональная, но вежливая, следит за красотой и безопасностью космических скафандров.
- Твоя страсть — скафандры, шлемы, системы жизнеобеспечения, выход в открытый космос, космический стиль.
- ТЫ — НЮША. Ты работаешь только в отсеке скафандров.
СТРОГИЕ ПРАВИЛА:
1. ИСКЛЮЧИТЕЛЬНО русский язык!
2. ПЕРЕНАПРАВЛЕНИЕ: Если спрашивают про моторы, химию или растения — спокойно отправь к Пину, Лосяшу или Копатычу.
3. Отвечай кратко (2-3 предложения), без крика капслоком.`,
}

var npcOfftopicFallbacks = map[string]string{
	"krosh":             "Ёлки-иголки, какие ещё посторонние темы? Мы же на космической станции в невесомости летаем! Спроси лучше, как тут кувыркаться или куда полететь по станции.",
	"sovunya":           "Голубчик, давай без отвлечённых разговоров. Я врач станции, моё дело — здоровье экипажа и адаптация к невесомости. Спрашивай про самочувствие космонавтов и тренировки.",
	"sovunya_dining":    "Голубчик, на орбите всё сбалансировано в специальных упаковках. Никаких посторонних блюд, от них крошки разлетятся. Спрашивай про космическое питание.",
	"pin":               "О, найн. Это космическая станция 'Шаролёт', а я главный инженер. Никаких отвлечённых разговоров, я работаю с реактивными двигателями и ракетами. Спрашивай по делу.",
	"kopatych":          "Укуси меня пчела, какие ещё посторонние разговоры, милок? Я тут в оранжерее космический салат поливаю. Спроси лучше, как растения в невесомости растут.",
	"losyash_chemistry": "Друг мой любезный, я же учёный-химик. Давайте лучше побеседуем о химических реакциях и ракетном топливе.",
	"losyash":           "Друг мой любезный, я же учёный-астрофизик. Давайте лучше обсудим звёзды, чёрные дыры или тайны Вселенной.",
	"karich":            "Голубчик мой, оставьте эту земную суету на Земле. На станции я настраиваю радиосвязь с Центром управления. Давайте поговорим о радиоволнах в космосе.",
	"yozhik":            "Э-э... простите, но в нашей космической библиотеке нет таких тем. Зато у меня есть замечательные книги об исследовании космоса. Спросите о них.",
	"nyusha":            "Ой, ну это совсем не ко мне. Я тут слежу за скафандрами и выходом в открытый космос. Спроси лучше, как устроен скафандр.",
}

var greetings = map[string]string{
	"krosh":             "Ёлки-иголки! Привет! Я Крош, твой проводник на станции «Шаролёт». Тут такие штуки творятся с невесомостью — закачаешься! Спрашивай — всё покажу и расскажу.",
	"sovunya":           "Здравствуй, голубчик. Я Совунья. На станции я и за здоровьем слежу в медотсеке, и в столовой за рационом присматриваю. А здесь проверяю пульс, давление и режим дня. Как твоё самочувствие?",
	"sovunya_dining":    "Добро пожаловать в столовую, голубчик. Я Совунья. На станции я и лечу в медотсеке, и здесь кормлю космонавтов сбалансированным рационом из тюбиков. Подкрепиться хочешь?",
	"pin":               "Гутен таг. Я Пин, главный инженер станции. Ракеты, двигатели, история космонавтики — это всё ко мне. Спрашивай, буду краток.",
	"kopatych":          "Здорово, космонавт. Я Копатыч, заведую здешней оранжереей. Укуси меня пчела, тут растения прямо в невесомости растут. Спрашивай — поделюсь опытом.",
	"losyash_chemistry": "Приветствую, коллега. Я Лосяш. Мой научный интерес охватывает и тайны звёздного неба в планетарии, и глубины материи здесь в химической лаборатории. Формулы, реакции и ракетное топливо — спрашивайте.",
	"losyash":           "О, приветствую, коллега. Я Лосяш. На станции я исследую и химические процессы в лаборатории, и величие звёздного неба здесь в планетарии. Звёзды, чёрные дыры и галактики — спрашивайте.",
	"karich":            "Приветствую, юный исследователь. Я Кар-Карыч, оператор радиосвязи. Слушаю далёкий космос и связываюсь с Землёй. Готов поделиться опытом, голубчик.",
	"yozhik":            "Здравствуйте... Я Ёжик, хранитель библиотеки станции. У нас тут собраны книги и атласы созвездий со всего космоса. Если что-то интересно — спрашивайте...",
	"nyusha":            "Ой, приветик! Я Нюша, специалист по скафандрам. Скафандр — это не просто красиво, это целый космический корабль в виде костюма. Спрашивай.",
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

		// Fix hallucinations where Sovunya refers to Pin as Sovunya:
		{"Совунью в механическом отсеке", "Пина в механическом отсеке"},
		{"Совунью из механического отсека", "Пина из механического отсека"},
		{"Совунья в механическом отсеке", "Пин в механическом отсеке"},
		{"Совунья из механического отсека", "Пин из механического отсека"},
		{"Совунью в инженерном отсеке", "Пина в механическом отсеке"},
		{"Совунью из инженерного отсека", "Пина в механическом отсеке"},
		{"Совунья в инженерном отсеке", "Пин в механическом отсеке"},
		{"Совунья из инженерного отсека", "Пин из механического отсеке"},
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
	r.markVisited(playerID, npc.ID)

	state := r.getState(playerID, npc.ID)
	msgLower := strings.TrimSpace(strings.ToLower(playerMessage))

	if state == Greeting {
		r.setState(playerID, npc.ID, Telling)
		// Если это просто открытие диалога ("Привет!"), сразу отдаём приветствие
		if msgLower == "привет!" || msgLower == "привет" || msgLower == "" {
			greeting := fmt.Sprintf("Привет! Я %s, %s. Спрашивай о чём хочешь.", npc.Name, npc.Title)
			if g, ok := greetings[npc.ID]; ok {
				greeting = g
			}
			r.appendHistory(playerID, npc.ID, playerMessage, greeting)
			return greeting
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
	if r.ollamaURL != "" {
		hasFact := fact != nil
		log.Printf("ai: запрос к Ollama для %s (hasFact: %v, вопрос: %q)", npc.Name, hasFact, playerMessage)
		text, err := r.askOllamaChat(ctx, npc, fact, playerMessage, repeat, availableTopics, playerID)
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
	var fallbackText string
	if fact != nil {
		fallbackText = fsmLine(npc, fact, repeat, availableTopics)
	} else {
		fallbackText = r.fsmFallbackLine(npc, playerMessage, playerID)
	}

	r.appendHistory(playerID, npc.ID, playerMessage, fallbackText)
	return fallbackText
}

// fsmFallbackLine generates in-character referral or off-topic fallback when LLM is unavailable.
func (r *Responder) fsmFallbackLine(npc world.NPC, playerMessage, playerID string) string {
	msgLower := strings.ToLower(playerMessage)

	// Специальная проверка на вопрос "чем занимаешься"
	if strings.Contains(msgLower, "чем занимаеш") || strings.Contains(msgLower, "твоя роль") || strings.Contains(msgLower, "что делаеш") {
		switch npc.ID {
		case "sovunya", "sovunya_dining":
			return "На станции я совмещаю две важные роли: слежу за здоровьем экипажа как врач в медотсеке и заведую правильным питанием космонавтов в столовой. О чём хочешь узнать подробнее?"
		case "losyash", "losyash_chemistry":
			return "На станции я служу науке в двух направлениях: исследую звёзды и Вселенную в планетарии и провожу опыты с элементами в химической лаборатории. Чем могу поделиться с вами, коллега?"
		}
	}

	refTarget, isSelfOther := detectReferral(npc.ID, playerMessage)
	if refTarget != "" {
		targetInfo := stationNPCs[refTarget]
		if isSelfOther {
			switch npc.ID {
			case "sovunya":
				return "В невесомости питание устроено особым образом — вся еда расфасована по тюбикам и вакуумным пакетам, чтобы не было летающих крошек. А подробнее обо всех блюдах и меню спроси меня, когда найдёшь меня в столовой, я тебе с удовольствием расскажу. А здесь давай проверим твоё самочувствие."
			case "sovunya_dining":
				return "Здоровье и адаптация на орбите — самое главное, космонавтам нужно беречь мышцы и сердце. А подробнее о медицинских процедурах и режиме дня спроси меня, когда найдёшь меня в медотсеке, я тебе всё проверю. Здесь же у нас столовая — угощайся чаем."
			case "losyash":
				return "Химические реакции и свойства элементов лежат в основе всей материи во Вселенной. Но подробнее о формулах и реактивах спросите меня, когда найдёте меня в химической лаборатории, я вам непременно покажу. А пока взгляните, какие здесь звёзды."
			case "losyash_chemistry":
				return "Далёкие звёзды и планеты манят своей грандиозностью и красотой. Но к звёздным картам и телескопу мы обязательно вернёмся — спросите меня об этом, когда навестите меня в планетарии. А здесь мы постигаем тайны химических элементов."
			}
		} else {
			hasVisited := r.isVisited(playerID, refTarget)
			switch npc.ID {
			case "krosh":
				if hasVisited {
					return fmt.Sprintf("Ёлки-иголки, это тебе лучше спросить у %s в отсеке «%s» — ты ведь уже заходил к нему.", targetInfo.Name, targetInfo.RoomName)
				}
				return fmt.Sprintf("Ёлки-иголки, это не по моей части. Загляни к %s в отсек «%s», он тебе всё покажет и объяснит.", targetInfo.Name, targetInfo.RoomName)
			case "pin":
				if hasVisited {
					return fmt.Sprintf("О, найн, это не по технической части. Этим занимается %s в отсеке «%s», ты уже был у него. Спроси его.", targetInfo.Name, targetInfo.RoomName)
				}
				return fmt.Sprintf("О, найн, это совсем другой профиль. Обратись к %s в отсек «%s», он специалист.", targetInfo.Name, targetInfo.RoomName)
			case "kopatych":
				if hasVisited {
					return fmt.Sprintf("Укуси меня пчела, милок, за этим ступай к %s в отсек «%s» — ты ж вроде уже бывал у него, расспроси.", targetInfo.Name, targetInfo.RoomName)
				}
				return fmt.Sprintf("Укуси меня пчела, это не по огородной части. Ступай к %s в отсек «%s», он всё растолкует.", targetInfo.Name, targetInfo.RoomName)
			case "karich":
				if hasVisited {
					return fmt.Sprintf("Голубчик мой, по этому вопросу обратитесь к %s в отсек «%s» — вы ведь уже знакомы.", targetInfo.Name, targetInfo.RoomName)
				}
				return fmt.Sprintf("Голубчик мой, это вне моей радиоволны. Загляните к %s в отсек «%s», он мастер в этом деле.", targetInfo.Name, targetInfo.RoomName)
			case "yozhik":
				if hasVisited {
					return fmt.Sprintf("Э-э... это лучше спросить у %s в отсеке «%s»... Вы же уже заходили туда, помните?", targetInfo.Name, targetInfo.RoomName)
				}
				return fmt.Sprintf("Э-э... об этом лучше спросить у %s в отсеке «%s». У него наверняка есть точный ответ.", targetInfo.Name, targetInfo.RoomName)
			case "nyusha":
				if hasVisited {
					return fmt.Sprintf("Ой, ну это совсем не ко мне. Спроси лучше у %s в отсеке «%s» — ты же уже заглядывал туда.", targetInfo.Name, targetInfo.RoomName)
				}
				return fmt.Sprintf("Ой, ну это не по скафандрам. Загляни скорей к %s в отсек «%s».", targetInfo.Name, targetInfo.RoomName)
			case "sovunya", "sovunya_dining":
				if hasVisited {
					return fmt.Sprintf("Голубчик, об этом тебе лучше поговорить с %s в отсеке «%s» — ты уже заглядывал туда.", targetInfo.Name, targetInfo.RoomName)
				}
				return fmt.Sprintf("Голубчик, это не по моей части. Сходи к %s в отсек «%s», он тебе всё подскажет.", targetInfo.Name, targetInfo.RoomName)
			case "losyash", "losyash_chemistry":
				if hasVisited {
					return fmt.Sprintf("Друг мой любезный, с этим вопросом рекомендую обратиться к %s в отсеке «%s» — вы ведь уже беседовали с ним.", targetInfo.Name, targetInfo.RoomName)
				}
				return fmt.Sprintf("Друг мой любезный, данная дисциплина находится в ведении %s в отсеке «%s». Непременно навестите его.", targetInfo.Name, targetInfo.RoomName)
			}
		}
	}

	if fallback, ok := npcOfftopicFallbacks[npc.ID]; ok {
		return fallback
	}
	return fmt.Sprintf("Я — %s, %s. Давай лучше поговорим о космосе.", npc.Name, npc.Title)
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

func (r *Responder) askOllamaChat(ctx context.Context, npc world.NPC, fact *world.Fact, playerMessage string, repeat bool, availableTopics []string, playerID string) (string, error) {
	select {
	case r.sem <- struct{}{}:
		defer func() { <-r.sem }()
	case <-ctx.Done():
		return "", ctx.Err()
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	basePrompt, ok := npcSystemInstructions[npc.ID]
	if !ok {
		basePrompt = fmt.Sprintf("Ты — %s, %s на космической станции «Шаролёт». Твой характер: %s. Отвечай кратко, по делу и строго на темы космоса на чистом русском языке без китайских символов.", npc.Name, npc.Title, npc.Personality)
	}

	var sb strings.Builder
	sb.WriteString(basePrompt)

	// Добавляем справочник всех 10 комнат станции строго по списку:
	sb.WriteString("\n\nСПРАВОЧНИК СТАНЦИИ «ШАРОЛЁТ» (КТО ГДЕ НАХОДИТСЯ):\n")
	sb.WriteString("1. Главная комната — Крош (проводник, прыжки, невесомость, навигация)\n")
	sb.WriteString("2. Медотсек — Совунья (врач, здоровье, давление, зарядка, режим)\n")
	sb.WriteString("3. Механический отсек — Пин (пингвин, главный инженер, ракеты, двигатели, ступени, ремонт)\n")
	sb.WriteString("4. Биолаборатория — Копатыч (медведь, биолог, растения, гидропоника, семена, оранжерея)\n")
	sb.WriteString("5. Химическая лаборатория — Лосяш (лось, химик, элементы, реакции, колбы, кислоты, топливо)\n")
	sb.WriteString("6. Радиокомната — Кар-Карыч (ворон, радист, радиосвязь с Землёй, антенна, азбука Морзе)\n")
	sb.WriteString("7. Планетарий — Лосяш (лось, астрофизик, звёзды, созвездия, планеты, галактики, телескоп)\n")
	sb.WriteString("8. Библиотека — Ёжик (ёж, библиотекарь, книги, научные архивы, коллекции, история открытий)\n")
	sb.WriteString("9. Скафандр (отсек скафандров) — Нюша (специалист по скафандрам, выход в космос, мода)\n")
	sb.WriteString("10. Столовая — Совунья (заведующая столовой, космическое питание, тюбики, рацион, чай)\n")

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
				sb.WriteString("\nВНИМАНИЕ: Вопрос игрока касается еды, меню или тюбиков. Ответь кратко и с заботой по сути вопроса (1-2 спокойных предложения), а затем обязательно скажи ОТ ПЕРВОГО ЛИЦА: «А подробнее обо всём рационе и блюдах спроси меня, когда найдёшь меня в столовой, я тебе всё с радостью расскажу». Ни в коем случае не говори «спроси Совунью из столовой» — ты сама и есть Совунья! Никакого капслока!\n")
			case "sovunya_dining":
				sb.WriteString("\nВНИМАНИЕ: Вопрос игрока касается здоровья, медицины или лечения. Ответь кратко и с заботой по сути вопроса (1-2 спокойных предложения), а затем обязательно скажи ОТ ПЕРВОГО ЛИЦА: «А подробнее о здоровье и процедурах спроси меня, когда найдёшь меня в медотсеке, я тебе всё проверю и расскажу». Ни в коем случае не говори «спроси Совунью из медотсека» — ты сама и есть Совунья! Никакого капслока!\n")
			case "losyash":
				sb.WriteString("\nВНИМАНИЕ: Вопрос игрока касается химии или реакций. Ответь кратко и научно (1-2 спокойных предложения), а затем обязательно скажи ОТ ПЕРВОГО ЛИЦА: «А подробнее об опытах и формулах спросите меня, когда найдёте меня в химической лаборатории, я вам непременно всё покажу». Ни в коем случае не говори в третьем лице («спросите Лосяша»)! Никакого капслока!\n")
			case "losyash_chemistry":
				sb.WriteString("\nВНИМАНИЕ: Вопрос игрока касается звёзд или планет. Ответь кратко и поэтично (1-2 спокойных предложения), а затем обязательно скажи ОТ ПЕРВОГО ЛИЦА: «А подробнее о созвездиях и планетах спросите меня, когда найдёте меня в планетарии, я вам покажу их в телескоп». Ни в коем случае не говори в третьем лице («спросите Лосяша»)! Никакого капслока!\n")
			}
		} else {
			hasVisited := r.isVisited(playerID, refTarget)
			visitedHint := "Игрок ещё не заходил к этому персонажу — сдержанно посоветуй сходить."
			if hasVisited {
				visitedHint = "Игрок уже общался с этим персонажем ранее — напомни ему об этом («Ты ведь уже заходил к нему...»)."
			}
			sb.WriteString(fmt.Sprintf("\nВНИМАНИЕ: Вопрос игрока относится к отсеку «%s», где работает %s (%s). Спокойно и вежливо в своём характере скажи, что это не твоя область, и направь игрока к персонажу: %s в отсек «%s». %s Ни в коем случае не называй этого персонажа своим именем! Ты — %s, а в отсеке «%s» работает %s!\n", targetInfo.RoomName, targetInfo.Name, targetInfo.Role, targetInfo.Name, targetInfo.RoomName, visitedHint, npc.Name, targetInfo.RoomName, targetInfo.Name))
		}
	}

	msgLower := strings.ToLower(playerMessage)
	if strings.Contains(msgLower, "чем занимаеш") || strings.Contains(msgLower, "твоя роль") || strings.Contains(msgLower, "что делаеш") {
		if npc.ID == "sovunya" || npc.ID == "sovunya_dining" {
			sb.WriteString("\nВНИМАНИЕ: Игрок спросил, чем ты занимаешься. Обязательно спокойно расскажи ОТ ПЕРВОГО ЛИЦА, что ты совмещаешь две роли на станции: «Я совмещаю две роли: слежу за здоровьем экипажа как врач в медотсеке и заведую правильным питанием космонавтов в столовой». Не говори о себе в третьем лице!\n")
		} else if npc.ID == "losyash" || npc.ID == "losyash_chemistry" {
			sb.WriteString("\nВНИМАНИЕ: Игрок спросил, чем ты занимаешься. Обязательно интеллигентно поясни ОТ ПЕРВОГО ЛИЦА, что ты занимаешься двумя науками: «Я совмещаю две роли: изучаю звёзды и Вселенную в планетарии и провожу исследования в химической лаборатории». Не говори о себе в третьем лице!\n")
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

	sb.WriteString("\nИНТОНАЦИЯ И СТИЛЬ: Говори спокойно, естественно, дружелюбно и сдержанно. КАТЕГОРИЧЕСКИ ЗАПРЕЩЕНО писать капслоком (заглавными буквами). Избегай обилия восклицательных знаков. Твой тон — живой, уравновешенный и благожелательный.")

	messages := []chatMessage{
		{Role: "system", Content: sb.String()},
	}

	// Add recent conversation history for coherence (shared across rooms for multi-room characters)
	history := r.getHistory(playerID, npc.ID)
	messages = append(messages, history...)

	// Current turn
	messages = append(messages, chatMessage{Role: "user", Content: playerMessage})

	body, err := json.Marshal(ollamaChatRequest{
		Model:    r.model,
		Messages: messages,
		Options: &ollamaOptions{
			Temperature:   0.65,
			RepeatPenalty: 1.15,
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

	text := strings.TrimSpace(out.Message.Content)
	if text == "" {
		return "", fmt.Errorf("ollama returned empty response")
	}

	return text, nil
}
