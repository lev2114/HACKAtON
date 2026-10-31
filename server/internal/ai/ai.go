// Package ai turns a raw fact into an in-character NPC line. If a local
// Ollama server is configured and answers in time, it rephrases the fact in
// the NPC's voice. Otherwise (no Ollama URL, Ollama unreachable, or slow) it
// falls back to a templated line built straight from the fact text — this
// fallback is what keeps the game fully playable with zero network access,
// which is required during the offline half of the demo.
package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"server/internal/world"
)

type Responder struct {
	ollamaURL string
	model     string
	client    *http.Client
}

// New builds a Responder. Pass ollamaURL = "" to disable the LLM entirely
// and always use the FSM fallback (useful for a low-powered demo laptop).
func New(ollamaURL, model string) *Responder {
	return &Responder{
		ollamaURL: ollamaURL,
		model:     model,
		client:    &http.Client{Timeout: 5 * time.Second},
	}
}

type ollamaRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
	Stream bool   `json:"stream"`
}

type ollamaResponse struct {
	Response string `json:"response"`
}

// Reply returns the line an NPC says. fact may be nil if the NPC has
// nothing left to say at all (empty fact bank).
func (r *Responder) Reply(ctx context.Context, npc world.NPC, fact *world.Fact, playerMessage string, repeat bool) string {
	if fact == nil {
		return fmt.Sprintf("%s задумывается: кажется, тут больше нечего рассказать.", npc.Name)
	}

	prefix := ""
	if repeat {
		prefix = "Кажется, я уже рассказывал тебе это, но вот ещё раз: "
	}

	if r.ollamaURL == "" {
		return fsmLine(npc, fact, prefix)
	}

	text, err := r.askOllama(ctx, npc, fact, playerMessage)
	if err != nil {
		return fsmLine(npc, fact, prefix)
	}
	return prefix + text
}

func (r *Responder) askOllama(ctx context.Context, npc world.NPC, fact *world.Fact, playerMessage string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()

	prompt := fmt.Sprintf(
		"Ты — %s, %s. Твой характер: %s. "+
			"Игрок говорит тебе: %q. "+
			"Ответь по-русски одним-двумя короткими предложениями в своём стиле и обязательно передай этот факт: %q. "+
			"Не упоминай, что ты ИИ или персонаж мультфильма.",
		npc.Name, npc.Title, npc.Personality, playerMessage, fact.Text,
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
	return out.Response, nil
}

func fsmLine(npc world.NPC, fact *world.Fact, prefix string) string {
	return fmt.Sprintf("%s%s: %s", prefix, npc.Name, fact.Text)
}
