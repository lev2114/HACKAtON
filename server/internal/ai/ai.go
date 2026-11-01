// Package ai turns a raw fact into an in-character NPC line using a finite
// state machine (FSM) with keyword-aware topic selection. If a local Ollama
// server is configured and answers in time, it rephrases the fact in the
// NPC's voice or handles conversation and off-topic questions in character.
// Otherwise (no Ollama URL, Ollama unreachable, or slow) it falls back to
// templated FSM lines.
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

// Responder generates NPC dialogue lines.
type Responder struct {
	ollamaURL string
	model     string
	client    *http.Client

	// Semaphore to limit concurrent Ollama requests (avoid overloading
	// a small local model with multiple players talking at once).
	sem chan struct{}

	// FSM state per (playerID, npcID) pair. Not persisted to DB — resets
	// on server restart, which is fine for a hackathon demo.
	mu     sync.RWMutex
	states map[string]State
}

// New builds a Responder. Pass ollamaURL = "" to disable the LLM entirely
// and always use the FSM fallback (useful for a low-powered demo laptop).
func New(ollamaURL, model string) *Responder {
	r := &Responder{
		ollamaURL: ollamaURL,
		model:     model,
		client:    &http.Client{Timeout: 35 * time.Second},
		sem:       make(chan struct{}, 2), // at most 2 concurrent Ollama requests
		states:    make(map[string]State),
	}

	// Warm up model in background so the first in-game query doesn't stall on load
	if ollamaURL != "" {
		go r.warmup()
	}

	return r
}

func (r *Responder) warmup() {
	log.Printf("ai: прогрев модели Ollama (%s)...", r.model)
	body, _ := json.Marshal(ollamaRequest{Model: r.model, Prompt: "Привет", Stream: false})
	req, err := http.NewRequest(http.MethodPost, r.ollamaURL+"/api/generate", bytes.NewReader(body))
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

// ---------------------------------------------------------------------------
// NPC specialties and greeting templates
// ---------------------------------------------------------------------------

var npcSpecialties = map[string]string{
	"losyash": "астрофизика: звёзды, чёрные дыры, галактики, Вселенная",
	"krosh":   "физика и движение: гравитация, невесомость, законы Ньютона, инерция, вращение",
	"pin":     "инженерия и техника: ракеты, двигатели, ступени, космическая техника, Гагарин и первый спутник",
	"nyusha":  "космическая биология: растения в оранжерее, питание космонавтов, изменения тела в невесомости",
	"karich":  "космическая медицина: адаптация к невесомости, радиация, сон, психология экипажа, реабилитация",
}

var npcOfftopicFallbacks = map[string]string{
	"losyash": "Друг мой любезный, я же учёный-астрофизик на космической станции! Земной быт, кулинария и подобные темы меня мало занимают. Давайте лучше обсудим звёзды, чёрные дыры или тайны Вселенной!",
	"krosh":   "Ёлки-иголки, какие ещё посторонние темы? Мы же на космической станции в невесомости летаем! Спроси лучше, как тут кувыркаться или как работают законы физики!",
	"pin":     "О, найн! Это космическая станция 'Шаролёт', а я главный инженер! Никаких отвлечённых разговоров, я работаю с реактивными двигателями и ракетами! Спрашивай по делу!",
	"nyusha":  "Ой, ну это совсем не ко мне! Я вообще-то занимаюсь космической биологией и оранжереей на станции. Спроси меня лучше про растения на орбите или про то, как тело меняется в космосе!",
	"karich":  "Голубчик мой, оставьте эти земные хлопоты на Земле! На станции я слежу за здоровьем и адаптацией экипажа. Давайте поговорим о самочувствии космонавтов в полете.",
}

var greetings = map[string]string{
	"losyash": "О, приветствую! Я Лосяш, астрофизик этой станции. Обожаю звёзды, чёрные дыры и всё, что связано с космосом! Спрашивай — расскажу!",
	"krosh":   "Йо-хо-хо! Я Крош, испытатель невесомости! Тут такие штуки творятся с гравитацией — закачаешься! Ну, в смысле, не закачаешься, тут же невесомость! Спрашивай!",
	"pin":     "Гутен таг. Я Пин, главный инженер. Ракеты, двигатели, история космонавтики — это всё ко мне. Спрашивай, буду краток.",
	"nyusha":  "Ой, привет! Я Нюша, биолог-садовод станции. Знаешь, в космосе столько интересного про тело и растения! Спрашивай, я расскажу!",
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
			if g, ok := greetings[npc.ID]; ok {
				return g
			}
			return fmt.Sprintf("Привет! Я %s, %s. Спрашивай о чём хочешь!", npc.Name, npc.Title)
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

	// Try Ollama first if available
	if r.ollamaURL != "" {
		hasFact := fact != nil
		log.Printf("ai: запрос к Ollama для %s (hasFact: %v, вопрос: %q)", npc.Name, hasFact, playerMessage)
		text, err := r.askOllama(ctx, npc, fact, playerMessage, repeat, availableTopics)
		if err != nil {
			log.Printf("ai: ollama error for %s→%s, falling back to FSM: %v", playerID, npc.ID, err)
		} else {
			log.Printf("ai: Ollama успешно сгенерировала ответ для %s", npc.Name)
			return text
		}
	}

	// FSM fallback:
	if fact != nil {
		return fsmLine(npc, fact, repeat, availableTopics)
	}

	// Fallback for off-topic or small-talk when no fact was requested
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
		// Vary the intro based on difficulty
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

	// If there are other untold topics, suggest them
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
// Ollama integration
// ---------------------------------------------------------------------------

type ollamaRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
	Stream bool   `json:"stream"`
}

type ollamaResponse struct {
	Response string `json:"response"`
}

func (r *Responder) askOllama(ctx context.Context, npc world.NPC, fact *world.Fact, playerMessage string, repeat bool, availableTopics []string) (string, error) {
	// Acquire semaphore (limit concurrent requests)
	select {
	case r.sem <- struct{}{}:
		defer func() { <-r.sem }()
	case <-ctx.Done():
		return "", ctx.Err()
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	specialty := npcSpecialties[npc.ID]
	if specialty == "" {
		specialty = npc.Title
	}

	var prompt string

	if fact != nil {
		// Scenario A: explaining a specific scientific fact
		repeatInstruction := ""
		if repeat {
			repeatInstruction = "Игрок уже слышал этот факт раньше — заметь в начале реплики, что ты это уже упоминал.\n"
		}

		topicHint := ""
		if len(availableTopics) > 0 {
			var names []string
			for _, t := range availableTopics {
				names = append(names, topicDisplayName(t))
			}
			topicHint = fmt.Sprintf("Если уместно, в конце можешь предложить поговорить ещё про: %s.\n", strings.Join(names, ", "))
		}

		prompt = fmt.Sprintf(
			"Ты — %s, %s на космической станции «Шаролёт». Твой характер: %s. Твоя специальность: %s.\n"+
				"Игрок спрашивает: «%s».\n"+
				"%s"+
				"Ответь по-русски двумя-тремя короткими предложениями в своём стиле и характере, объяснив этот научный факт: «%s».\n"+
				"%s\n"+
				"ПРАВИЛА:\n"+
				"- Не называй своё имя в начале реплики.\n"+
				"- Не говори, что ты ИИ, бот или персонаж мультфильма.\n"+
				"- Не используй маркдаун.\n"+
				"- Отвечай живо, кратко и строго в своём характере.",
			npc.Name, npc.Title, npc.Personality, specialty,
			playerMessage,
			repeatInstruction,
			fact.Text,
			topicHint,
		)
	} else {
		// Scenario B: small-talk OR off-topic question without a science fact request
		prompt = fmt.Sprintf(
			"Ты — %s, %s на космической станции «Шаролёт».\n"+
				"Твой характер: %s.\n"+
				"Твоя научная специальность на станции: %s.\n"+
				"Игрок говорит тебе: «%s».\n\n"+
				"ПРАВИЛА ОТВЕТА:\n"+
				"1. Если вопрос игрока на отвлечённую или постороннюю тему (кулинарные рецепты вроде блинчиков, земная бытовая жизнь, не связанная с космосом история, программирование, другие темы не про станцию и космос) — ВЕЖЛИВО, НО ТВЁРДО И С ХАРАКТЕРНЫМ ЮМОРОМ В СВОЁМ СТИЛЕ ОТКАЖИСЬ отвечать. Скажи, что ты на космической станции занимаешься наукой (%s), а не бытовыми вопросами, и предложи поговорить о твоей специальности.\n"+
				"2. Если игрок просто общается («как дела?», «чем занят?», «кто ты?») — ответь в своём характере, расскажи кратко, чем занят на станции, и предложи задать вопрос по твоей теме (%s).\n"+
				"3. НЕ придумывай случайных научных фактов, если о них прямо не спрашивали!\n"+
				"4. Не называй своё имя в начале реплики. Не говори, что ты ИИ, языковая модель или персонаж мультфильма.\n"+
				"5. Ответ должен быть кратким: 2-3 живых предложения на русском языке. Без маркдауна.",
			npc.Name, npc.Title,
			npc.Personality,
			specialty,
			playerMessage,
			specialty,
			specialty,
		)
	}

	body, err := json.Marshal(ollamaRequest{Model: r.model, Prompt: prompt, Stream: false})
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.ollamaURL+"/api/generate", bytes.NewReader(body))
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

	var out ollamaResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.Response == "" {
		return "", fmt.Errorf("ollama returned empty response")
	}
	return strings.TrimSpace(out.Response), nil
}
