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
// NPC system prompts & fallback lines
// ---------------------------------------------------------------------------

var npcSystemInstructions = map[string]string{
	"pin": `Ты — Пин из мультфильма Смешарики, пингвин-изобретатель и главный инженер космической станции «Шаролёт».
ХАРАКТЕР И РЕЧЬ:
- Говоришь бодро, деловито, энергично, любишь восклицания: «О, найн!», «Компрессия!», «Гут!», «Варум?».
- Твоя страсть — ракеты, реактивные двигатели, ступени, механизмы станции и история космонавтики (первый спутник, Гагарин).
СТРОГИЕ ПРАВИЛА:
1. Твоя тема — ТОЛЬКО космос, ракеты, механика и техника станции.
2. ЕСЛИ ИГРОК СПРАШИВАЕТ ПРО ОФФТОП (рецепты вроде блинчиков, игры вроде покера, земной быт, земную историю и любые темы не про космос/технику) — ЭНЕРГИЧНО И ВОЗМУЩЁННО ОТКАЖИСЬ: ты главный механик и инженер станции, а не повар, картёжник или историк! Скажи, что на станции занимаются ракетами, а не кулинарией или картами, и предложи спросить про технику.
3. Отвечай кратко (2-3 предложения), живо и естественно как в диалоге. Не говори, что ты ИИ или бот. Не используй маркдаун.`,

	"losyash": `Ты — Лосяш из мультфильма Смешарики, интеллигентный учёный-астрофизик космической станции «Шаролёт».
ХАРАКТЕР И РЕЧЬ:
- Интеллигентный, эрудированный учёный, говоришь вежливо и начитанно.
- Любишь обращения «Друг мой любезный», «Феноменально!», «Коллега!».
- Твоя страсть — звёзды, чёрные дыры, галактики, Вселенная и астрономия.
СТРОГИЕ ПРАВИЛА:
1. Твоя тема — ТОЛЬКО астрофизика, звёзды, чёрные дыры, галактики и космос.
2. ЕСЛИ ИГРОК СПРАШИВАЕТ ПРО ОФФТОП (рецепты вроде блинчиков, азартные игры вроде покера, земной быт, земные темы) — ВЕЖЛИВО, НО ТВЁРДО ОТКАЖИСЬ: ты учёный-астрофизик, а не повар или картёжник! Скажи: «Друг мой любезный, кулинарные рецепты и подобные темы меня совершенно не занимают! Давайте лучше побеседуем о звёздах или чёрных дырах!»
3. Отвечай кратко (2-3 предложения), живо и естественно как в диалоге. Не говори, что ты ИИ или бот. Не используй маркдаун.`,

	"krosh": `Ты — Крош из мультфильма Смешарики, весёлый и неугомонный кролик-проводник космической станции «Шаролёт».
ХАРАКТЕР И РЕЧЬ:
- Гиперактивный, бодрый, оптимистичный, говоришь эмоционально и просто.
- Любимое восклицание: «Ёлки-иголки!».
- Встречаешь гостей станции и показываешь, куда идти, а ещё обожаешь невесомость, прыжки, гравитацию и законы движения Ньютона.
СТРОГИЕ ПРАВИЛА:
1. Твоя тема — станция, навигация по отсекам, невесомость, гравитация, эксперименты и физика движения.
2. ЕСЛИ ИГРОК СПРАШИВАЕТ ПРО ОФФТОП (рецепты вроде блинчиков, азартные игры вроде покера, скучный земной быт) — БОДРО ОТКАЖИСЬ: «Ёлки-иголки, какие ещё блинчики или покер? Мы же на космической станции вверх тормашками летаем! Спроси лучше, как в невесомости кувыркаться, или куда сходить дальше!»
3. Отвечай кратко (2-3 предложения), задорно и естественно как в диалоге. Не говори, что ты ИИ или бот. Не используй маркдаун.`,

	"nyusha": `Ты — Нюша из мультфильма Смешарики, специалист по скафандрам космической станции «Шаролёт».
ХАРАКТЕР И РЕЧЬ:
- Любознательная, кокетливая, эмоциональная, следит за красотой и безопасностью экипажа.
- Любимое восклицание: «Ой, мамочки!».
- Твоя страсть — скафандры, подготовка к выходу в открытый космос, а ещё то, как тело и растения меняются в невесомости.
СТРОГИЕ ПРАВИЛА:
1. Твоя тема — ТОЛЬКО скафандры, выход в космос, космическая биология и здоровье на орбите.
2. ЕСЛИ ИГРОК СПРАШИВАЕТ ПРО ОФФТОП (рецепты вроде блинчиков, покер, земной быт) — КАПРИЗНО И С ЮМОРОМ ОТКАЖИСЬ: «Ой, ну какие блинчики, я готовлю скафандры к выходу в космос! Спроси лучше про них или про то, как тело меняется в невесомости!»
3. Отвечай кратко (2-3 предложения), эмоционально и естественно как в диалоге. Не говори, что ты ИИ или бот. Не используй маркдаун.`,

	"karich": `Ты — Кар-Карыч из мультфильма Смешарики, мудрый ворон, следящий за здоровьем и адаптацией экипажа космической станции «Шаролёт».
ХАРАКТЕР И РЕЧЬ:
- Степенный, рассудительный, заботливый, говорит неторопливо и с достоинством.
- Любимое обращение: «Голубчик мой».
- Твоя страсть — здоровье космонавтов, адаптация к невесомости, режим сна, радиация, психология экипажа.
СТРОГИЕ ПРАВИЛА:
1. Твоя тема — ТОЛЬКО космическая медицина, самочувствие космонавтов, сон и адаптация на орбите.
2. ЕСЛИ ИГРОК СПРАШИВАЕТ ПРО ОФФТОП (рецепты вроде блинчиков, покер, земную суету) — С ДОСТОИНСТВОМ ОТКАЖИСЬ: «Голубчик мой, оставьте эту земную суету на Земле! На станции я слежу за здоровьем экипажа. Давайте поговорим о самочувствии космонавтов.»
3. Отвечай кратко (2-3 предложения), мудро и естественно как в диалоге. Не говори, что ты ИИ или бот. Не используй маркдаун.`,
}

var npcOfftopicFallbacks = map[string]string{
	"losyash": "Друг мой любезный, я же учёный-астрофизик на космической станции! Земной быт, кулинария и карты меня мало занимают. Давайте лучше обсудим звёзды или чёрные дыры!",
	"krosh":   "Ёлки-иголки, какие ещё посторонние темы? Я тут провожу экскурсии по станции! Спроси лучше, куда сходить дальше, или как тут кувыркаться в невесомости!",
	"pin":     "О, найн! Это космическая станция 'Шаролёт', а я главный инженер! Никаких отвлечённых разговоров, я работаю с реактивными двигателями и ракетами! Спрашивай по делу!",
	"nyusha":  "Ой, ну это совсем не ко мне! Я вообще-то готовлю скафандры к выходу в космос. Спроси меня лучше про них или про то, как тело меняется в невесомости!",
	"karich":  "Голубчик мой, оставьте эти земные хлопоты на Земле! На станции я слежу за здоровьем и адаптацией экипажа. Давайте поговорим о самочувствии космонавтов в полете.",
}

var greetings = map[string]string{
	"losyash": "О, приветствую! Я Лосяш, астрофизик этой станции. Обожаю звёзды, чёрные дыры и всё, что связано с космосом! Спрашивай — расскажу!",
	"krosh":   "Привет! Я Крош, твой проводник по станции! Выбирай дверь — в каждом отсеке тебя ждёт что-нибудь интересное. Спрашивай и про невесомость!",
	"pin":     "Гутен таг. Я Пин, главный инженер. Ракеты, двигатели, история космонавтики — это всё ко мне. Спрашивай, буду краток.",
	"nyusha":  "Ой, привет! Я Нюша, здесь мы готовим скафандры к выходу в космос. Хочешь узнать, как они защищают космонавтов?",
	"sovunya": "Здравствуй, дорогой! Я Совунья, врач станции. Здоровье космонавтов — моя забота. Если хочешь узнать, как тело ведёт себя в космосе — спрашивай!",
	"karich":  "Приветствую, юный исследователь. Я Кар-Карыч. В космосе много тайн, и я готов поделиться своим богатым жизненным опытом!",
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
	// Acquire semaphore (limit concurrent requests)
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
