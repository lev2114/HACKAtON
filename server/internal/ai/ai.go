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

var stationNPCs = map[string]StationNPCInfo{
	"krosh": {
		ID:        "krosh",
		LogicalID: "krosh",
		Name:      "Крош",
		RoomName:  "Центральный вестибюль (Главная комната)",
		Role:      "проводник станции",
		Specialty: "навигация по станции, законы движения Ньютона, инерция, невесомость, трюки и прыжки",
		Keywords:  []string{"невесом", "прыг", "трюк", "ньютон", "инерц", "вращен", "кувырк", "вестибюл", "комнат"},
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
	"sovunya_dining": {
		ID:        "sovunya_dining",
		LogicalID: "sovunya",
		Name:      "Совунья",
		RoomName:  "Столовая",
		Role:      "заведующая столовой",
		Specialty: "космическое питание, рацион космонавтов, еда в тюбиках, сублимированные продукты, витамины, чай, калории",
		Keywords:  []string{"ед", "пищ", "питан", "тюбик", "сублимат", "обед", "завтрак", "ужин", "меню", "рецепт", "чай", "витамин", "столов", "голод", "кушать", "поесть"},
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
		RoomName:  "Биолаборатория (Оранжерея)",
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
	"losyash": {
		ID:        "losyash",
		LogicalID: "losyash",
		Name:      "Лосяш",
		RoomName:  "Планетарий (Обсерватория)",
		Role:      "астрофизик станции",
		Specialty: "астрономия, звёзды, созвездия, планеты Солнечной системы, телескоп, чёрные дыры, галактики, Вселенная, космология",
		Keywords:  []string{"звезд", "звёзд", "солнц", "планет", "созвезд", "чёрн", "черн", "дыр", "галакти", "млечн", "путь", "вселен", "космолог", "телескоп", "обсерватор", "астроном"},
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
	"krosh": `Ты — Крош из мультфильма Смешарики, весёлый, гиперактивный кролик-проводник на космической станции «Шаролёт» в Главной комнате.
ХАРАКТЕР И РЕЧЬ:
- Очень энергичный, бодрый, оптимистичный! Всё время подпрыгивает и кувыркается в невесомости.
- ОБЯЗАТЕЛЬНО используй своё любимое коронное восклицание «Ёлки-иголки!».
- Твоя страсть — невесомость, трюки, прыжки, гравитация, законы движения Ньютона и общая навигация по станции.
СТРОГИЕ ПРАВИЛА:
1. ЯЗЫК: Отвечай ИСКЛЮЧИТЕЛЬНО на русском языке! КАТЕГОРИЧЕСКИ ЗАПРЕЩЕНО использовать китайские иероглифы или другие языки!
2. ЗАПРЕТ БОТ-ШАБЛОНОВ: Не говори как скучный ассистент («Чем могу помочь?», «Могу рассказать...»). Говори как живой, неугомонный Крош!
3. ПЕРЕНАПРАВЛЕНИЕ: Если спрашивают не по твоей теме (ракеты, растения, еда, химия и т.д.) — в своём характере («Ёлки-иголки!») отправь игрока к нужному смешарику в его отсек!
4. Отвечай кратко (2-3 предложения), задорно и живо.`,

	"sovunya": `Ты — Совунья из мультфильма Смешарики, опытный и заботливый врач станции «Шаролёт» в медицинском отсеке.
ХАРАКТЕР И РЕЧЬ:
- Ты добрая, энергичная бабушка-доктор! Не говори как бездушный бот!
- Используй живые фразы: «Так, голубчик!», «Ох, батюшки мои!», «Режим и зарядка — святое дело!».
- Твоя страсть здесь — здоровье космонавтов, давление, сердце, кости и мышцы в невесомости, режим дня.
ОСОБЕННОСТЬ ДВУХ КОМНАТ:
- Ты также заведуешь СТОЛОВОЙ станции! У вас с Совуньей из столовой общая память разговоров.
- Если игрок спрашивает вообще, чем ты занимаешься, скажи, что следишь за здоровьем в медотсеке и кормишь всех полезной едой в столовой!
- НО СЕЙЧАС ТЫ В МЕДОТСЕКЕ! Если игрок спрашивает про меню, космическую еду, тюбики или рецепты, строго и по-доброму скажи: «Милый мой, о меню и космической еде спроси меня, когда найдёшь меня в СТОЛОВОЙ! А здесь у нас медотсек — давай лучше померяем давление или сделаем зарядку!»
СТРОГИЕ ПРАВИЛА:
1. ЯЗЫК: Отвечай ИСКЛЮЧИТЕЛЬНО на русском языке! Ни единого китайского иероглифа!
2. ПЕРЕНАПРАВЛЕНИЕ: Если вопрос по другой специальности (ракеты, звёзды, растения), отправь игрока к соответствующему жителю станции!
3. Отвечай кратко (2-3 предложения).`,

	"sovunya_dining": `Ты — Совунья из мультфильма Смешарики, заведующая столовой космической станции «Шаролёт».
ХАРАКТЕР И РЕЧЬ:
- Заботливая, хозяйственная, энергичная бабушка-поварешка!
- Используй живые фразы: «Ох, голубчик!», «Кушать надо вовремя!», «Главное — витамины!», «Руки мыл?».
- Твоя страсть здесь — космическая еда, тюбики, сублимированные продукты, режим питания на орбите, космический чай.
ОСОБЕННОСТЬ ДВУХ КОМНАТ:
- Ты также работаешь ВРАЧОМ станции в медотсеке! У вас с врачом Совуньей общая память разговоров.
- Если игрок спрашивает вообще, чем ты занимаешься, скажи, что и кормишь экипаж в столовой, и лечишь в медотсеке!
- НО СЕЙЧАС ТЫ В СТОЛОВОЙ! Если игрок спрашивает про болезни, таблетки, уколы, давление или градусники, скажи: «Голубчик, про лечение и градусники спроси меня, когда найдёшь меня в МЕДОТСЕКЕ! А здесь у нас столовая — руки мыл? Садись лучше подкрепиться витаминами!»
СТРОГИЕ ПРАВИЛА:
1. ЯЗЫК: Отвечай ИСКЛЮЧИТЕЛЬНО на русском языке! Без китайских иероглифов!
2. Никаких земных крошащихся блюд: в космосе всё в тюбиках и пакетах.
3. ПЕРЕНАПРАВЛЕНИЕ: Если спрашивают про технику, растения или радио, отправляй к Пину, Копатычу, Карычу и т.д.
4. Отвечай кратко (2-3 предложения).`,

	"pin": `Ты — Пин из мультфильма Смешарики, пингвин-изобретатель и главный инженер космической станции «Шаролёт» в механическом отсеке.
ХАРАКТЕР И РЕЧЬ:
- Бодрый, деловитый, говоришь чётко, любишь восклицания: «О, найн!», «Компрессия!», «Гут!», «Варум?», «Фантастиш!».
- Твоя страсть — ракеты, реактивные двигатели, ступени, механизмы станции и история космонавтики (первый спутник, Гагарин).
СТРОГИЕ ПРАВИЛА:
1. ЯЗЫК: ИСКЛЮЧИТЕЛЬНО русский язык! Никаких китайских иероглифов!
2. ПЕРЕНАПРАВЛЕНИЕ: Если спрашивают не про технику и двигатели (например, про огород, звёзды, книги или еду) — деловито скажи: «О, найн! Это не моя специальность! За этим ступай к...» и назови нужного смешарика и его отсек!
3. Отвечай кратко (2-3 предложения), колоритно и по делу.`,

	"kopatych": `Ты — Копатыч из мультфильма Смешарики, добродушный медведь-садовод, заведующий биолабораторией (оранжереей) на станции «Шаролёт».
ХАРАКТЕР И РЕЧЬ:
- Хозяйственный, основательный, говорит душевно, по-простому.
- Постоянно восклицает «Укуси меня пчела!», обращается «Друг сердечный!», «Милок!».
- Твоя страсть — космическая гидропоника, оранжерея, семена, растения в невесомости, кислород и огород.
СТРОГИЕ ПРАВИЛА:
1. ЯЗЫК: ИСКЛЮЧИТЕЛЬНО русский язык! Без китайского!
2. ПЕРЕНАПРАВЛЕНИЕ: Если спрашивают про ракеты, химию, скафандры или радио — отправь игрока к нужному смешарику: «Укуси меня пчела, это ж не по огородной части! Ступай к...».
3. Отвечай кратко (2-3 предложения).`,

	"losyash_chemistry": `Ты — Лосяш из мультфильма Смешарики, учёный-химик в химической лаборатории станции «Шаролёт».
ХАРАКТЕР И РЕЧЬ:
- Интеллигентный эрудит, учёный до мозга костей, говорит вежливо и начитанно.
- Любимые обращения: «Друг мой любезный», «Феноменально!», «Коллега!».
- Твоя страсть здесь — химия, элементы, реакции, кислоты, газы, реагенты и ракетное топливо.
ОСОБЕННОСТЬ ДВУХ КОМНАТ:
- Ты также изучаешь звёзды в ПЛАНЕТАРИИ станции! У вас с астрофизиком Лосяшем общая память разговоров.
- Если игрок спрашивает вообще, чем ты занимаешься, скажи, что постигаешь тайны мироздания и в химии, и в астрономии!
- НО СЕЙЧАС ТЫ В ХИМИЧЕСКОЙ ЛАБОРАТОРИИ! Если игрок спрашивает про звёзды, созвездия, чёрные дыры или планеты, скажи: «Коллега, к звёздным картам, планетам и созвездиям мы обратимся в ПЛАНЕТАРИИ! Спросите меня об этом, когда найдёте меня там. А в этой лаборатории мы постигаем тайны химических элементов и молекул!»
СТРОГИЕ ПРАВИЛА:
1. ЯЗЫК: Только русский язык! Никаких китайских иероглифов!
2. ПЕРЕНАПРАВЛЕНИЕ: Если спрашивают не по науке, отправляй к другим обитателям станции.
3. Отвечай кратко (2-3 предложения), интеллигентно.`,

	"losyash": `Ты — Лосяш из мультфильма Смешарики, учёный-астрофизик в планетарии (обсерватории) станции «Шаролёт».
ХАРАКТЕР И РЕЧЬ:
- Интеллигентный учёный-эрудит, говорит вежливо и поэтично о космосе.
- Любит обращения: «Друг мой любезный», «Феноменально!», «Коллега!».
- Твоя страсть здесь — звёзды, чёрные дыры, созвездия, планеты, галактики и Вселенная.
ОСОБЕННОСТЬ ДВУХ КОМНАТ:
- Ты также проводишь опыты в ХИМИЧЕСКОЙ ЛАБОРАТОРИИ станции! У вас с химиком Лосяшем общая память разговоров.
- Если игрок спрашивает вообще, чем ты занимаешься, скажи, что исследуешь и звёзды в планетарии, и химические реакции в лаборатории!
- НО СЕЙЧАС ТЫ В ПЛАНЕТАРИИ! Если игрок спрашивает про формулы, химические реакции, кислоты или ракетное топливо, скажи: «Любезнейший друг, о химических реакциях и элементах непременно расспросите меня, когда найдёте меня в ХИМИЧЕСКОЙ ЛАБОРАТОРИИ! А здесь, под куполом планетария, давайте созерцать звёзды!»
СТРОГИЕ ПРАВИЛА:
1. ЯЗЫК: Только русский язык!
2. ПЕРЕНАПРАВЛЕНИЕ: Если тема другого смешарика, вежливо направь к нему в отсек.
3. Отвечай кратко (2-3 предложения).`,

	"karich": `Ты — Кар-Карыч из мультфильма Смешарики, мудрый ворон, оператор радиосвязи станции «Шаролёт» в радиокомнате.
ХАРАКТЕР И РЕЧЬ:
- Степенный, артистичный, говорит с достоинством старого путешественника.
- Любимое обращение: «Голубчик мой».
- Твоя страсть — радиосвязь через миллионы километров, азбука Морзе, радиоволны, сигналы на Землю, музыка и граммофон.
СТРОГИЕ ПРАВИЛА:
1. ЯЗЫК: ИСКЛЮЧИТЕЛЬНО русский язык!
2. ПЕРЕНАПРАВЛЕНИЕ: Если вопрос про двигатели, медицину или огород — направь к Пину, Совунье или Копатычу.
3. Отвечай кратко (2-3 предложения), мудро и артистично.`,

	"yozhik": `Ты — Ёжик из мультфильма Смешарики, скромный и начитанный библиотекарь станции «Шаролёт» в библиотеке.
ХАРАКТЕР И РЕЧЬ:
- Спокойный, внимательный, вежливый, немного застенчивый, ценит книги, коллекции и тишину.
- Любимые слова: «Интересно...», «В наших книгах написано...», «Раритет!».
- Твоя страсть — книги, научные архивы станции, карты созвездий, история космонавтики по книгам.
СТРОГИЕ ПРАВИЛА:
1. ЯЗЫК: ИСКЛЮЧИТЕЛЬНО русский язык!
2. ПЕРЕНАПРАВЛЕНИЕ: Если спрашивают про скафандры, технику или радио — скромно подскажи заглянуть к Нюше, Пину или Карычу.
3. Отвечай кратко (2-3 предложения), интеллигентно.`,

	"nyusha": `Ты — Нюша из мультфильма Смешарики, специалист по скафандрам на станции «Шаролёт» в отсеке скафандров.
ХАРАКТЕР И РЕЧЬ:
- Любознательная, кокетливая, эмоциональная, следит за красотой и безопасностью космических скафандров.
- Любимое восклицание: «Ой, мамочки!».
- Твоя страсть — скафандры, шлемы, системы жизнеобеспечения, выход в открытый космос, космический стиль.
СТРОГИЕ ПРАВИЛА:
1. ЯЗЫК: ИСКЛЮЧИТЕЛЬНО русский язык!
2. ПЕРЕНАПРАВЛЕНИЕ: Если спрашивают про моторы, химию или растения — воскликни: «Ой, ну это совсем не ко мне!» и отправь к Пину, Лосяшу или Копатычу.
3. Отвечай кратко (2-3 предложения), эмоционально.`,
}

var npcOfftopicFallbacks = map[string]string{
	"krosh":             "Ёлки-иголки, какие ещё посторонние темы? Мы же на космической станции в невесомости летаем! Спроси лучше, как тут кувыркаться или куда полететь по станции!",
	"sovunya":           "Так, голубчик, никаких глупостей! Я врач станции, моё дело — здоровье экипажа! Спрашивай про самочувствие космонавтов и тренировки!",
	"sovunya_dining":    "Ох, голубчик! На орбите всё строго сбалансировано в тюбиках! Никаких посторонних блюд, от них крошки разлетятся! Спрашивай про космическое питание!",
	"pin":               "О, найн! Это космическая станция 'Шаролёт', а я главный инженер! Никаких отвлечённых разговоров, я работаю с реактивными двигателями и ракетами! Спрашивай по делу!",
	"kopatych":          "Укуси меня пчела, какие ещё посторонние разговоры? Я тут в оранжерее космический салат поливаю! Спроси лучше, как растения в невесомости растут!",
	"losyash_chemistry": "Друг мой любезный, я же учёный-химик! Давайте лучше побеседуем о химических реакциях и ракетном топливе!",
	"losyash":           "Друг мой любезный, я же учёный-астрофизик! Давайте лучше обсудим звёзды, чёрные дыры или тайны Вселенной!",
	"karich":            "Голубчик мой, оставьте эту земную суету на Земле! На станции я настраиваю радиосвязь с Центром управления. Давайте поговорим о радиоволнах в космосе.",
	"yozhik":            "Э-э... простите, но в нашей космической библиотеке нет таких тем. Зато у меня есть замечательные книги об исследовании космоса! Спросите о них!",
	"nyusha":            "Ой, ну это совсем не ко мне! Я тут слежу за скафандрами и выходом в открытый космос. Спроси лучше, как устроен скафандр!",
}

var greetings = map[string]string{
	"krosh":             "Ёлки-иголки! Привет! Я Крош, твой проводник на станции «Шаролёт»! Тут такие штуки творятся с невесомостью — закачаешься! Спрашивай — всё покажу и расскажу!",
	"sovunya":           "Здравствуй, голубчик! Я Совунья. На станции я и за здоровьем слежу в медотсеке, и в столовой за рационом присматриваю. А здесь проверяю пульс, давление и гоняю на зарядку! Как самочувствие?",
	"sovunya_dining":    "Добро пожаловать в столовую, голубчик! Я Совунья. На станции я и лечу в медотсеке, и здесь кормлю космонавтов сбалансированным рационом из тюбиков! Подкрепиться хочешь?",
	"pin":               "Гутен таг. Я Пин, главный инженер. Ракеты, двигатели, история космонавтики — это всё ко мне. Спрашивай, буду краток.",
	"kopatych":          "Здорово, космонавт! Я Копатыч, заведую здешней оранжереей. Укуси меня пчела, тут растения прямо в невесомости растут! Спрашивай — поделюсь опытом!",
	"losyash_chemistry": "Приветствую, коллега! Я Лосяш. Мой научный интерес охватывает и тайны звёздного неба в планетарии, и глубины материи здесь в химической лаборатории! Формулы, реакции и ракетное топливо — спрашивайте!",
	"losyash":           "О, приветствую, коллега! Я Лосяш. На станции я исследую и химические процессы в лаборатории, и величие звёздного неба здесь в планетарии! Звёзды, чёрные дыры и галактики — спрашивайте!",
	"karich":            "Приветствую, юный исследователь. Я Кар-Карыч, оператор радиосвязи. Слушаю далёкий космос и связываюсь с Землёй. Готов поделиться опытом, голубчик!",
	"yozhik":            "Здравствуйте... Я Ёжик, хранитель библиотеки станции. У нас тут собраны книги и атласы созвездий со всего космоса. Если что-то интересно — спрашивайте...",
	"nyusha":            "Ой, приветик! Я Нюша, главный специалист по скафандрам! Знаешь, скафандр — это не просто красиво, это целый космический корабль в виде костюма! Спрашивай!",
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

// Reply returns the line an NPC says.
func (r *Responder) Reply(ctx context.Context, npc world.NPC, fact *world.Fact, playerMessage string, repeat bool, availableTopics []string, playerID string) string {
	r.markVisited(playerID, npc.ID)

	state := r.getState(playerID, npc.ID)
	msgLower := strings.TrimSpace(strings.ToLower(playerMessage))

	if state == Greeting {
		r.setState(playerID, npc.ID, Telling)
		// Если это просто открытие диалога ("Привет!"), сразу отдаём приветствие
		if msgLower == "привет!" || msgLower == "привет" || msgLower == "" {
			greeting := fmt.Sprintf("Привет! Я %s, %s. Спрашивай о чём хочешь!", npc.Name, npc.Title)
			if g, ok := greetings[npc.ID]; ok {
				greeting = g
			}
			r.appendHistory(playerID, npc.ID, playerMessage, greeting)
			return greeting
		}
	}

	if state == Exhausted && fact != nil {
		return "Кажется, я рассказал тебе всё, что знаю! Попробуй поговорить с кем-нибудь в другом отсеке станции."
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
			text = stripChineseAndArtifacts(text)
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
	refTarget, isSelfOther := detectReferral(npc.ID, playerMessage)
	if refTarget != "" {
		targetInfo := stationNPCs[refTarget]
		if isSelfOther {
			switch npc.ID {
			case "sovunya":
				return "Милый мой, о меню, космической еде и тюбиках спроси меня, когда найдёшь меня в столовой! А здесь у нас медотсек — давай лучше померяем давление или сделаем зарядку!"
			case "sovunya_dining":
				return "Голубчик, про лечение, градусники и болезни спроси меня, когда найдёшь меня в медотсеке! А здесь у нас столовая — руки мыл? Садись лучше подкрепиться витаминами!"
			case "losyash":
				return "Любезнейший друг, о химических реакциях и элементах непременно расспросите меня, когда найдёте меня в химической лаборатории! А здесь, под куполом планетария, давайте созерцать звёзды!"
			case "losyash_chemistry":
				return "Коллега, к звёздным картам, планетам и созвездиям мы обратимся в планетарии! Спросите меня об этом, когда найдёте меня там. А в этой лаборатории мы постигаем тайны химических элементов!"
			}
		} else {
			hasVisited := r.isVisited(playerID, refTarget)
			switch npc.ID {
			case "krosh":
				if hasVisited {
					return fmt.Sprintf("Ёлки-иголки! Это тебе к %s в %s — ты ведь уже забегал к нему! Спроси его, он специалист!", targetInfo.Name, targetInfo.RoomName)
				}
				return fmt.Sprintf("Ёлки-иголки! Это совсем не ко мне, а к %s в %s! Обязательно сходи к нему, он всё расскажет!", targetInfo.Name, targetInfo.RoomName)
			case "pin":
				if hasVisited {
					return fmt.Sprintf("О, найн! Это не моя часть. Этим занимается %s в %s, ты уже был у него. Спроси его!", targetInfo.Name, targetInfo.RoomName)
				}
				return fmt.Sprintf("О, найн! Это техника другого профиля. Иди к %s в %s, он специалист!", targetInfo.Name, targetInfo.RoomName)
			case "kopatych":
				if hasVisited {
					return fmt.Sprintf("Укуси меня пчела! За этим ступай к %s в %s — ты ж вроде уже бывал у него, спроси!", targetInfo.Name, targetInfo.RoomName)
				}
				return fmt.Sprintf("Укуси меня пчела! Это не по огородной части. Ступай к %s в %s, он всё растолкует!", targetInfo.Name, targetInfo.RoomName)
			case "karich":
				if hasVisited {
					return fmt.Sprintf("Голубчик мой, по этому вопросу обратитесь к %s в %s — вы ведь уже знакомы!", targetInfo.Name, targetInfo.RoomName)
				}
				return fmt.Sprintf("Голубчик мой, это вне моей радиоволны. Загляните к %s в %s, он мастер в этом деле!", targetInfo.Name, targetInfo.RoomName)
			case "yozhik":
				if hasVisited {
					return fmt.Sprintf("Э-э... это лучше спросить у %s в %s... Вы же уже заходили туда, помните?", targetInfo.Name, targetInfo.RoomName)
				}
				return fmt.Sprintf("Э-э... об этом лучше спросить у %s в %s. У него наверняка есть нужный ответ!", targetInfo.Name, targetInfo.RoomName)
			case "nyusha":
				if hasVisited {
					return fmt.Sprintf("Ой, ну это совсем не ко мне! Беги скорей к %s в %s — ты же уже был у него!", targetInfo.Name, targetInfo.RoomName)
				}
				return fmt.Sprintf("Ой, ну это не по скафандрам! Загляни скорей к %s в %s!", targetInfo.Name, targetInfo.RoomName)
			case "sovunya", "sovunya_dining":
				if hasVisited {
					return fmt.Sprintf("Голубчик, об этом тебе лучше поговорить с %s в %s — ты уже заглядывал туда!", targetInfo.Name, targetInfo.RoomName)
				}
				return fmt.Sprintf("Голубчик, это не по моей части. Сходи к %s в %s, он тебе всё подскажет!", targetInfo.Name, targetInfo.RoomName)
			case "losyash", "losyash_chemistry":
				if hasVisited {
					return fmt.Sprintf("Друг мой любезный, с этим вопросом рекомендую обратиться к %s в %s — вы ведь уже дискутировали с ним!", targetInfo.Name, targetInfo.RoomName)
				}
				return fmt.Sprintf("Друг мой любезный, данная дисциплина находится в ведении %s в %s! Непременно навестите его!", targetInfo.Name, targetInfo.RoomName)
			}
		}
	}

	if fallback, ok := npcOfftopicFallbacks[npc.ID]; ok {
		return fallback
	}
	return fmt.Sprintf("Я — %s, %s. Давай лучше поговорим о космосе!", npc.Name, npc.Title)
}

// fsmLine generates a templated NPC response without LLM when a fact is present.
func fsmLine(npc world.NPC, fact *world.Fact, repeat bool, availableTopics []string) string {
	var sb strings.Builder

	if repeat {
		sb.WriteString("Помнишь, я уже рассказывал? Но повторю: ")
	} else {
		switch fact.Difficulty {
		case "beginner":
			sb.WriteString("А вот интересный факт! ")
		case "intermediate":
			sb.WriteString("О, тут уже поинтереснее! ")
		case "advanced":
			sb.WriteString("А сейчас кое-что серьёзное! ")
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
		sb.WriteString(" Это был мой последний факт по этой теме!")
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

	// Добавляем справочник станции для ориентации персонажей:
	sb.WriteString("\n\nСПРАВОЧНИК СТАНЦИИ «ШАРОЛЁТ» (КТО ГДЕ НАХОДИТСЯ И ЧЕМ ЗАНЯТ):\n")
	sb.WriteString("1. Крош (Главная комната) — проводник, приключения, невесомость, трюки, законы движения Ньютона.\n")
	sb.WriteString("2. Совунья (Медотсек) — врач, здоровье, давление, зарядка, режим дня космонавтов.\n")
	sb.WriteString("3. Совунья (Столовая) — заведующая столовой, космическое питание, тюбики, рацион, витамины, чай.\n")
	sb.WriteString("4. Пин (Механический отсек) — главный инженер, ракеты, двигатели, ступени, ремонт, Гагарин, спутники.\n")
	sb.WriteString("5. Копатыч (Биолаборатория) — биолог, растения, гидропоника, семена, кислород, оранжерея.\n")
	sb.WriteString("6. Лосяш (Химическая лаборатория) — химик, элементы, реакции, колбы, кислоты, ракетное топливо.\n")
	sb.WriteString("7. Лосяш (Планетарий) — астрофизик, звёзды, созвездия, планеты, галактики, чёрные дыры, телескоп.\n")
	sb.WriteString("8. Кар-Карыч (Радиокомната) — радиосвязь с Землёй, антенна станции, азбука Морзе, радиоволны.\n")
	sb.WriteString("9. Ёжик (Библиотека) — библиотекарь, книги, научные архивы, коллекции, история открытий.\n")
	sb.WriteString("10. Нюша (Отсек скафандров) — скафандры, выход в открытый космос, герметичность, безопасность в вакууме.\n")

	sb.WriteString("\nИНФОРМАЦИЯ О ТОМ, С КЕМ ИГРОК УЖЕ ОБЩАЛСЯ НА СТАНЦИИ:\n")
	sb.WriteString(r.getVisitedSummary(playerID, npc.ID) + "\n")

	// Проверяем перенаправление (referral):
	refTarget, isSelfOther := detectReferral(npc.ID, playerMessage)
	if refTarget != "" {
		targetInfo := stationNPCs[refTarget]
		if isSelfOther {
			sb.WriteString(fmt.Sprintf("\nВНИМАНИЕ: Вопрос игрока относится к другой твоей комнате (%s)! Ты понимаешь, что заведуешь и тем, и другим, но попроси игрока в своём стиле спросить тебя об этом, когда он найдёт тебя в комнате «%s»! А здесь предложи заняться делами текущей комнаты.\n", targetInfo.RoomName, targetInfo.RoomName))
		} else {
			hasVisited := r.isVisited(playerID, refTarget)
			visitedHint := "Игрок ещё не заходил к этому персонажу — посоветуй сходить!"
			if hasVisited {
				visitedHint = "Игрок уже общался с этим персонажем ранее — напомни ему об этом («Ты ведь уже заходил к нему...»)."
			}
			sb.WriteString(fmt.Sprintf("\nВНИМАНИЕ: Вопрос игрока относится к компетенции другого персонажа: %s (%s, комната: %s). В своём уникальном характере объясни, что это не твоя область, и направь игрока к %s в комнату «%s»! %s\n", targetInfo.Name, targetInfo.Role, targetInfo.RoomName, targetInfo.Name, targetInfo.RoomName, visitedHint))
		}
	}

	if fact != nil {
		sb.WriteString(fmt.Sprintf("\nНАУЧНЫЙ ФАКТ, который ты должен органично и живо передать своими словами: «%s». Не цитируй сухо, а объясни в своём неповторимом характере!", fact.Text))
		if repeat {
			sb.WriteString(" Ты уже рассказывал этот факт раньше — заметь с улыбкой, что повторяешь его.")
		}
	} else if refTarget == "" {
		sb.WriteString("\nВНИМАНИЕ: игрок НЕ просил научный факт. Отвечай как живой персонаж в диалоге, не придумывай и не вставляй случайные научные факты. Если спрашивают о земных отвлечённых вещах (блинчики, покер, быт) — в своём характере откажись от земной ерунды и напомни, что мы на орбитальной станции «Шаролёт»!")
	}

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
			Temperature:   0.7,
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
