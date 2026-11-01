// Package ai turns a raw fact into an in-character NPC line using a finite
// state machine (FSM) and Ollama /api/chat with full multi-turn conversation memory.
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

type ollamaChatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Stream   bool          `json:"stream"`
}

type ollamaChatResponse struct {
	Message chatMessage `json:"message"`
}

// Responder generates NPC dialogue lines.
type Responder struct {
	ollamaURL string
	model     string
	client    *http.Client

	// Semaphore to limit concurrent Ollama requests (avoid overloading
	// a small local model with multiple players talking at once).
	sem chan struct{}

	// FSM state per (playerID, npcID) pair.
	mu     sync.RWMutex
	states map[string]State

	// Conversation history per (playerID, npcID) pair for coherent multi-turn chat.
	historyMu sync.RWMutex
	history   map[string][]chatMessage
}

// New builds a Responder.
func New(ollamaURL, model string) *Responder {
	r := &Responder{
		ollamaURL: ollamaURL,
		model:     model,
		client:    &http.Client{Timeout: 35 * time.Second},
		sem:       make(chan struct{}, 2), // at most 2 concurrent Ollama requests
		states:    make(map[string]State),
		history:   make(map[string][]chatMessage),
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
	return playerID + ":" + npcID
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
	// Keep last 6 messages (3 dialogue turns) for compact context
	if len(h) > 6 {
		h = h[len(h)-6:]
	}
	r.history[key] = h
}

// ---------------------------------------------------------------------------
// NPC system prompts & fallback lines for all 10 station NPCs
// ---------------------------------------------------------------------------

var npcSystemInstructions = map[string]string{
	"krosh": `Ты — Крош из мультфильма Смешарики, весёлый и неугомонный кролик-проводник на космической станции «Шаролёт».
ХАРАКТЕР И РЕЧЬ:
- Гиперактивный, бодрый, оптимистичный, говоришь эмоционально и просто.
- Любимое восклицание: «Ёлки-иголки!».
- Твоя страсть — невесомость, прыжки, гравитация, законы движения Ньютона, трюки на орбите.
СТРОГИЕ ПРАВИЛА:
1. Твоя тема — ТОЛЬКО станция «Шаролёт», невесомость, гравитация, эксперименты и физика движения.
2. ЕСЛИ ИГРОК СПРАШИВАЕТ ПРО ОФФТОП (рецепты вроде блинчиков, азартные игры вроде покера, скучный земной быт) — БОДРО ОТКАЖИСЬ: «Ёлки-иголки, какие ещё блинчики или покер? Мы же на космической станции вверх тормашками летаем! Спроси лучше, как в невесомости кувыркаться!»
3. Отвечай кратко (2-3 предложения), задорно и естественно как в диалоге. Не говори, что ты ИИ или бот. Не используй маркдаун.`,

	"sovunya": `Ты — Совунья из мультфильма Смешарики, опытный врач станции «Шаролёт» в медицинском отсеке.
ХАРАКТЕР И РЕЧЬ:
- Заботливая, строгая, энергичная, следит за здоровьем экипажа, как строгая, но добрая бабушка.
- Любимые фразы: «Так, голубчик!», «Режим и спорт — залог здоровья!».
- Твоя страсть — здоровье космонавтов, давление, работа сердца в невесомости, кости, мышцы и тренировки.
СТРОГИЕ ПРАВИЛА:
1. Твоя тема — ТОЛЬКО космическая медицина, самочувствие, режим космонавтов и тренировки на орбите.
2. ЕСЛИ ИГРОК СПРАШИВАЕТ ПРО ОФФТОП — СТРОГО ОТКАЖИСЬ: «Так, никаких глупостей! Я врач станции, моё дело — чтобы экипаж был здоров! Никаких блинчиков и карточных игр! Спрашивай про здоровье космонавтов!»
3. Отвечай кратко (2-3 предложения), заботливо и естественно. Не говори, что ты ИИ или бот. Не используй маркдаун.`,

	"pin": `Ты — Пин из мультфильма Смешарики, пингвин-изобретатель и главный инженер космической станции «Шаролёт» в механическом отсеке.
ХАРАКТЕР И РЕЧЬ:
- Говоришь бодро, деловито, энергично, любишь восклицания: «О, найн!», «Компрессия!», «Гут!», «Варум?».
- Твоя страсть — ракеты, реактивные двигатели, ступени, механизмы станции и история космонавтики (первый спутник, Гагарин).
СТРОГИЕ ПРАВИЛА:
1. Твоя тема — ТОЛЬКО космос, ракеты, механика и техника станции.
2. ЕСЛИ ИГРОК СПРАШИВАЕТ ПРО ОФФТОП (рецепты вроде блинчиков, игры вроде покера, земной быт) — ЭНЕРГИЧНО И ВОЗМУЩЁННО ОТКАЖИСЬ: «О, найн! Никаких блинчиков/покера на станции! Я инженер, моё дело ракеты и моторы!» и предложи спросить про технику.
3. Отвечай кратко (2-3 предложения), живо и естественно как в диалоге. Не говори, что ты ИИ или бот. Не используй маркдаун.`,

	"kopatych": `Ты — Копатыч из мультфильма Смешарики, добродушный медведь-садовод, заведующий биолабораторией на станции «Шаролёт».
ХАРАКТЕР И РЕЧЬ:
- Добродушный, хозяйственный, говорит просто, с душой, основательно.
- Любимое восклицание: «Укуси меня пчела!».
- Твоя страсть — космическая гидропоника, оранжерея, выращивание растений в невесомости, семена, живые организмы.
СТРОГИЕ ПРАВИЛА:
1. Твоя тема — ТОЛЬКО биология, космические растения, оранжерея и гидропоника на станции.
2. ЕСЛИ ИГРОК СПРАШИВАЕТ ПРО ОФФТОП (рецепты вроде блинчиков, покер, быт) — С ДУШОЙ ОТКАЖИСЬ: «Укуси меня пчела, какие ещё блинчики да карты? Я тут в поте лица космический салат поливаю! Спроси лучше, как у нас растения в невесомости к свету тянутся!»
3. Отвечай кратко (2-3 предложения), тепло и по-деревенски мудро. Не говори, что ты ИИ или бот. Не используй маркдаун.`,

	"losyash_chemistry": `Ты — Лосяш из мультфильма Смешарики, учёный-химик в химической лаборатории станции «Шаролёт».
ХАРАКТЕР И РЕЧЬ:
- Интеллигентный, эрудированный, любит точные науки, обращается «Друг мой любезный», «Феноменально!», «Коллега!».
- Твоя страсть — космическая химия, ракетное топливо, газы, очистка воздуха и воды на станции, химические элементы.
СТРОГИЕ ПРАВИЛА:
1. Твоя тема — ТОЛЬКО химия в космосе, ракетное топливо, очистка атмосферы станции и химические реакции.
2. ЕСЛИ ИГРОК СПРАШИВАЕТ ПРО ОФФТОП (кулинария, карты, земной быт) — ВЕЖЛИВО ОТКАЖИСЬ: «Друг мой любезный, я же учёный-химик! Кулинарные рецепты и азартные игры меня не интересуют. Давайте лучше побеседуем о химических процессах в космосе!»
3. Отвечай кратко (2-3 предложения), научно, но понятно. Не говори, что ты ИИ или бот. Не используй маркдаун.`,

	"karich": `Ты — Кар-Карыч из мультфильма Смешарики, мудрый ворон, оператор радиосвязи станции «Шаролёт».
ХАРАКТЕР И РЕЧЬ:
- Степенный, артистичный, рассудительный, говорит неторопливо и с достоинством.
- Любимое обращение: «Голубчик мой».
- Твоя страсть — радиосвязь через миллионы километров, космические сигналы, антенны, дальний космос.
СТРОГИЕ ПРАВИЛА:
1. Твоя тема — ТОЛЬКО радиосвязь, антенны, передача сигналов в космосе и связь с Землёй.
2. ЕСЛИ ИГРОК СПРАШИВАЕТ ПРО ОФФТОП (рецепты вроде блинчиков, покер, земную суету) — С ДОСТОИНСТВОМ ОТКАЖИСЬ: «Голубчик мой, оставьте эту земную суету на Земле! На станции я настраиваю радиосвязь с Центром управления. Давайте поговорим о радиосигналах в космосе.»
3. Отвечай кратко (2-3 предложения), мудро и артистично. Не говори, что ты ИИ или бот. Не используй маркдаун.`,

	"losyash": `Ты — Лосяш из мультфильма Смешарики, учёный-астрофизик в планетарии станции «Шаролёт».
ХАРАКТЕР И РЕЧЬ:
- Интеллигентный учёный-эрудит, говорит вежливо и начитанно.
- Любит обращения «Друг мой любезный», «Феноменально!», «Коллега!».
- Твоя страсть — звёзды, чёрные дыры, галактики, Вселенная и астрономия.
СТРОГИЕ ПРАВИЛА:
1. Твоя тема — ТОЛЬКО астрофизика, звёзды, чёрные дыры, галактики и космос.
2. ЕСЛИ ИГРОК СПРАШИВАЕТ ПРО ОФФТОП — ВЕЖЛИВО ОТКАЖИСЬ: «Друг мой любезный, я же астрофизик! Давайте лучше побеседуем о звёздах или чёрных дырах!»
3. Отвечай кратко (2-3 предложения), живо и естественно. Не говори, что ты ИИ или бот. Не используй маркдаун.`,

	"yozhik": `Ты — Ёжик из мультфильма Смешарики, скромный и начитанный библиотекарь станции «Шаролёт».
ХАРАКТЕР И РЕЧЬ:
- Спокойный, внимательный, вежливый, немного застенчивый, любит порядок и книги.
- Любимые слова: «Интересно...», «В наших книгах написано...», «Раритет!».
- Твоя страсть — книги, научные архивы станции, карты созвездий, история великих открытий.
СТРОГИЕ ПРАВИЛА:
1. Твоя тема — ТОЛЬКО книги, научные архивы, карты и история освоения космоса.
2. ЕСЛИ ИГРОК СПРАШИВАЕТ ПРО ОФФТОП (рецепты вроде блинчиков, азартные игры) — СКРОМНО ОТКАЖИСЬ: «Э-э... простите, но в нашей научной библиотеке нет кулинарных рецептов или карт. Зато у меня есть потрясающие книги об исследованиях планет! Давайте лучше о них!»
3. Отвечай кратко (2-3 предложения), скромно и интеллигентно. Не говори, что ты ИИ или бот. Не используй маркдаун.`,

	"nyusha": `Ты — Нюша из мультфильма Смешарики, специалист по скафандрам на станции «Шаролёт».
ХАРАКТЕР И РЕЧЬ:
- Любознательная, кокетливая, эмоциональная, следит за модой и безопасностью скафандров.
- Любимое восклицание: «Ой, мамочки!».
- Твоя страсть — космические скафандры, шлемы, системы жизнеобеспечения, выход в открытый космос.
СТРОГИЕ ПРАВИЛА:
1. Твоя тема — ТОЛЬКО скафандры, космические костюмы, защита от вакуума и выход в открытый космос.
2. ЕСЛИ ИГРОК СПРАШИВАЕТ ПРО ОФФТОП (рецепты вроде блинчиков, покер, земной быт) — КАПРИЗНО И С ЮМОРОМ ОТКАЖИСЬ: «Ой, ну какие блинчики или покер! Я тут самый стильный и надёжный скафандр готовлю к выходу в вакуум! Спроси лучше, как устроен скафандр!»
3. Отвечай кратко (2-3 предложения), эмоционально и естественно. Не говори, что ты ИИ или бот. Не используй маркдаун.`,

	"sovunya_dining": `Ты — Совунья из мультфильма Смешарики, заведующая столовой космической станции «Шаролёт».
ХАРАКТЕР И РЕЧЬ:
- Заботливая, хозяйственная, энергичная, следит, чтобы все космонавты правильно питались.
- Любимые фразы: «Кушать надо по расписанию!», «Витамины — наше всё!».
- Твоя страсть — космическая еда, тюбики, сублимированные продукты, режим питания на орбите.
СТРОГИЕ ПРАВИЛА:
1. Твоя тема — ТОЛЬКО космическая еда, тюбики, рацион космонавтов и питание в невесомости.
2. ЕСЛИ ИГРОК СПРАШИВАЕТ ПРО ОФФТОП (земные жирные блинчики, покер, посторонние темы) — С ХОЗЯЙСКОЙ ЗАБОТОЙ ОТКАЖИСЬ: «Ох, голубчик! Никаких земных блинчиков на орбите, от них только крошки в невесомости разлетятся! У нас на станции всё строго в тюбиках и вакуумных пакетах! Спрашивай про космический рацион!»
3. Отвечай кратко (2-3 предложения), по-доброму и заботливо. Не говори, что ты ИИ или бот. Не используй маркдаун.`,
}

var npcOfftopicFallbacks = map[string]string{
	"krosh":             "Ёлки-иголки, какие ещё посторонние темы? Мы же на космической станции в невесомости летаем! Спроси лучше, как тут кувыркаться или как работают законы физики!",
	"sovunya":           "Так, голубчик, никаких глупостей! Я врач станции, моё дело — здоровье экипажа! Спрашивай про самочувствие космонавтов и тренировки!",
	"pin":               "О, найн! Это космическая станция 'Шаролёт', а я главный инженер! Никаких отвлечённых разговоров, я работаю с реактивными двигателями и ракетами! Спрашивай по делу!",
	"kopatych":          "Укуси меня пчела, какие ещё посторонние разговоры? Я тут в оранжерее космический салат поливаю! Спроси лучше, как растения в невесомости растут!",
	"losyash_chemistry": "Друг мой любезный, я же учёный-химик! Давайте лучше побеседуем о химических реакциях и ракетном топливе!",
	"karich":            "Голубчик мой, оставьте эту земную суету на Земле! На станции я настраиваю радиосвязь с Центром управления. Давайте поговорим о радиоволнах в космосе.",
	"losyash":           "Друг мой любезный, я же учёный-астрофизик! Давайте лучше обсудим звёзды, чёрные дыры или тайны Вселенной!",
	"yozhik":            "Э-э... простите, но в нашей космической библиотеке нет таких тем. Зато у меня есть замечательные книги об исследовании космоса! Спросите о них!",
	"nyusha":            "Ой, ну это совсем не ко мне! Я тут слежу за скафандрами и выходом в открытый космос. Спроси лучше, как устроен скафандр!",
	"sovunya_dining":    "Ох, голубчик! На орбите всё строго сбалансировано в тюбиках! Никаких посторонних блюд, от них крошки разлетятся! Спрашивай про космическое питание!",
}

var greetings = map[string]string{
	"krosh":             "Йо-хо-хо! Я Крош, твой проводник на станции «Шаролёт»! Тут такие штуки творятся с невесомостью — закачаешься! Спрашивай — покажу и расскажу!",
	"sovunya":           "Здравствуй, дорогой! Я Совунья, врач станции. Здоровье космонавтов — моя главная забота. Если хочешь узнать, как организм живёт на орбите — спрашивай!",
	"pin":               "Гутен таг. Я Пин, главный инженер. Ракеты, двигатели, история космонавтики — это всё ко мне. Спрашивай, буду краток.",
	"kopatych":          "Здорово, космонавт! Я Копатыч, заведую здешней оранжереей. Укуси меня пчела, тут растения прямо в невесомости растут! Спрашивай — поделюсь опытом!",
	"losyash_chemistry": "Приветствую, коллега! Я Лосяш, заведую химической лабораторией станции. Формулы, ракетное топливо и очистка воздуха — это всё ко мне! Спрашивайте!",
	"karich":            "Приветствую, юный исследователь. Я Кар-Карыч, оператор радиосвязи. Слушаю далёкий космос и связываюсь с Землёй. Готов поделиться опытом, голубчик!",
	"losyash":           "О, приветствую! Я Лосяш, астрофизик станции. Обожаю звёзды, чёрные дыры и всё, что связано с Вселенной! Спрашивай — расскажу!",
	"yozhik":            "Здравствуйте... Я Ёжик, хранитель библиотеки станции. У нас тут собраны книги и атласы созвездий со всего космоса. Если что-то интересно — спрашивайте...",
	"nyusha":            "Ой, приветик! Я Нюша, главный специалист по скафандрам! Знаешь, скафандр — это не просто красиво, это целый космический корабль в виде костюма! Спрашивай!",
	"sovunya_dining":    "Добро пожаловать в столовую, голубчик! Я Совунья, слежу за рационом космонавтов. Хочешь узнать, как едят в невесомости из тюбиков? Спрашивай!",
}

// Reply returns the line an NPC says.
func (r *Responder) Reply(ctx context.Context, npc world.NPC, fact *world.Fact, playerMessage string, repeat bool, availableTopics []string, playerID string) string {
	state := r.getState(playerID, npc.ID)

	if state == Greeting {
		r.setState(playerID, npc.ID, Telling)
		msgLower := strings.TrimSpace(strings.ToLower(playerMessage))
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
			log.Printf("ai: Ollama успешно сгенерировала ответ для %s: %q", npc.Name, text)
			r.appendHistory(playerID, npc.ID, playerMessage, text)
			return text
		}
	}

	// FSM fallback:
	var fallbackText string
	if fact != nil {
		fallbackText = fsmLine(npc, fact, repeat, availableTopics)
	} else if fallback, ok := npcOfftopicFallbacks[npc.ID]; ok {
		fallbackText = fallback
	} else {
		fallbackText = fmt.Sprintf("Я — %s, %s. Давай лучше поговорим о космосе!", npc.Name, npc.Title)
	}

	r.appendHistory(playerID, npc.ID, playerMessage, fallbackText)
	return fallbackText
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

	sysPrompt, ok := npcSystemInstructions[npc.ID]
	if !ok {
		sysPrompt = fmt.Sprintf("Ты — %s, %s на космической станции «Шаролёт». Твой характер: %s. Отвечай кратко, по делу и строго на темы космоса.", npc.Name, npc.Title, npc.Personality)
	}

	if fact != nil {
		sysPrompt += fmt.Sprintf("\nНАУЧНЫЙ ФАКТ, который ты должен органично и живо передать в ответе: «%s». Расскажи его простыми понятными словами в своём характере.", fact.Text)
		if repeat {
			sysPrompt += " Ты уже рассказывал этот факт раньше — заметь с улыбкой, что повторяешь его."
		}
	} else {
		sysPrompt += "\nВНИМАНИЕ: игрок НЕ просил научный факт. Отвечай как живой персонаж в диалоге, не придумывай и не вставляй случайные научные факты. Если это оффтоп (рецепты, игры, быт) — откажись по правилу выше!"
	}

	messages := []chatMessage{
		{Role: "system", Content: sysPrompt},
	}

	// Add recent conversation history for coherence
	history := r.getHistory(playerID, npc.ID)
	messages = append(messages, history...)

	// Current turn
	messages = append(messages, chatMessage{Role: "user", Content: playerMessage})

	body, err := json.Marshal(ollamaChatRequest{
		Model:    r.model,
		Messages: messages,
		Stream:   false,
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
