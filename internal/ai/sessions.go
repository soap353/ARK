package ai

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync/atomic"
	"time"
)

var sessionSeq atomic.Uint64

// ChatSession represents a saved Ask AI conversation.
type ChatSession struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Pinned    bool   `json:"pinned"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// ChatMessage represents a single turn stored in a saved chat session.
type ChatMessage struct {
	ID          int64  `json:"id"`
	SessionID   string `json:"session_id"`
	Role        string `json:"role"`
	Content     string `json:"content"`
	Attachments string `json:"attachments,omitempty"`
	CreatedAt   string `json:"created_at"`
}

// EnsureChatSchema ensures chat session and message tables exist.
func EnsureChatSchema(ctx context.Context, db *sql.DB) error {
	if db == nil {
		return nil
	}
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS ai_chat_sessions (
			id TEXT PRIMARY KEY,
			title TEXT NOT NULL DEFAULT 'New Chat',
			pinned INTEGER NOT NULL DEFAULT 0,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
		CREATE TABLE IF NOT EXISTS ai_chat_messages (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL,
			role TEXT NOT NULL,
			content TEXT NOT NULL,
			attachments TEXT NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
		CREATE INDEX IF NOT EXISTS idx_ai_chat_msgs_session ON ai_chat_messages(session_id, id);
	`)
	return err
}

// PruneExpiredSessions removes unpinned chat sessions untouched for more than 48 hours.
func PruneExpiredSessions(ctx context.Context, db *sql.DB, now time.Time) (int64, error) {
	if db == nil {
		return 0, nil
	}
	if err := EnsureChatSchema(ctx, db); err != nil {
		return 0, err
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	cutoff := now.Add(-48 * time.Hour).Format("2006-01-02 15:04:05")
	_, _ = db.ExecContext(ctx, `
		DELETE FROM ai_chat_messages
		WHERE session_id IN (
			SELECT id FROM ai_chat_sessions WHERE pinned = 0 AND updated_at < ?
		)
	`, cutoff)
	res, err := db.ExecContext(ctx, `
		DELETE FROM ai_chat_sessions WHERE pinned = 0 AND updated_at < ?
	`, cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// CreateChatSession inserts a new chat session row and returns its ID.
func CreateChatSession(ctx context.Context, db *sql.DB, title string) (string, error) {
	if db == nil {
		return "", nil
	}
	if err := EnsureChatSchema(ctx, db); err != nil {
		return "", err
	}
	id := fmt.Sprintf("chat-%d-%d", time.Now().UnixNano(), sessionSeq.Add(1))
	t := ClampTitleWords(title, 4)
	if t == "" {
		t = "New Chat"
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO ai_chat_sessions (id, title, pinned, created_at, updated_at)
		VALUES (?, ?, 0, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
	`, id, t)
	return id, err
}

// AppendChatMessage records a chat message turn and bumps session updated_at.
func AppendChatMessage(ctx context.Context, db *sql.DB, sessionID, role, content, attachments string) error {
	if db == nil || sessionID == "" {
		return nil
	}
	if err := EnsureChatSchema(ctx, db); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO ai_chat_messages (session_id, role, content, attachments, created_at)
		VALUES (?, ?, ?, ?, CURRENT_TIMESTAMP)
	`, sessionID, role, content, attachments); err != nil {
		return err
	}
	_, _ = db.ExecContext(ctx, `UPDATE ai_chat_sessions SET updated_at = CURRENT_TIMESTAMP WHERE id = ?`, sessionID)
	return nil
}

// UpdateSessionTitle saves a max-4-word title onto an existing session.
func UpdateSessionTitle(ctx context.Context, db *sql.DB, sessionID, title string) error {
	if db == nil || sessionID == "" {
		return nil
	}
	t := ClampTitleWords(title, 4)
	if t == "" {
		return nil
	}
	_, err := db.ExecContext(ctx, `UPDATE ai_chat_sessions SET title = ? WHERE id = ?`, t, sessionID)
	return err
}

// SetSessionPinned marks a session as permanently kept (pinned=true) or 2-day temporary (pinned=false).
func SetSessionPinned(ctx context.Context, db *sql.DB, sessionID string, pinned bool) error {
	if db == nil || sessionID == "" {
		return nil
	}
	p := 0
	if pinned {
		p = 1
	}
	_, err := db.ExecContext(ctx, `UPDATE ai_chat_sessions SET pinned = ? WHERE id = ?`, p, sessionID)
	return err
}

// DeleteChatSession permanently deletes a chat session and its messages.
func DeleteChatSession(ctx context.Context, db *sql.DB, sessionID string) error {
	if db == nil || sessionID == "" {
		return nil
	}
	_, _ = db.ExecContext(ctx, `DELETE FROM ai_chat_messages WHERE session_id = ?`, sessionID)
	_, err := db.ExecContext(ctx, `DELETE FROM ai_chat_sessions WHERE id = ?`, sessionID)
	return err
}

// ListChatSessions prunes expired unpinned sessions and returns active sessions ordered by pinned/recency.
func ListChatSessions(ctx context.Context, db *sql.DB) ([]ChatSession, error) {
	if db == nil {
		return nil, nil
	}
	_, _ = PruneExpiredSessions(ctx, db, time.Now().UTC())
	rows, err := db.QueryContext(ctx, `
		SELECT id, title, pinned, created_at, updated_at
		FROM ai_chat_sessions
		ORDER BY pinned DESC, updated_at DESC
		LIMIT 50
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChatSession
	for rows.Next() {
		var s ChatSession
		var p int
		if err := rows.Scan(&s.ID, &s.Title, &p, &s.CreatedAt, &s.UpdatedAt); err == nil {
			s.Pinned = p == 1
			out = append(out, s)
		}
	}
	return out, nil
}

// GetChatSession loads a single session header and all its messages in chronological order.
func GetChatSession(ctx context.Context, db *sql.DB, sessionID string) (*ChatSession, []ChatMessage, error) {
	if db == nil || sessionID == "" {
		return nil, nil, sql.ErrNoRows
	}
	if err := EnsureChatSchema(ctx, db); err != nil {
		return nil, nil, err
	}
	var s ChatSession
	var p int
	err := db.QueryRowContext(ctx, `
		SELECT id, title, pinned, created_at, updated_at FROM ai_chat_sessions WHERE id = ?
	`, sessionID).Scan(&s.ID, &s.Title, &p, &s.CreatedAt, &s.UpdatedAt)
	if err != nil {
		return nil, nil, err
	}
	s.Pinned = p == 1

	rows, err := db.QueryContext(ctx, `
		SELECT id, session_id, role, content, COALESCE(attachments, ''), created_at
		FROM ai_chat_messages WHERE session_id = ? ORDER BY id ASC
	`, sessionID)
	if err != nil {
		return &s, nil, err
	}
	defer rows.Close()
	var msgs []ChatMessage
	for rows.Next() {
		var m ChatMessage
		if err := rows.Scan(&m.ID, &m.SessionID, &m.Role, &m.Content, &m.Attachments, &m.CreatedAt); err == nil {
			msgs = append(msgs, m)
		}
	}
	return &s, msgs, nil
}

// GenerateSessionTitle returns a max-4-word session title based strictly on the user's query.
// If jsonTitle (already returned inside the AI's JSON response) is valid, it is used directly;
// otherwise it derives the title from question alone (never the AI output).
func GenerateSessionTitle(ctx context.Context, conn Conn, question, jsonTitle string) string {
	if fromJSON := ClampTitleWords(jsonTitle, 4); fromJSON != "" && !strings.EqualFold(fromJSON, "New Chat") {
		return fromJSON
	}
	fallback := TitleFromQuery(question, 4)
	if fallback == "" {
		fallback = "Offline Chat"
	}
	if strings.TrimSpace(conn.BaseURL) == "" || strings.TrimSpace(conn.ChatModel) == "" {
		return fallback
	}

	tCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()

	qClip := strings.TrimSpace(question)
	if len([]rune(qClip)) > 200 {
		qClip = string([]rune(qClip)[:200])
	}

	rawTitle, err := conn.ChatStream(tCtx, []Msg{
		{Role: "system", Content: "Create a 1 to 4 word title based ONLY on the user's query. Output ONLY the title words with no quotes, no punctuation, and no markdown."},
		{Role: "user", Content: qClip},
	}, nil)
	if err != nil {
		return fallback
	}
	cleaned := ClampTitleWords(rawTitle, 4)
	if cleaned == "" {
		return fallback
	}
	return cleaned
}

// TitleFromQuery extracts up to maxWords meaningful words directly from the user's query.
func TitleFromQuery(question string, maxWords int) string {
	if maxWords <= 0 {
		maxWords = 4
	}
	clean := strings.NewReplacer(`"`, "", `'`, "", "`", "", "*", "", "#", "", "?", "", "!", "", ".", " ", ",", " ", ":", " ", ";", " ", "\n", " ", "\r", " ").Replace(question)
	words := strings.Fields(clean)
	var kept []string
	for _, w := range words {
		low := strings.ToLower(w)
		if ftsStopWords[low] || low == "what" || low == "is" || low == "are" || low == "the" || low == "a" || low == "an" {
			continue
		}
		kept = append(kept, w)
		if len(kept) >= maxWords {
			break
		}
	}
	if len(kept) == 0 {
		return ClampTitleWords(question, maxWords)
	}
	return ClampTitleWords(strings.Join(kept, " "), maxWords)
}

// ClampTitleWords strips quotes/markdown/punctuation and caps the title at maxWords (default 4).
func ClampTitleWords(raw string, maxWords int) string {
	if maxWords <= 0 {
		maxWords = 4
	}
	clean := strings.NewReplacer(`"`, "", `'`, "", "`", "", "*", "", "#", "", ":", " ", "\n", " ", "\r", " ").Replace(raw)
	words := strings.Fields(clean)
	if len(words) == 0 {
		return ""
	}
	if len(words) > maxWords {
		words = words[:maxWords]
	}
	out := strings.Trim(strings.Join(words, " "), ".,;!?- ")
	return out
}
