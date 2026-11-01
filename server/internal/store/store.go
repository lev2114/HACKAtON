// Package store persists player accounts and per-player NPC memory in
// SQLite. This is what makes NPCs "remember" a player across reconnects and
// what satisfies the task's "player state saved server-side" requirement.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("player not found")

// TokenTTL bounds how long a session token stays valid after it's issued
// (register/login). Previously tokens never expired — a leaked token (e.g.
// from a browser's localStorage, now that "remember me" persists it there)
// would have stayed valid forever. Logging in again always issues a fresh
// token with a fresh TTL, so this only matters for abandoned/leaked tokens.
const TokenTTL = 14 * 24 * time.Hour

type Player struct {
	ID             string
	Name           string
	PasswordHash   string
	Token          string
	TokenExpiresAt time.Time
	Room           string
	X, Y, Z        float64
	IsAdmin        bool
}

type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS players (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL UNIQUE,
	password_hash TEXT NOT NULL,
	token TEXT NOT NULL UNIQUE,
	token_expires_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
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
	// SQLite allows exactly one writer at a time; database/sql's default
	// connection pool happily opens several connections and lets two of
	// them race, which SQLite answers with SQLITE_BUSY ("database is
	// locked") instead of queuing. A single connection serializes every
	// query through it, which is more than fast enough at our scale and
	// removes the race entirely.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	// Both columns were added after the first version of this schema
	// shipped (back when login was just `{name}`, no password at all) —
	// this backfills any station.db that predates them. SQLite has no
	// "ADD COLUMN IF NOT EXISTS", so each just ignores its own "duplicate
	// column" error when it's already there.
	if _, err := db.Exec(`ALTER TABLE players ADD COLUMN password_hash TEXT NOT NULL DEFAULT ''`); err != nil {
		if !strings.Contains(err.Error(), "duplicate column") {
			return nil, fmt.Errorf("migrate password_hash: %w", err)
		}
	}
	// SQLite rejects a non-constant default (CURRENT_TIMESTAMP) on ADD
	// COLUMN, so this uses a fixed past date instead — existing tokens
	// just read as already-expired, which only means those sessions need
	// one fresh login (a new token gets a real TTL via UpdateToken). Any
	// account backfilled with an empty password_hash above similarly just
	// can't log in with any password — it predates passwords entirely, so
	// there's nothing correct to log in with; re-registering is the fix.
	if _, err := db.Exec(`ALTER TABLE players ADD COLUMN token_expires_at DATETIME NOT NULL DEFAULT '1970-01-01 00:00:00'`); err != nil {
		if !strings.Contains(err.Error(), "duplicate column") {
			return nil, fmt.Errorf("migrate token_expires_at: %w", err)
		}
	}
	if _, err := db.Exec(`ALTER TABLE players ADD COLUMN is_admin INTEGER NOT NULL DEFAULT 0`); err != nil {
		if !strings.Contains(err.Error(), "duplicate column") {
			return nil, fmt.Errorf("migrate is_admin: %w", err)
		}
	}
	// Admin-panel edits are stored as overrides on top of world-data/*.json
	// (which stays untouched in git): kind = "npc" | "facts" | "settings",
	// key = npc id / fact bank / "global", value = JSON.
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS admin_config (
		kind TEXT NOT NULL,
		key TEXT NOT NULL,
		value TEXT NOT NULL,
		updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (kind, key)
	)`); err != nil {
		return nil, fmt.Errorf("migrate admin_config: %w", err)
	}
	return &Store{db: db}, nil
}

// SetAdmin grants or revokes the admin role by account name. Returns
// ErrNotFound if no such account exists yet.
func (s *Store) SetAdmin(name string, admin bool) error {
	res, err := s.db.Exec(`UPDATE players SET is_admin = ? WHERE name = ?`, admin, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// PlayerNames maps account ids to names, for showing whose dialogue
// history the admin is looking at.
func (s *Store) PlayerNames() (map[string]string, error) {
	rows, err := s.db.Query(`SELECT id, name FROM players`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		out[id] = name
	}
	return out, rows.Err()
}

// AdminConfig returns every override of one kind as key -> raw JSON.
func (s *Store) AdminConfig(kind string) (map[string]string, error) {
	rows, err := s.db.Query(`SELECT key, value FROM admin_config WHERE kind = ?`, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, err
		}
		out[key] = value
	}
	return out, rows.Err()
}

func (s *Store) SetAdminConfig(kind, key, value string) error {
	_, err := s.db.Exec(
		`INSERT INTO admin_config (kind, key, value, updated_at) VALUES (?, ?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(kind, key) DO UPDATE SET value = excluded.value, updated_at = CURRENT_TIMESTAMP`,
		kind, key, value,
	)
	return err
}

func (s *Store) DeleteAdminConfig(kind, key string) error {
	_, err := s.db.Exec(`DELETE FROM admin_config WHERE kind = ? AND key = ?`, kind, key)
	return err
}

// ResetToldFacts makes an NPC forget which facts it already told. Empty
// playerID = forget for every player.
func (s *Store) ResetToldFacts(npcID, playerID string) error {
	if playerID == "" {
		_, err := s.db.Exec(`DELETE FROM npc_memory WHERE npc_id = ?`, npcID)
		return err
	}
	_, err := s.db.Exec(`DELETE FROM npc_memory WHERE npc_id = ? AND player_id = ?`, npcID, playerID)
	return err
}

// ToldFactCounts returns, for one NPC, how many facts each player has heard.
func (s *Store) ToldFactCounts(npcID string) (map[string]int, error) {
	rows, err := s.db.Query(`SELECT player_id, COUNT(*) FROM npc_memory WHERE npc_id = ? GROUP BY player_id`, npcID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

// CreatePlayer registers a brand-new account. Callers must check PlayerByName
// first (httpapi does, to return a clean "name taken" error) — name is also
// UNIQUE at the DB level as a backstop.
func (s *Store) CreatePlayer(p Player) error {
	_, err := s.db.Exec(
		`INSERT INTO players (id, name, password_hash, token, token_expires_at, room, x, y, z, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)`,
		p.ID, p.Name, p.PasswordHash, p.Token, time.Now().Add(TokenTTL), p.Room, p.X, p.Y, p.Z,
	)
	return err
}

func (s *Store) PlayerByName(name string) (*Player, error) {
	row := s.db.QueryRow(
		`SELECT id, name, password_hash, token, token_expires_at, room, x, y, z, is_admin FROM players WHERE name = ?`, name,
	)
	var p Player
	if err := row.Scan(&p.ID, &p.Name, &p.PasswordHash, &p.Token, &p.TokenExpiresAt, &p.Room, &p.X, &p.Y, &p.Z, &p.IsAdmin); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &p, nil
}

// PlayerByToken also rejects an expired token as ErrNotFound (same as "no
// such token") — the caller (WS handshake) doesn't need to distinguish
// "never existed" from "expired", both just mean "log in again".
func (s *Store) PlayerByToken(token string) (*Player, error) {
	row := s.db.QueryRow(
		`SELECT id, name, password_hash, token, token_expires_at, room, x, y, z FROM players WHERE token = ?`, token,
	)
	var p Player
	if err := row.Scan(&p.ID, &p.Name, &p.PasswordHash, &p.Token, &p.TokenExpiresAt, &p.Room, &p.X, &p.Y, &p.Z); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if time.Now().After(p.TokenExpiresAt) {
		return nil, ErrNotFound
	}
	return &p, nil
}

// UpdateToken issues a fresh session token (with a fresh TTL) on login — the
// old one, if any client still holds it, stops working. Simplest way to not
// accumulate live tokens per account forever, and it bounds how long a
// leaked/forgotten token (e.g. in a browser's localStorage) stays useful.
func (s *Store) UpdateToken(id, token string) error {
	_, err := s.db.Exec(
		`UPDATE players SET token = ?, token_expires_at = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`,
		token, time.Now().Add(TokenTTL), id,
	)
	return err
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
