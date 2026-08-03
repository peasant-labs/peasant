package store

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// Lesson represents a generalized lesson extracted from a friction episode.
type Lesson struct {
	ID                  string
	EpisodeAnnotationID string
	SessionID           string
	Topic               string
	Rule                string
	FailureMode         string
	SituationEmbedding  []float32 // nil if not yet embedded
	CreatedAt           int64     // Unix milliseconds
}

// CreateLessonParams holds the parameters for inserting a new lesson.
type CreateLessonParams struct {
	EpisodeAnnotationID string
	SessionID           string
	Topic               string
	Rule                string
	FailureMode         string
}

// CreateLesson inserts a new lesson row and returns its UUID and a bool
// indicating whether the lesson was newly created (true) or already existed
// (false = duplicate). On both code paths a lesson_sources row is inserted to
// record provenance.
// Uses INSERT OR IGNORE + SELECT to avoid TOCTOU races under concurrent access.
func (s *Store) CreateLesson(ctx context.Context, p CreateLessonParams) (string, bool, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return "", false, fmt.Errorf("take conn: %w", err)
	}
	defer s.pool.Put(conn)

	id := uuid.New().String()
	now := time.Now().UnixMilli()

	// INSERT OR IGNORE: the UNIQUE(topic, rule, failure_mode) index silently
	// skips the insert if a duplicate exists. This is atomic — no TOCTOU window.
	err = sqlitex.ExecuteTransient(conn, `
		INSERT OR IGNORE INTO lessons (id, episode_annotation_id, session_id, topic, rule, failure_mode, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, &sqlitex.ExecOptions{
		Args: []any{id, p.EpisodeAnnotationID, p.SessionID, p.Topic, p.Rule, p.FailureMode, now},
	})
	if err != nil {
		return "", false, fmt.Errorf("insert lesson: %w", err)
	}

	created := conn.Changes() > 0
	lessonID := id

	// If the insert was ignored (duplicate), fetch the existing row's ID.
	if !created {
		var existingID string
		err = sqlitex.ExecuteTransient(conn, `
			SELECT id FROM lessons WHERE topic = ? AND rule = ? AND failure_mode = ?
		`, &sqlitex.ExecOptions{
			Args: []any{p.Topic, p.Rule, p.FailureMode},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				existingID = stmt.ColumnText(0)
				return nil
			},
		})
		if err != nil {
			return "", false, fmt.Errorf("fetch existing lesson: %w", err)
		}
		lessonID = existingID
	}

	// Record provenance — insert a lesson_sources row on both code paths.
	// INSERT OR IGNORE handles the case where the same (lesson, annotation, session)
	// triple is submitted again (e.g. re-running memory build on the same JSONL).
	sourceID := uuid.New().String()
	err = sqlitex.ExecuteTransient(conn, `
		INSERT OR IGNORE INTO lesson_sources (id, lesson_id, episode_annotation_id, session_id, created_at)
		VALUES (?, ?, ?, ?, ?)
	`, &sqlitex.ExecOptions{
		Args: []any{sourceID, lessonID, p.EpisodeAnnotationID, p.SessionID, now},
	})
	if err != nil {
		return "", false, fmt.Errorf("insert lesson_source: %w", err)
	}

	return lessonID, created, nil
}

// LessonSource represents a provenance record linking a lesson to its source episode.
type LessonSource struct {
	ID                  string
	LessonID            string
	EpisodeAnnotationID string
	SessionID           string
	CreatedAt           int64
}

// LessonSources returns all provenance records for a lesson, ordered by creation time.
func (s *Store) LessonSources(ctx context.Context, lessonID string) ([]LessonSource, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("take conn: %w", err)
	}
	defer s.pool.Put(conn)

	var sources []LessonSource
	err = sqlitex.ExecuteTransient(conn, `
		SELECT id, lesson_id, episode_annotation_id, session_id, created_at
		FROM lesson_sources
		WHERE lesson_id = ?
		ORDER BY created_at ASC
	`, &sqlitex.ExecOptions{
		Args: []any{lessonID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			sources = append(sources, LessonSource{
				ID:                  stmt.ColumnText(0),
				LessonID:            stmt.ColumnText(1),
				EpisodeAnnotationID: stmt.ColumnText(2),
				SessionID:           stmt.ColumnText(3),
				CreatedAt:           stmt.ColumnInt64(4),
			})
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("list lesson_sources: %w", err)
	}
	return sources, nil
}

// UpdateLessonEmbedding stores a float32 embedding vector for a lesson.
func (s *Store) UpdateLessonEmbedding(ctx context.Context, lessonID string, embedding []float32) error {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return fmt.Errorf("take conn: %w", err)
	}
	defer s.pool.Put(conn)

	blob := encodeFloat32s(embedding)
	err = sqlitex.ExecuteTransient(conn, `
		UPDATE lessons SET situation_embedding = ? WHERE id = ?
	`, &sqlitex.ExecOptions{
		Args: []any{blob, lessonID},
	})
	if err != nil {
		return fmt.Errorf("update embedding: %w", err)
	}
	return nil
}

// ListLessons returns all lessons, optionally filtered by session ID.
func (s *Store) ListLessons(ctx context.Context, sessionID string) ([]Lesson, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("take conn: %w", err)
	}
	defer s.pool.Put(conn)

	query := `SELECT id, episode_annotation_id, session_id, topic, rule, failure_mode, situation_embedding, created_at FROM lessons`
	var args []any
	if sessionID != "" {
		query += ` WHERE session_id = ?`
		args = []any{sessionID}
	}
	query += ` ORDER BY created_at ASC`

	var lessons []Lesson
	err = sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{
		Args: args,
		ResultFunc: func(stmt *sqlite.Stmt) error {
			l := Lesson{
				ID:                  stmt.ColumnText(0),
				EpisodeAnnotationID: stmt.ColumnText(1),
				SessionID:           stmt.ColumnText(2),
				Topic:               stmt.ColumnText(3),
				Rule:                stmt.ColumnText(4),
				FailureMode:         stmt.ColumnText(5),
				CreatedAt:           stmt.ColumnInt64(7),
			}
			if stmt.ColumnLen(6) > 0 {
				n := stmt.ColumnLen(6)
				blob := make([]byte, n)
				stmt.ColumnBytes(6, blob)
				l.SituationEmbedding = decodeFloat32s(blob)
			}
			lessons = append(lessons, l)
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("list lessons: %w", err)
	}
	return lessons, nil
}

// LessonsWithEmbeddings returns only lessons that have embeddings stored.
func (s *Store) LessonsWithEmbeddings(ctx context.Context) ([]Lesson, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("take conn: %w", err)
	}
	defer s.pool.Put(conn)

	var lessons []Lesson
	err = sqlitex.ExecuteTransient(conn, `
		SELECT id, episode_annotation_id, session_id, topic, rule, failure_mode, situation_embedding, created_at
		FROM lessons
		WHERE situation_embedding IS NOT NULL
		ORDER BY created_at ASC
	`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			n := stmt.ColumnLen(6)
			blob := make([]byte, n)
			stmt.ColumnBytes(6, blob)
			l := Lesson{
				ID:                  stmt.ColumnText(0),
				EpisodeAnnotationID: stmt.ColumnText(1),
				SessionID:           stmt.ColumnText(2),
				Topic:               stmt.ColumnText(3),
				Rule:                stmt.ColumnText(4),
				FailureMode:         stmt.ColumnText(5),
				SituationEmbedding:  decodeFloat32s(blob),
				CreatedAt:           stmt.ColumnInt64(7),
			}
			lessons = append(lessons, l)
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("list embedded lessons: %w", err)
	}
	return lessons, nil
}

// DeleteLessonsForSession removes all lessons for a session.
func (s *Store) DeleteLessonsForSession(ctx context.Context, sessionID string) (int, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return 0, fmt.Errorf("take conn: %w", err)
	}
	defer s.pool.Put(conn)

	err = sqlitex.ExecuteTransient(conn, `DELETE FROM lessons WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{sessionID},
	})
	if err != nil {
		return 0, fmt.Errorf("delete lessons: %w", err)
	}
	return conn.Changes(), nil
}

// encodeFloat32s converts a float32 slice to little-endian bytes.
func encodeFloat32s(v []float32) []byte {
	buf := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(f))
	}
	return buf
}

// decodeFloat32s converts little-endian bytes back to a float32 slice.
func decodeFloat32s(buf []byte) []float32 {
	n := len(buf) / 4
	v := make([]float32, n)
	for i := range n {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(buf[i*4:]))
	}
	return v
}

// FrictionStats holds aggregated friction episode counts for an eval period.
type FrictionStats struct {
	SessionCount int
	EpisodeCount int
	ByType       map[string]int // value → count
}

// FrictionStatsBefore returns friction stats for sessions starting before cutoffMs.
// Only counts annotations by annotators matching the given name prefix (e.g. "llm-judge").
func (s *Store) FrictionStatsBefore(ctx context.Context, cutoffMs int64, annotatorPrefix string) (*FrictionStats, error) {
	return s.frictionStats(ctx, "s.start_ms < ?", cutoffMs, annotatorPrefix)
}

// FrictionStatsAfter returns friction stats for sessions starting at or after cutoffMs.
func (s *Store) FrictionStatsAfter(ctx context.Context, cutoffMs int64, annotatorPrefix string) (*FrictionStats, error) {
	return s.frictionStats(ctx, "s.start_ms >= ?", cutoffMs, annotatorPrefix)
}

func (s *Store) frictionStats(ctx context.Context, timeClause string, cutoffMs int64, annotatorPrefix string) (*FrictionStats, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("take conn: %w", err)
	}
	defer s.pool.Put(conn)

	stats := &FrictionStats{ByType: make(map[string]int)}

	// Count annotated sessions in this period.
	err = sqlitex.ExecuteTransient(conn, `
		SELECT COUNT(DISTINCT ate.session_id)
		FROM annotation_target_entries ate
		JOIN annotations a ON ate.annotation_id = a.id
		JOIN annotation_types t ON a.annotation_type_id = t.id
		JOIN annotators ann ON a.annotator_id = ann.id
		JOIN sessions s ON ate.session_id = s.session_id
		WHERE t.type_id = 'research.friction_episode'
		AND a.value IN ('bad_handoff', 'bad_output', 'bad_process')
		AND ann.name LIKE ? || '%'
		AND `+timeClause, &sqlitex.ExecOptions{
		Args: []any{annotatorPrefix, cutoffMs},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			stats.SessionCount = stmt.ColumnInt(0)
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("count sessions: %w", err)
	}

	// Count episodes by type.
	err = sqlitex.ExecuteTransient(conn, `
		SELECT a.value, COUNT(*)
		FROM annotations a
		JOIN annotation_types t ON a.annotation_type_id = t.id
		JOIN annotators ann ON a.annotator_id = ann.id
		JOIN annotation_target_entries ate ON a.id = ate.annotation_id
		JOIN sessions s ON ate.session_id = s.session_id
		WHERE t.type_id = 'research.friction_episode'
		AND a.value IN ('bad_handoff', 'bad_output', 'bad_process')
		AND ann.name LIKE ? || '%'
		AND `+timeClause+`
		GROUP BY a.value`, &sqlitex.ExecOptions{
		Args: []any{annotatorPrefix, cutoffMs},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			val := stmt.ColumnText(0)
			count := stmt.ColumnInt(1)
			stats.ByType[val] = count
			stats.EpisodeCount += count
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("count episodes: %w", err)
	}

	return stats, nil
}

// LogInjectionEvent records an inject on/off event for a project.
func (s *Store) LogInjectionEvent(ctx context.Context, projectPath, event string) error {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return fmt.Errorf("take conn: %w", err)
	}
	defer s.pool.Put(conn)

	id := uuid.New().String()
	now := time.Now().UnixMilli()

	err = sqlitex.ExecuteTransient(conn, `
		INSERT INTO memory_injection_log (id, project_path, event, created_at)
		VALUES (?, ?, ?, ?)
	`, &sqlitex.ExecOptions{
		Args: []any{id, projectPath, event, now},
	})
	if err != nil {
		return fmt.Errorf("log injection event: %w", err)
	}
	return nil
}

// InjectionWindow represents a period when injection was active.
type InjectionWindow struct {
	ProjectPath string
	StartMs     int64
	EndMs       int64 // 0 means still active (no "off" event yet)
}

// InjectionWindows returns all on→off periods for all projects.
func (s *Store) InjectionWindows(ctx context.Context) ([]InjectionWindow, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("take conn: %w", err)
	}
	defer s.pool.Put(conn)

	type logEntry struct {
		projectPath string
		event       string
		createdAt   int64
	}

	var entries []logEntry
	err = sqlitex.ExecuteTransient(conn, `
		SELECT project_path, event, created_at
		FROM memory_injection_log
		ORDER BY project_path, created_at ASC
	`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			entries = append(entries, logEntry{
				projectPath: stmt.ColumnText(0),
				event:       stmt.ColumnText(1),
				createdAt:   stmt.ColumnInt64(2),
			})
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("query injection log: %w", err)
	}

	// Build windows: pair consecutive on→off events per project.
	var windows []InjectionWindow
	openWindows := make(map[string]int64) // project_path → start_ms

	for _, e := range entries {
		if e.event == "on" {
			openWindows[e.projectPath] = e.createdAt
		} else if e.event == "off" {
			if startMs, ok := openWindows[e.projectPath]; ok {
				windows = append(windows, InjectionWindow{
					ProjectPath: e.projectPath,
					StartMs:     startMs,
					EndMs:       e.createdAt,
				})
				delete(openWindows, e.projectPath)
			}
		}
	}

	// Still-open windows (injection currently active).
	for path, startMs := range openWindows {
		windows = append(windows, InjectionWindow{
			ProjectPath: path,
			StartMs:     startMs,
			EndMs:       0,
		})
	}

	return windows, nil
}

// FrictionStatsByInjection splits annotated sessions into those that overlapped
// with an injection window and those that didn't.
func (s *Store) FrictionStatsByInjection(ctx context.Context, annotatorPrefix string) (baseline, treatment *FrictionStats, err error) {
	windows, err := s.InjectionWindows(ctx)
	if err != nil {
		return nil, nil, err
	}

	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("take conn: %w", err)
	}
	defer s.pool.Put(conn)

	// Get all annotated sessions with their start times and project paths.
	type sessionInfo struct {
		sessionID string
		startMs   int64
		cwdPath   string
	}

	var sessions []sessionInfo
	err = sqlitex.ExecuteTransient(conn, `
		SELECT DISTINCT ate.session_id, s.start_ms, COALESCE(p.canonical_cwd, '')
		FROM annotation_target_entries ate
		JOIN annotations a ON ate.annotation_id = a.id
		JOIN annotation_types t ON a.annotation_type_id = t.id
		JOIN annotators ann ON a.annotator_id = ann.id
		JOIN sessions s ON ate.session_id = s.session_id
		LEFT JOIN projects p ON s.project_hash = p.project_hash
		WHERE t.type_id = 'research.friction_episode'
		AND a.value IN ('bad_handoff', 'bad_output', 'bad_process')
		AND ann.name LIKE ? || '%'
	`, &sqlitex.ExecOptions{
		Args: []any{annotatorPrefix},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			sessions = append(sessions, sessionInfo{
				sessionID: stmt.ColumnText(0),
				startMs:   stmt.ColumnInt64(1),
				cwdPath:   stmt.ColumnText(2),
			})
			return nil
		},
	})
	if err != nil {
		return nil, nil, fmt.Errorf("query sessions: %w", err)
	}

	// Classify each session as baseline or treatment.
	injectedSessions := make(map[string]bool)
	for _, sess := range sessions {
		for _, w := range windows {
			endMs := w.EndMs
			if endMs == 0 {
				endMs = time.Now().UnixMilli()
			}
			if sess.startMs >= w.StartMs && sess.startMs <= endMs {
				injectedSessions[sess.sessionID] = true
				break
			}
		}
	}

	// Now count episodes for each group.
	baseline = &FrictionStats{ByType: make(map[string]int)}
	treatment = &FrictionStats{ByType: make(map[string]int)}

	baselineSessions := make(map[string]bool)
	treatmentSessions := make(map[string]bool)

	err = sqlitex.ExecuteTransient(conn, `
		SELECT ate.session_id, a.value
		FROM annotations a
		JOIN annotation_types t ON a.annotation_type_id = t.id
		JOIN annotators ann ON a.annotator_id = ann.id
		JOIN annotation_target_entries ate ON a.id = ate.annotation_id
		WHERE t.type_id = 'research.friction_episode'
		AND a.value IN ('bad_handoff', 'bad_output', 'bad_process')
		AND ann.name LIKE ? || '%'
	`, &sqlitex.ExecOptions{
		Args: []any{annotatorPrefix},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			sid := stmt.ColumnText(0)
			val := stmt.ColumnText(1)
			if injectedSessions[sid] {
				treatment.EpisodeCount++
				treatment.ByType[val]++
				treatmentSessions[sid] = true
			} else {
				baseline.EpisodeCount++
				baseline.ByType[val]++
				baselineSessions[sid] = true
			}
			return nil
		},
	})
	if err != nil {
		return nil, nil, fmt.Errorf("count episodes: %w", err)
	}

	baseline.SessionCount = len(baselineSessions)
	treatment.SessionCount = len(treatmentSessions)

	return baseline, treatment, nil
}

// LessonsWithoutEmbeddings returns lessons that have NULL situation_embedding,
// ordered by creation time ascending.
func (s *Store) LessonsWithoutEmbeddings(ctx context.Context) ([]Lesson, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("take conn: %w", err)
	}
	defer s.pool.Put(conn)

	var lessons []Lesson
	err = sqlitex.ExecuteTransient(conn, `
		SELECT id, episode_annotation_id, session_id, topic, rule, failure_mode, created_at
		FROM lessons
		WHERE situation_embedding IS NULL
		ORDER BY created_at ASC
	`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			l := Lesson{
				ID:                  stmt.ColumnText(0),
				EpisodeAnnotationID: stmt.ColumnText(1),
				SessionID:           stmt.ColumnText(2),
				Topic:               stmt.ColumnText(3),
				Rule:                stmt.ColumnText(4),
				FailureMode:         stmt.ColumnText(5),
				CreatedAt:           stmt.ColumnInt64(6),
			}
			lessons = append(lessons, l)
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("list unembedded lessons: %w", err)
	}
	return lessons, nil
}

// CosineSimilarity computes the cosine similarity between two float32 vectors.
func CosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		ai, bi := float64(a[i]), float64(b[i])
		dot += ai * bi
		normA += ai * ai
		normB += bi * bi
	}
	denom := math.Sqrt(normA) * math.Sqrt(normB)
	if denom == 0 {
		return 0
	}
	return dot / denom
}
