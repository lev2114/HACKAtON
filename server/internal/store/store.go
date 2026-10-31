// Package store persists player state and per-player NPC memory in SQLite.
// This is what makes NPCs "remember" a player across reconnects and what
// satisfies the task's "player state saved server-side" requirement.
package store

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

type Player struct {
	ID    string
	Name  string
	Token string
	Room  string
	X, Y, Z float64
}

type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS players (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	token TEXT NOT NULL UNIQUE,
	room TEXT NOT NULL DEFAULT 'hub',
	x REAL NOT NULL DEFAULT 0,
	y REAL NOT NULL DEFAULT 0,
	z REAL NOT NULL DEFAULT 0,
	updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE IF NOT EXISTS npc_memory (
	player_id TEXT NOT NULL,
	npc_id TEXT NOT NULL,
	fact_id TEXT NOT NULL,
	told_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	PRIMARY KEY (player_id, npc_id, fact_id)
);
`

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) UpsertPlayer(p Player) error {
	_, err := s.db.Exec(`
		INSERT INTO players (id, name, token, room, x, y, z, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(id) DO UPDATE SET
			name = excluded.name,
			token = excluded.token,
			room = excluded.room,
			x = excluded.x, y = excluded.y, z = excluded.z,
			updated_at = CURRENT_TIMESTAMP
	`, p.ID, p.Name, p.Token, p.Room, p.X, p.Y, p.Z)
	return err
}

func (s *Store) PlayerByToken(token string) (*Player, error) {
	row := s.db.QueryRow(`SELECT id, name, token, room, x, y, z FROM players WHERE token = ?`, token)
	var p Player
	if err := row.Scan(&p.ID, &p.Name, &p.Token, &p.Room, &p.X, &p.Y, &p.Z); err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *Store) SavePosition(id, room string, x, y, z float64) error {
	_, err := s.db.Exec(
		`UPDATE players SET room = ?, x = ?, y = ?, z = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`,
		room, x, y, z, id,
	)
	return err
}

// ToldFacts returns the set of fact IDs this NPC has already told this
// player, so the NPC can skip ahead instead of repeating itself.
func (s *Store) ToldFacts(playerID, npcID string) (map[string]bool, error) {
	rows, err := s.db.Query(`SELECT fact_id FROM npc_memory WHERE player_id = ? AND npc_id = ?`, playerID, npcID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]bool{}
	for rows.Next() {
		var factID string
		if err := rows.Scan(&factID); err != nil {
			return nil, err
		}
		out[factID] = true
	}
	return out, rows.Err()
}

func (s *Store) MarkToldFact(playerID, npcID, factID string) error {
	_, err := s.db.Exec(
		`INSERT OR IGNORE INTO npc_memory (player_id, npc_id, fact_id) VALUES (?, ?, ?)`,
		playerID, npcID, factID,
	)
	return err
}
