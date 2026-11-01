// Package ai turns a raw fact into an in-character NPC line using a finite
// state machine (FSM) with keyword-aware topic selection. If a local Ollama
// server is configured and answers in time, it rephrases the fact in the
// NPC's voice. Otherwise (no Ollama URL, Ollama unreachable, or slow) it
// falls back to a templated FSM line — this keeps the game fully playable
// with zero network access, which is required during the offline demo.
//
// FSM states per (player, NPC) pair:
//
//	GREETING  → first interaction, NPC introduces itself
//	TELLING   → NPC tells facts, adapting to player's questions via keywords
//	DEEPDIVE  → player asked about a topic NPC already covered → NPC repeats
//	            with a "remember?" prefix and suggests other topics
//	EXHAUSTED → all facts told → NPC says goodbye and suggests other NPCs
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
	return &Responder{
		ollamaURL: ollamaURL,
		model:     model,
		client:    &http.Client{Timeout: 10 * time.Second},
		sem:       make(chan struct{}, 2), // at most 2 concurrent Ollama requests
		states:    make(map[string]State),
	}
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
// NPC greeting templates (one per NPC, adds personality to first contact)
// ---------------------------------------------------------------------------

var greetings = map[string]string{
	"losyash": "О, приветствую! Я Лосяш, астрофизик этой станции. Обожаю звёзды, чёрные дыры и всё, что связано с космосом! Спрашивай — расскажу!",
	"krosh":   "Привет! Я Крош, твой проводник по станции! Выбирай дверь — в каждом отсеке тебя ждёт что-нибудь интересное. Спрашивай и про невесомость!",
	"pin":     "Гутен таг. Я Пин, главный инженер. Ракеты, двигатели, история космонавтики — это всё ко мне. Спрашивай, буду краток.",
	"nyusha":  "Ой, привет! Я Нюша, здесь мы готовим скафандры к выходу в космос. Хочешь узнать, как они защищают космонавтов?",
	"sovunya": "Здравствуй, дорогой! Я Совунья, врач станции. Здоровье космонавтов — моя забота. Если хочешь узнать, как тело ведёт себя в космосе — спрашивай!",
}

// Reply returns the line an NPC says. It drives the FSM through states:
// GREETING → TELLING → DEEPDIVE/EXHAUSTED. Uses PickFact from world
// package for keyword-aware topic selection.
func (r *Responder) Reply(ctx context.Context, npc world.NPC, fact *world.Fact, playerMessage string, repeat bool, availableTopics []string, playerID string) string {
	state := r.getState(playerID, npc.ID)

	switch state {
	case Greeting:
		// First interaction — greet and transition to Telling
		r.setState(playerID, npc.ID, Telling)
		if g, ok := greetings[npc.ID]; ok {
			return g
		}
		return fmt.Sprintf("Привет! Я %s, %s. Спрашивай о чём хочешь!", npc.Name, npc.Title)

	case Exhausted:
		// All facts told
		return "Кажется, я рассказал тебе всё, что знаю! Попробуй поговорить с кем-нибудь в другом отсеке станции."
	}

	// States: Telling or DeepDive
	if fact == nil {
		r.setState(playerID, npc.ID, Exhausted)
		return "Хм, кажется, я рассказал тебе всё, что знаю по этой теме! Загляни в другие отсеки — там тоже много интересного."
	}

	if repeat {
		r.setState(playerID, npc.ID, DeepDive)
	} else {
		r.setState(playerID, npc.ID, Telling)
	}

	// Try Ollama first if available
	if r.ollamaURL != "" {
		text, err := r.askOllama(ctx, npc, fact, playerMessage, repeat, availableTopics)
		if err != nil {
			log.Printf("ai: ollama error for %s→%s, falling back to FSM: %v", playerID, npc.ID, err)
		} else {
			return text
		}
	}

	// FSM fallback: templated response (client already displays speaker name in chat bubble)
	return fsmLine(npc, fact, repeat, availableTopics)
}

// fsmLine generates a templated NPC response without LLM.
// Note: we do not prefix with npc.Name because the client UI already shows it.
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

	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	repeatInstruction := ""
	if repeat {
		repeatInstruction = "Игрок уже слышал этот факт раньше — перефразируй его и заметь, что ты это уже рассказывал. "
	}

	topicHint := ""
	if len(availableTopics) > 0 {
		var names []string
		for _, t := range availableTopics {
			names = append(names, topicDisplayName(t))
		}
		topicHint = fmt.Sprintf("Если уместно, в конце можешь предложить поговорить ещё про: %s. ", strings.Join(names, ", "))
	}

	prompt := fmt.Sprintf(
		"Ты — %s, %s на космической станции «Шаролёт». Твой характер: %s. "+
			"Игрок говорит тебе: «%s». "+
			"%s"+
			"Ответь по-русски двумя-тремя короткими предложениями в своём стиле и обязательно передай этот научный факт: «%s». "+
			"%s"+
			"Не называй своё имя в начале реплики. Не упоминай, что ты ИИ, бот или персонаж мультфильма. Не используй маркдаун.",
		npc.Name, npc.Title, npc.Personality, playerMessage,
		repeatInstruction,
		fact.Text,
		topicHint,
	)

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
