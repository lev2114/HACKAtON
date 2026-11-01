package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type WorldState struct {
	PlayerID  string `json:"playerId"`
	Apples    int    `json:"apples"`
	SuitColor string `json:"suitColor"`
	Revision  uint64 `json:"revision"`
}

func (s *Store) WorldState(playerID string) (WorldState, error) {
	state := WorldState{PlayerID: playerID, SuitColor: "#ffffff"}
	err := s.db.QueryRow(`SELECT apples, suit_color FROM player_world_state WHERE player_id = ?`, playerID).Scan(&state.Apples, &state.SuitColor)
	if errors.Is(err, sql.ErrNoRows) {
		return state, nil
	}
	if err != nil {
		return state, fmt.Errorf("read player world state: %w", err)
	}
	return state, nil
}

func (s *Store) GiveApple(playerID string) (bool, error) {
	now := time.Now().Unix()
	result, err := s.db.Exec(`INSERT INTO player_world_state (player_id, apples, last_apple_at) VALUES (?, 1, ?)
	 ON CONFLICT(player_id) DO UPDATE SET apples = apples + 1, last_apple_at = excluded.last_apple_at
	 WHERE apples < 5 AND last_apple_at <= ?`, playerID, now, now-5)
	if err != nil {
		return false, fmt.Errorf("give apple: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("count apple grant: %w", err)
	}
	return n > 0, nil
}

func (s *Store) EatApple(playerID string) (bool, error) {
	result, err := s.db.Exec(`UPDATE player_world_state SET apples = apples - 1 WHERE player_id = ? AND apples > 0`, playerID)
	if err != nil {
		return false, fmt.Errorf("eat apple: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("count apple consumption: %w", err)
	}
	return n > 0, nil
}

func (s *Store) SetSuitColor(playerID, color string) error {
	_, err := s.db.Exec(`INSERT INTO player_world_state (player_id, suit_color) VALUES (?, ?)
	 ON CONFLICT(player_id) DO UPDATE SET suit_color = excluded.suit_color`, playerID, color)
	if err != nil {
		return fmt.Errorf("set suit color: %w", err)
	}
	return nil
}
