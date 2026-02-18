package server

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"langopen.dev/pkg/contracts"
	"langopen.dev/pkg/observability"
)

type contextKey string

const projectIDContextKey contextKey = "project_id"

var errRunConflict = errors.New("run_conflict")

type Server struct {
	logger *slog.Logger
	router chi.Router
	store  *memoryStore
	pg     *pgxpool.Pool
	redis  *redis.Client
	cfg    serverConfig
}

type serverConfig struct {
	QueueKey            string
	CancelKeyPrefix     string
	CancelChannelPrefix string
	StreamChannelPrefix string
	StreamBufferPrefix  string
	StreamBufferTTL     time.Duration
}

type createRunRequest struct {
	AssistantID       string `json:"assistant_id"`
	MultitaskStrategy string `json:"multitask_strategy"`
	StreamResumable   *bool  `json:"stream_resumable"`
	WebhookURL        string `json:"webhook_url"`
}

type memoryStore struct {
	mu         sync.RWMutex
	assistants map[string]contracts.Assistant
	threads    map[string]contracts.Thread
	runs       map[string]contracts.Run
	events     map[string][]streamEvent
	crons      map[string]contracts.CronJob
	storeItems map[string]storeRecord
}

type streamEvent struct {
	ID    int64           `json:"id"`
	Event string          `json:"event"`
	Data  json.RawMessage `json:"data"`
}

type storeRecord struct {
	ID        string         `json:"id"`
	Namespace string         `json:"namespace"`
	Key       string         `json:"key"`
	Value     any            `json:"value"`
	Metadata  map[string]any `json:"metadata,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
}

func New(logger *slog.Logger) (*Server, error) {
	s := &Server{
		logger: logger,
		store: &memoryStore{
			assistants: map[string]contracts.Assistant{},
			threads:    map[string]contracts.Thread{},
			runs:       map[string]contracts.Run{},
			events:     map[string][]streamEvent{},
			crons:      map[string]contracts.CronJob{},
			storeItems: map[string]storeRecord{},
		},
		cfg: serverConfig{
			QueueKey:            envOrDefault("RUN_WAKEUP_LIST", "langopen:runs:wakeup"),
			CancelKeyPrefix:     envOrDefault("RUN_CANCEL_KEY_PREFIX", "langopen:run:cancel:"),
			CancelChannelPrefix: envOrDefault("RUN_CANCEL_CHANNEL_PREFIX", "langopen:run:cancel:ch:"),
			StreamChannelPrefix: envOrDefault("RUN_STREAM_CHANNEL_PREFIX", "langopen:run:stream:"),
			StreamBufferPrefix:  envOrDefault("RUN_STREAM_BUFFER_PREFIX", "langopen:run:events:"),
			StreamBufferTTL:     time.Duration(envIntOrDefault("RUN_STREAM_BUFFER_TTL_SECONDS", 3600)) * time.Second,
		},
	}

	if dsn := os.Getenv("POSTGRES_DSN"); dsn != "" {
		pg, err := pgxpool.New(context.Background(), dsn)
		if err != nil {
			return nil, fmt.Errorf("connect postgres: %w", err)
		}
		s.pg = pg
		if envBoolOrDefault("MIGRATIONS_AUTO_APPLY", false) {
			if err := s.applyMigrations(context.Background()); err != nil {
				return nil, fmt.Errorf("apply migrations: %w", err)
			}
		}
		if err := s.ensureBootstrapData(context.Background()); err != nil {
			return nil, fmt.Errorf("bootstrap postgres defaults: %w", err)
		}
	}
	if addr := os.Getenv("REDIS_ADDR"); addr != "" {
		s.redis = redis.NewClient(&redis.Options{
			Addr:     addr,
			Username: strings.TrimSpace(os.Getenv("REDIS_USERNAME")),
			Password: os.Getenv("REDIS_PASSWORD"),
		})
	}

	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(middleware.RealIP)
	r.Use(observability.CorrelationMiddleware(logger))
	r.Use(s.authMiddleware)

	r.Get("/healthz", s.healthz)
	r.Get("/docs", s.docs)
	r.Get("/openapi.json", s.openapi)

	r.Route("/api/v1", func(api chi.Router) {
		api.Get("/assistants", s.listAssistants)
		api.Post("/assistants", s.createAssistant)
		api.Get("/threads", s.listThreads)
		api.Post("/threads", s.createThread)
		api.Post("/threads/{thread_id}/runs/stream", s.createRunStream)
		api.Get("/threads/{thread_id}/runs/{run_id}/stream", s.joinRunStream)
		api.Post("/threads/{thread_id}/runs/{run_id}/cancel", s.cancelRun)
		api.Post("/runs/stream", s.createStatelessRunStream)
		api.Get("/runs/{run_id}/stream", s.joinRunStream)
		api.Post("/runs/{run_id}/cancel", s.cancelRun)
		api.Get("/runs/{run_id}", s.getRun)
		api.Get("/store/items", s.listStoreItems)
		api.Post("/store/items", s.putStoreItem)
		api.Get("/store/items/{namespace}/{key}", s.getStoreItem)
		api.Delete("/store/items/{namespace}/{key}", s.deleteStoreItem)
		api.Get("/crons", s.listCrons)
		api.Post("/crons", s.createCron)
		api.Get("/system", s.systemInfo)
		api.Get("/system/health", s.systemHealth)
	})

	r.Post("/a2a/{assistant_id}", s.a2a)
	r.Post("/mcp", s.mcp)

	s.router = r
	return s, nil
}

func (s *Server) Router() http.Handler { return s.router }

func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.URL.Path == "/docs" || r.URL.Path == "/openapi.json" {
			next.ServeHTTP(w, r)
			return
		}

		apiKey := strings.TrimSpace(r.Header.Get(contracts.HeaderAPIKey))
		if apiKey == "" {
			contracts.WriteError(w, http.StatusUnauthorized, "missing_api_key", "X-Api-Key header is required", observability.RequestIDFromContext(r.Context()))
			return
		}
		authScheme := strings.TrimSpace(r.Header.Get(contracts.HeaderAuthScheme))
		if authScheme != "" && authScheme != "langsmith-api-key" {
			contracts.WriteError(w, http.StatusBadRequest, "invalid_auth_scheme", "X-Auth-Scheme must be langsmith-api-key when provided", observability.RequestIDFromContext(r.Context()))
			return
		}

		if s.pg != nil {
			projectID, err := s.validateAPIKey(r.Context(), apiKey)
			if err != nil {
				contracts.WriteError(w, http.StatusUnauthorized, "invalid_api_key", "API key is invalid or revoked", observability.RequestIDFromContext(r.Context()))
				return
			}
			r = r.WithContext(context.WithValue(r.Context(), projectIDContextKey, projectID))
		}

		next.ServeHTTP(w, r)
	})
}

func (s *Server) validateAPIKey(ctx context.Context, apiKey string) (string, error) {
	if s.pg == nil {
		return "", nil
	}
	hash := hashAPIKey(apiKey)
	var projectID string
	err := s.pg.QueryRow(ctx, `SELECT project_id FROM api_keys WHERE key_hash=$1 AND revoked_at IS NULL LIMIT 1`, hash).Scan(&projectID)
	if err != nil {
		return "", err
	}
	return projectID, nil
}

func (s *Server) ensureBootstrapData(ctx context.Context) error {
	if s.pg == nil {
		return nil
	}
	_, err := s.pg.Exec(ctx, `
		INSERT INTO organizations (id, name) VALUES ('org_default', 'Default Org') ON CONFLICT (id) DO NOTHING;
		INSERT INTO projects (id, organization_id, name) VALUES ('proj_default', 'org_default', 'Default Project') ON CONFLICT (id) DO NOTHING;
		INSERT INTO deployments (id, project_id, repo_url, git_ref, repo_path, runtime_profile, mode)
		VALUES ('dep_default', 'proj_default', 'bootstrap://local', 'main', '/', 'gvisor', 'mode_a')
		ON CONFLICT (id) DO NOTHING;
		INSERT INTO assistants (id, deployment_id, graph_id, config, version)
		VALUES ('asst_default', 'dep_default', 'default', '{}'::jsonb, 'v1')
		ON CONFLICT (id) DO NOTHING;
	`)
	if err != nil {
		return err
	}

	if bootstrapAPIKey := strings.TrimSpace(os.Getenv("BOOTSTRAP_API_KEY")); bootstrapAPIKey != "" {
		_, err = s.pg.Exec(ctx, `
			INSERT INTO api_keys (id, project_id, name, key_hash, created_at, revoked_at)
			VALUES ('key_bootstrap', 'proj_default', 'bootstrap', $1, NOW(), NULL)
			ON CONFLICT (id) DO UPDATE SET key_hash = EXCLUDED.key_hash, revoked_at = NULL
		`, hashAPIKey(bootstrapAPIKey))
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) applyMigrations(ctx context.Context) error {
	if s.pg == nil {
		return nil
	}
	migrationsDir := os.Getenv("MIGRATIONS_DIR")
	if migrationsDir == "" {
		migrationsDir = resolveMigrationsDir()
	}

	if _, err := os.Stat(migrationsDir); err != nil {
		return fmt.Errorf("stat migrations dir %s: %w", migrationsDir, err)
	}
	if _, err := s.pg.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`); err != nil {
		return err
	}

	entries, err := os.ReadDir(migrationsDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		var applied bool
		if err := s.pg.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=$1)`, entry.Name()).Scan(&applied); err != nil {
			return err
		}
		if applied {
			continue
		}

		path := filepath.Join(migrationsDir, entry.Name())
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		tx, err := s.pg.BeginTx(ctx, pgx.TxOptions{})
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(content)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("apply migration %s: %w", entry.Name(), err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version, applied_at) VALUES ($1, NOW())`, entry.Name()); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		s.logger.Info("migration_applied", "version", entry.Name())
	}
	return nil
}

func hashAPIKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (s *Server) docs(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(`<html><body><h1>LangOpen API Docs</h1><p>OpenAPI spec: <a href="/openapi.json">/openapi.json</a></p></body></html>`))
}

func (s *Server) openapi(w http.ResponseWriter, _ *http.Request) {
	path := os.Getenv("OPENAPI_SPEC_PATH")
	if path == "" {
		path = resolveSpecPath("agent-server.json")
	}
	content, err := os.ReadFile(path)
	if err != nil {
		contracts.WriteError(w, http.StatusInternalServerError, "openapi_unavailable", err.Error(), "")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(content)
}

func resolveSpecPath(file string) string {
	wd, err := os.Getwd()
	if err != nil {
		return filepath.Join("contracts", "openapi", file)
	}
	current := wd
	for range 8 {
		candidate := filepath.Join(current, "contracts", "openapi", file)
		if _, statErr := os.Stat(candidate); statErr == nil {
			return candidate
		}
		next := filepath.Dir(current)
		if next == current {
			break
		}
		current = next
	}
	return filepath.Join("contracts", "openapi", file)
}

func resolveMigrationsDir() string {
	wd, err := os.Getwd()
	if err != nil {
		return filepath.Join("db", "migrations")
	}
	current := wd
	for range 8 {
		candidate := filepath.Join(current, "db", "migrations")
		if info, statErr := os.Stat(candidate); statErr == nil && info.IsDir() {
			return candidate
		}
		next := filepath.Dir(current)
		if next == current {
			break
		}
		current = next
	}
	return filepath.Join("db", "migrations")
}

func (s *Server) listAssistants(w http.ResponseWriter, r *http.Request) {
	if s.pg != nil {
		rows, err := s.pg.Query(r.Context(), `SELECT id, deployment_id, graph_id, config, version FROM assistants ORDER BY created_at DESC LIMIT 200`)
		if err != nil {
			contracts.WriteError(w, http.StatusInternalServerError, "db_query_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
			return
		}
		defer rows.Close()

		out := make([]contracts.Assistant, 0)
		for rows.Next() {
			var a contracts.Assistant
			var raw []byte
			if err := rows.Scan(&a.ID, &a.DeploymentID, &a.GraphID, &raw, &a.Version); err != nil {
				contracts.WriteError(w, http.StatusInternalServerError, "db_scan_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
				return
			}
			a.Config = map[string]string{}
			_ = json.Unmarshal(raw, &a.Config)
			out = append(out, a)
		}
		writeJSON(w, http.StatusOK, out)
		return
	}

	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	out := make([]contracts.Assistant, 0, len(s.store.assistants))
	for _, a := range s.store.assistants {
		out = append(out, a)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) createAssistant(w http.ResponseWriter, r *http.Request) {
	var in contracts.Assistant
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_json", err.Error(), observability.RequestIDFromContext(r.Context()))
		return
	}
	in.ID = contracts.NewID("asst")
	if in.Version == "" {
		in.Version = "v1"
	}
	if in.DeploymentID == "" {
		in.DeploymentID = "dep_default"
	}

	if s.pg != nil {
		cfg := map[string]string{}
		for k, v := range in.Config {
			cfg[k] = v
		}
		raw, _ := json.Marshal(cfg)
		_, err := s.pg.Exec(r.Context(), `INSERT INTO assistants (id, deployment_id, graph_id, config, version) VALUES ($1,$2,$3,$4,$5)`, in.ID, in.DeploymentID, in.GraphID, raw, in.Version)
		if err != nil {
			contracts.WriteError(w, http.StatusBadRequest, "db_insert_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
			return
		}
		writeJSON(w, http.StatusCreated, in)
		return
	}

	s.store.mu.Lock()
	s.store.assistants[in.ID] = in
	s.store.mu.Unlock()
	writeJSON(w, http.StatusCreated, in)
}

func (s *Server) listThreads(w http.ResponseWriter, r *http.Request) {
	if s.pg != nil {
		rows, err := s.pg.Query(r.Context(), `SELECT id, assistant_id, metadata, updated_at FROM threads ORDER BY updated_at DESC LIMIT 200`)
		if err != nil {
			contracts.WriteError(w, http.StatusInternalServerError, "db_query_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
			return
		}
		defer rows.Close()
		out := make([]contracts.Thread, 0)
		for rows.Next() {
			var t contracts.Thread
			var raw []byte
			if err := rows.Scan(&t.ID, &t.AssistantID, &raw, &t.UpdatedAt); err != nil {
				contracts.WriteError(w, http.StatusInternalServerError, "db_scan_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
				return
			}
			t.Metadata = map[string]string{}
			_ = json.Unmarshal(raw, &t.Metadata)
			out = append(out, t)
		}
		writeJSON(w, http.StatusOK, out)
		return
	}

	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	out := make([]contracts.Thread, 0, len(s.store.threads))
	for _, t := range s.store.threads {
		out = append(out, t)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) createThread(w http.ResponseWriter, r *http.Request) {
	var in contracts.Thread
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil && err.Error() != "EOF" {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_json", err.Error(), observability.RequestIDFromContext(r.Context()))
		return
	}
	if in.AssistantID == "" {
		in.AssistantID = "asst_default"
	}
	in.ID = contracts.NewID("thread")
	in.UpdatedAt = time.Now().UTC()

	if s.pg != nil {
		raw, _ := json.Marshal(in.Metadata)
		_, err := s.pg.Exec(r.Context(), `INSERT INTO threads (id, assistant_id, metadata, updated_at, created_at) VALUES ($1,$2,$3,$4,NOW())`, in.ID, in.AssistantID, raw, in.UpdatedAt)
		if err != nil {
			contracts.WriteError(w, http.StatusBadRequest, "db_insert_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
			return
		}
		writeJSON(w, http.StatusCreated, in)
		return
	}

	s.store.mu.Lock()
	s.store.threads[in.ID] = in
	s.store.mu.Unlock()
	writeJSON(w, http.StatusCreated, in)
}

func (s *Server) createRunStream(w http.ResponseWriter, r *http.Request) {
	threadID := chi.URLParam(r, "thread_id")
	var req createRunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err.Error() != "EOF" {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_json", err.Error(), observability.RequestIDFromContext(r.Context()))
		return
	}
	if req.AssistantID == "" {
		req.AssistantID = "asst_default"
	}
	strategy := contracts.MultitaskStrategy(strings.TrimSpace(req.MultitaskStrategy))
	if strategy == "" {
		strategy = contracts.MultitaskEnqueue
	}
	if !isValidMultitaskStrategy(strategy) {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_multitask_strategy", "multitask_strategy must be reject, rollback, interrupt, enqueue", observability.RequestIDFromContext(r.Context()))
		return
	}
	streamResumable := true
	if req.StreamResumable != nil {
		streamResumable = *req.StreamResumable
	}

	var run contracts.Run
	var err error
	if s.pg != nil {
		run, err = s.createRunInPostgres(r.Context(), threadID, req.AssistantID, strategy, streamResumable, strings.TrimSpace(req.WebhookURL))
		if err != nil {
			if errors.Is(err, errRunConflict) {
				contracts.WriteError(w, http.StatusConflict, "run_conflict", "thread already has active run", observability.RequestIDFromContext(r.Context()))
				return
			}
			contracts.WriteError(w, http.StatusInternalServerError, "create_run_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
			return
		}
		if s.redis != nil {
			_ = s.redis.LPush(r.Context(), s.cfg.QueueKey, "wake").Err()
		}
	} else {
		run = s.createRunInMemory(threadID, req.AssistantID, strategy, streamResumable)
	}

	events := []streamEvent{mkEvent(1, "run_queued", map[string]any{"run_id": run.ID, "thread_id": run.ThreadID, "status": run.Status})}
	for _, ev := range events {
		s.persistEvent(r.Context(), run.ID, run.StreamResumable, ev)
	}

	if s.pg == nil {
		completionEvents := []streamEvent{
			mkEvent(2, "token", map[string]string{"token": "hello"}),
			mkEvent(3, "run_completed", map[string]string{"status": string(contracts.RunStatusSuccess), "id": run.ID}),
		}
		s.store.mu.Lock()
		run.Status = contracts.RunStatusSuccess
		run.UpdatedAt = time.Now().UTC()
		s.store.runs[run.ID] = run
		s.store.events[run.ID] = append(s.store.events[run.ID], completionEvents...)
		s.store.mu.Unlock()
		events = append(events, completionEvents...)
	}

	streamSSE(w, r, events, 0)
}

func (s *Server) createStatelessRunStream(w http.ResponseWriter, r *http.Request) {
	var req createRunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err.Error() != "EOF" {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_json", err.Error(), observability.RequestIDFromContext(r.Context()))
		return
	}
	if req.AssistantID == "" {
		req.AssistantID = "asst_default"
	}
	strategy := contracts.MultitaskStrategy(strings.TrimSpace(req.MultitaskStrategy))
	if strategy == "" {
		strategy = contracts.MultitaskEnqueue
	}
	if !isValidMultitaskStrategy(strategy) {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_multitask_strategy", "multitask_strategy must be reject, rollback, interrupt, enqueue", observability.RequestIDFromContext(r.Context()))
		return
	}
	streamResumable := true
	if req.StreamResumable != nil {
		streamResumable = *req.StreamResumable
	}

	var (
		run contracts.Run
		err error
	)
	if s.pg != nil {
		run, err = s.createStatelessRunInPostgres(r.Context(), req.AssistantID, strategy, streamResumable, strings.TrimSpace(req.WebhookURL))
		if err != nil {
			contracts.WriteError(w, http.StatusInternalServerError, "create_run_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
			return
		}
		if s.redis != nil {
			_ = s.redis.LPush(r.Context(), s.cfg.QueueKey, "wake").Err()
		}
	} else {
		run = s.createRunInMemory("", req.AssistantID, strategy, streamResumable)
	}

	events := []streamEvent{
		mkEvent(1, "run_queued", map[string]any{
			"run_id":       run.ID,
			"assistant_id": run.AssistantID,
			"status":       run.Status,
		}),
	}
	for _, ev := range events {
		s.persistEvent(r.Context(), run.ID, run.StreamResumable, ev)
	}

	if s.pg == nil {
		completionEvents := []streamEvent{
			mkEvent(2, "token", map[string]string{"token": "hello"}),
			mkEvent(3, "run_completed", map[string]string{"status": string(contracts.RunStatusSuccess), "id": run.ID}),
		}
		s.store.mu.Lock()
		run.Status = contracts.RunStatusSuccess
		run.UpdatedAt = time.Now().UTC()
		s.store.runs[run.ID] = run
		s.store.events[run.ID] = append(s.store.events[run.ID], completionEvents...)
		s.store.mu.Unlock()
		events = append(events, completionEvents...)
	}

	streamSSE(w, r, events, 0)
}

func (s *Server) createRunInMemory(threadID, assistantID string, strategy contracts.MultitaskStrategy, streamResumable bool) contracts.Run {
	runID := contracts.NewID("run")
	now := time.Now().UTC()
	run := contracts.Run{
		ID:                runID,
		ThreadID:          threadID,
		Status:            contracts.RunStatusPending,
		AssistantID:       assistantID,
		MultitaskStrategy: strategy,
		StreamResumable:   streamResumable,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	s.store.mu.Lock()
	s.store.runs[runID] = run
	s.store.mu.Unlock()
	return run
}

func (s *Server) createRunInPostgres(ctx context.Context, threadID, assistantID string, strategy contracts.MultitaskStrategy, streamResumable bool, webhookURL string) (contracts.Run, error) {
	tx, err := s.pg.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return contracts.Run{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var threadExists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM threads WHERE id=$1)`, threadID).Scan(&threadExists); err != nil {
		return contracts.Run{}, err
	}
	if !threadExists {
		_, err = tx.Exec(ctx, `INSERT INTO threads (id, assistant_id, metadata, updated_at, created_at) VALUES ($1,$2,'{}'::jsonb,NOW(),NOW())`, threadID, assistantID)
		if err != nil {
			return contracts.Run{}, err
		}
	}

	var activeRunID string
	var activeStatus string
	err = tx.QueryRow(ctx, `SELECT id, status FROM runs WHERE thread_id=$1 AND status IN ('pending','running') ORDER BY created_at ASC LIMIT 1`, threadID).Scan(&activeRunID, &activeStatus)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return contracts.Run{}, err
	}
	if activeRunID != "" {
		switch strategy {
		case contracts.MultitaskReject:
			return contracts.Run{}, errRunConflict
		case contracts.MultitaskInterrupt:
			_, err = tx.Exec(ctx, `UPDATE runs SET status='interrupted', updated_at=NOW() WHERE id=$1`, activeRunID)
			if err != nil {
				return contracts.Run{}, err
			}
		case contracts.MultitaskRollback:
			_, err = tx.Exec(ctx, `DELETE FROM checkpoints WHERE run_id=$1`, activeRunID)
			if err != nil {
				return contracts.Run{}, err
			}
			_, err = tx.Exec(ctx, `DELETE FROM runs WHERE id=$1`, activeRunID)
			if err != nil {
				return contracts.Run{}, err
			}
		case contracts.MultitaskEnqueue:
		}
	}

	now := time.Now().UTC()
	run := contracts.Run{
		ID:                contracts.NewID("run"),
		ThreadID:          threadID,
		AssistantID:       assistantID,
		Status:            contracts.RunStatusPending,
		MultitaskStrategy: strategy,
		StreamResumable:   streamResumable,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO runs (id, thread_id, assistant_id, status, multitask_strategy, stream_resumable, webhook_url, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
	`, run.ID, run.ThreadID, run.AssistantID, run.Status, run.MultitaskStrategy, run.StreamResumable, webhookURL, run.CreatedAt, run.UpdatedAt)
	if err != nil {
		return contracts.Run{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return contracts.Run{}, err
	}
	return run, nil
}

func (s *Server) joinRunStream(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "run_id")
	startFrom := parseStartFrom(r.Header.Get("Last-Event-ID"))

	events := s.loadEvents(r.Context(), runID)
	streamSSE(w, r, events, startFrom)
}

func parseStartFrom(lastID string) int64 {
	if lastID == "" || lastID == "-1" {
		return 0
	}
	v, err := strconv.ParseInt(lastID, 10, 64)
	if err != nil {
		return 0
	}
	return v + 1
}

func (s *Server) persistEvent(ctx context.Context, runID string, resumable bool, event streamEvent) {
	if s.redis != nil {
		channel := s.cfg.StreamChannelPrefix + runID
		payload, _ := json.Marshal(event)
		_ = s.redis.Publish(ctx, channel, payload).Err()
		if resumable {
			bufferKey := s.cfg.StreamBufferPrefix + runID
			_ = s.redis.RPush(ctx, bufferKey, payload).Err()
			_ = s.redis.Expire(ctx, bufferKey, s.cfg.StreamBufferTTL).Err()
		}
	}

	s.store.mu.Lock()
	s.store.events[runID] = append(s.store.events[runID], event)
	s.store.mu.Unlock()
}

func (s *Server) loadEvents(ctx context.Context, runID string) []streamEvent {
	if s.redis != nil {
		items, err := s.redis.LRange(ctx, s.cfg.StreamBufferPrefix+runID, 0, -1).Result()
		if err == nil && len(items) > 0 {
			events := make([]streamEvent, 0, len(items))
			for _, item := range items {
				var ev streamEvent
				if unmarshalErr := json.Unmarshal([]byte(item), &ev); unmarshalErr == nil {
					events = append(events, ev)
				}
			}
			if len(events) > 0 {
				return events
			}
		}
	}

	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	return append([]streamEvent(nil), s.store.events[runID]...)
}

func (s *Server) cancelRun(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "run_id")
	action := r.URL.Query().Get("action")
	if action == "" {
		action = "interrupt"
	}
	if action != "interrupt" && action != "rollback" {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_action", "action must be interrupt or rollback", observability.RequestIDFromContext(r.Context()))
		return
	}

	if s.redis != nil {
		_ = s.redis.Set(r.Context(), s.cfg.CancelKeyPrefix+runID, action, 10*time.Minute).Err()
		_ = s.redis.Publish(r.Context(), s.cfg.CancelChannelPrefix+runID, action).Err()
	}

	if s.pg != nil {
		if err := s.cancelRunInPostgres(r.Context(), runID, action); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				contracts.WriteError(w, http.StatusNotFound, "run_not_found", "run not found", observability.RequestIDFromContext(r.Context()))
				return
			}
			contracts.WriteError(w, http.StatusInternalServerError, "cancel_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
			return
		}
		if action == "rollback" && s.redis != nil {
			_ = s.redis.Del(r.Context(), s.cfg.StreamBufferPrefix+runID).Err()
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "action": action})
		return
	}

	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	run, ok := s.store.runs[runID]
	if !ok {
		contracts.WriteError(w, http.StatusNotFound, "run_not_found", "run not found", observability.RequestIDFromContext(r.Context()))
		return
	}
	switch action {
	case "interrupt":
		run.Status = contracts.RunStatusInterrupted
		run.UpdatedAt = time.Now().UTC()
		s.store.runs[runID] = run
	case "rollback":
		delete(s.store.runs, runID)
		delete(s.store.events, runID)
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "action": action})
}

func (s *Server) cancelRunInPostgres(ctx context.Context, runID, action string) error {
	tx, err := s.pg.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM runs WHERE id=$1)`, runID).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return sql.ErrNoRows
	}

	if action == "interrupt" {
		_, err = tx.Exec(ctx, `UPDATE runs SET status='interrupted', updated_at=NOW() WHERE id=$1`, runID)
		if err != nil {
			return err
		}
	} else {
		_, err = tx.Exec(ctx, `DELETE FROM checkpoints WHERE run_id=$1`, runID)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `DELETE FROM runs WHERE id=$1`, runID)
		if err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *Server) getRun(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "run_id")
	if s.pg != nil {
		run, err := s.getRunFromPostgres(r.Context(), runID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				contracts.WriteError(w, http.StatusNotFound, "run_not_found", "run not found", observability.RequestIDFromContext(r.Context()))
				return
			}
			contracts.WriteError(w, http.StatusInternalServerError, "db_query_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
			return
		}
		writeJSON(w, http.StatusOK, run)
		return
	}

	s.store.mu.RLock()
	run, ok := s.store.runs[runID]
	s.store.mu.RUnlock()
	if !ok {
		contracts.WriteError(w, http.StatusNotFound, "run_not_found", "run not found", observability.RequestIDFromContext(r.Context()))
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) getRunFromPostgres(ctx context.Context, runID string) (contracts.Run, error) {
	var run contracts.Run
	var status string
	var strategy string
	err := s.pg.QueryRow(ctx, `
		SELECT id, COALESCE(thread_id,''), assistant_id, status, multitask_strategy, stream_resumable, created_at, updated_at
		FROM runs WHERE id=$1
	`, runID).Scan(&run.ID, &run.ThreadID, &run.AssistantID, &status, &strategy, &run.StreamResumable, &run.CreatedAt, &run.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return contracts.Run{}, sql.ErrNoRows
		}
		return contracts.Run{}, err
	}
	run.Status = contracts.RunStatus(status)
	run.MultitaskStrategy = contracts.MultitaskStrategy(strategy)
	return run, nil
}

func (s *Server) listStoreItems(w http.ResponseWriter, r *http.Request) {
	namespaceFilter := strings.TrimSpace(r.URL.Query().Get("namespace"))

	if s.pg != nil {
		items, err := s.listStoreItemsFromPostgres(r.Context(), namespaceFilter)
		if err != nil {
			contracts.WriteError(w, http.StatusInternalServerError, "db_query_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
			return
		}
		writeJSON(w, http.StatusOK, items)
		return
	}

	s.store.mu.RLock()
	out := make([]storeRecord, 0, len(s.store.storeItems))
	for _, item := range s.store.storeItems {
		if namespaceFilter != "" && item.Namespace != namespaceFilter {
			continue
		}
		out = append(out, item)
	}
	s.store.mu.RUnlock()
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) putStoreItem(w http.ResponseWriter, r *http.Request) {
	type request struct {
		Namespace string         `json:"namespace"`
		Key       string         `json:"key"`
		Value     any            `json:"value"`
		Metadata  map[string]any `json:"metadata"`
	}
	var in request
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_json", err.Error(), observability.RequestIDFromContext(r.Context()))
		return
	}
	in.Namespace = strings.TrimSpace(in.Namespace)
	in.Key = strings.TrimSpace(in.Key)
	if in.Namespace == "" || in.Key == "" {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_store_item", "namespace and key are required", observability.RequestIDFromContext(r.Context()))
		return
	}
	if in.Metadata == nil {
		in.Metadata = map[string]any{}
	}

	if s.pg != nil {
		item, err := s.upsertStoreItemInPostgres(r.Context(), in.Namespace, in.Key, in.Value, in.Metadata)
		if err != nil {
			contracts.WriteError(w, http.StatusInternalServerError, "db_insert_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
			return
		}
		writeJSON(w, http.StatusCreated, item)
		return
	}

	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	recordKey := storeKey(in.Namespace, in.Key)
	item, exists := s.store.storeItems[recordKey]
	if !exists {
		item = storeRecord{
			ID:        contracts.NewID("store"),
			Namespace: in.Namespace,
			Key:       in.Key,
			CreatedAt: time.Now().UTC(),
		}
	}
	item.Value = in.Value
	item.Metadata = in.Metadata
	s.store.storeItems[recordKey] = item
	writeJSON(w, http.StatusCreated, item)
}

func (s *Server) getStoreItem(w http.ResponseWriter, r *http.Request) {
	namespace := strings.TrimSpace(chi.URLParam(r, "namespace"))
	key := strings.TrimSpace(chi.URLParam(r, "key"))
	if namespace == "" || key == "" {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_store_item", "namespace and key are required", observability.RequestIDFromContext(r.Context()))
		return
	}

	if s.pg != nil {
		item, err := s.getStoreItemFromPostgres(r.Context(), namespace, key)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				contracts.WriteError(w, http.StatusNotFound, "store_item_not_found", "store item not found", observability.RequestIDFromContext(r.Context()))
				return
			}
			contracts.WriteError(w, http.StatusInternalServerError, "db_query_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
			return
		}
		writeJSON(w, http.StatusOK, item)
		return
	}

	s.store.mu.RLock()
	item, ok := s.store.storeItems[storeKey(namespace, key)]
	s.store.mu.RUnlock()
	if !ok {
		contracts.WriteError(w, http.StatusNotFound, "store_item_not_found", "store item not found", observability.RequestIDFromContext(r.Context()))
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) deleteStoreItem(w http.ResponseWriter, r *http.Request) {
	namespace := strings.TrimSpace(chi.URLParam(r, "namespace"))
	key := strings.TrimSpace(chi.URLParam(r, "key"))
	if namespace == "" || key == "" {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_store_item", "namespace and key are required", observability.RequestIDFromContext(r.Context()))
		return
	}

	if s.pg != nil {
		if err := s.deleteStoreItemFromPostgres(r.Context(), namespace, key); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				contracts.WriteError(w, http.StatusNotFound, "store_item_not_found", "store item not found", observability.RequestIDFromContext(r.Context()))
				return
			}
			contracts.WriteError(w, http.StatusInternalServerError, "db_delete_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}

	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	recordKey := storeKey(namespace, key)
	if _, ok := s.store.storeItems[recordKey]; !ok {
		contracts.WriteError(w, http.StatusNotFound, "store_item_not_found", "store item not found", observability.RequestIDFromContext(r.Context()))
		return
	}
	delete(s.store.storeItems, recordKey)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) systemInfo(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"service":  "langopen-api-server",
		"version":  "0.1.0",
		"api_base": "/api/v1",
		"features": []string{"assistants", "threads", "thread_runs", "stateless_runs", "store", "crons", "a2a", "mcp", "system"},
	})
}

func (s *Server) systemHealth(w http.ResponseWriter, r *http.Request) {
	type componentHealth struct {
		Status string `json:"status"`
		Error  string `json:"error,omitempty"`
	}
	response := map[string]any{
		"status": "ok",
		"checks": map[string]componentHealth{
			"api": {Status: "ok"},
		},
	}

	checks := response["checks"].(map[string]componentHealth)
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	if s.pg == nil {
		checks["postgres"] = componentHealth{Status: "disabled"}
	} else if err := s.pg.Ping(ctx); err != nil {
		checks["postgres"] = componentHealth{Status: "error", Error: err.Error()}
		response["status"] = "degraded"
	} else {
		checks["postgres"] = componentHealth{Status: "ok"}
	}

	if s.redis == nil {
		checks["redis"] = componentHealth{Status: "disabled"}
	} else if err := s.redis.Ping(ctx).Err(); err != nil {
		checks["redis"] = componentHealth{Status: "error", Error: err.Error()}
		response["status"] = "degraded"
	} else {
		checks["redis"] = componentHealth{Status: "ok"}
	}

	if response["status"] == "degraded" {
		writeJSON(w, http.StatusServiceUnavailable, response)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) createStatelessRunInPostgres(ctx context.Context, assistantID string, strategy contracts.MultitaskStrategy, streamResumable bool, webhookURL string) (contracts.Run, error) {
	now := time.Now().UTC()
	run := contracts.Run{
		ID:                contracts.NewID("run"),
		ThreadID:          "",
		AssistantID:       assistantID,
		Status:            contracts.RunStatusPending,
		MultitaskStrategy: strategy,
		StreamResumable:   streamResumable,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	_, err := s.pg.Exec(ctx, `
		INSERT INTO runs (id, thread_id, assistant_id, status, multitask_strategy, stream_resumable, webhook_url, created_at, updated_at)
		VALUES ($1,NULL,$2,$3,$4,$5,$6,$7,$8)
	`, run.ID, run.AssistantID, run.Status, run.MultitaskStrategy, run.StreamResumable, webhookURL, run.CreatedAt, run.UpdatedAt)
	if err != nil {
		return contracts.Run{}, err
	}
	return run, nil
}

func (s *Server) listStoreItemsFromPostgres(ctx context.Context, namespaceFilter string) ([]storeRecord, error) {
	var (
		rows pgx.Rows
		err  error
	)
	if namespaceFilter == "" {
		rows, err = s.pg.Query(ctx, `SELECT id, namespace, key, value, metadata, created_at FROM store_items ORDER BY created_at DESC LIMIT 500`)
	} else {
		rows, err = s.pg.Query(ctx, `SELECT id, namespace, key, value, metadata, created_at FROM store_items WHERE namespace=$1 ORDER BY created_at DESC LIMIT 500`, namespaceFilter)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]storeRecord, 0)
	for rows.Next() {
		item, err := scanStoreRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Server) upsertStoreItemInPostgres(ctx context.Context, namespace, key string, value any, metadata map[string]any) (storeRecord, error) {
	valueRaw, err := json.Marshal(value)
	if err != nil {
		return storeRecord{}, err
	}
	metaRaw, err := json.Marshal(metadata)
	if err != nil {
		return storeRecord{}, err
	}

	rows, err := s.pg.Query(ctx, `
		INSERT INTO store_items (id, namespace, key, value, metadata, created_at)
		VALUES ($1,$2,$3,$4::jsonb,$5::jsonb,NOW())
		ON CONFLICT (namespace, key)
		DO UPDATE SET value = EXCLUDED.value, metadata = EXCLUDED.metadata
		RETURNING id, namespace, key, value, metadata, created_at
	`, contracts.NewID("store"), namespace, key, valueRaw, metaRaw)
	if err != nil {
		return storeRecord{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		return storeRecord{}, sql.ErrNoRows
	}
	return scanStoreRecord(rows)
}

func (s *Server) getStoreItemFromPostgres(ctx context.Context, namespace, key string) (storeRecord, error) {
	rows, err := s.pg.Query(ctx, `
		SELECT id, namespace, key, value, metadata, created_at
		FROM store_items WHERE namespace=$1 AND key=$2
		LIMIT 1
	`, namespace, key)
	if err != nil {
		return storeRecord{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		return storeRecord{}, sql.ErrNoRows
	}
	return scanStoreRecord(rows)
}

func (s *Server) deleteStoreItemFromPostgres(ctx context.Context, namespace, key string) error {
	tag, err := s.pg.Exec(ctx, `DELETE FROM store_items WHERE namespace=$1 AND key=$2`, namespace, key)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func scanStoreRecord(row pgx.Row) (storeRecord, error) {
	var item storeRecord
	var valueRaw []byte
	var metaRaw []byte
	if err := row.Scan(&item.ID, &item.Namespace, &item.Key, &valueRaw, &metaRaw, &item.CreatedAt); err != nil {
		return storeRecord{}, err
	}
	if len(valueRaw) > 0 {
		if err := json.Unmarshal(valueRaw, &item.Value); err != nil {
			return storeRecord{}, err
		}
	}
	if len(metaRaw) > 0 {
		if err := json.Unmarshal(metaRaw, &item.Metadata); err != nil {
			return storeRecord{}, err
		}
	}
	if item.Metadata == nil {
		item.Metadata = map[string]any{}
	}
	return item, nil
}

func storeKey(namespace, key string) string {
	return namespace + "\x00" + key
}

func (s *Server) listCrons(w http.ResponseWriter, r *http.Request) {
	if s.pg != nil {
		rows, err := s.pg.Query(r.Context(), `SELECT id, assistant_id, schedule, enabled FROM cron_jobs ORDER BY created_at DESC LIMIT 200`)
		if err != nil {
			contracts.WriteError(w, http.StatusInternalServerError, "db_query_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
			return
		}
		defer rows.Close()
		out := make([]contracts.CronJob, 0)
		for rows.Next() {
			var c contracts.CronJob
			if err := rows.Scan(&c.ID, &c.AssistantID, &c.Schedule, &c.Enabled); err != nil {
				contracts.WriteError(w, http.StatusInternalServerError, "db_scan_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
				return
			}
			out = append(out, c)
		}
		writeJSON(w, http.StatusOK, out)
		return
	}

	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	out := make([]contracts.CronJob, 0, len(s.store.crons))
	for _, c := range s.store.crons {
		out = append(out, c)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) createCron(w http.ResponseWriter, r *http.Request) {
	var in contracts.CronJob
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_json", err.Error(), observability.RequestIDFromContext(r.Context()))
		return
	}
	if in.AssistantID == "" {
		in.AssistantID = "asst_default"
	}
	in.ID = contracts.NewID("cron")
	in.Enabled = true

	if s.pg != nil {
		_, err := s.pg.Exec(r.Context(), `INSERT INTO cron_jobs (id, assistant_id, schedule, enabled, created_at, updated_at) VALUES ($1,$2,$3,$4,NOW(),NOW())`, in.ID, in.AssistantID, in.Schedule, in.Enabled)
		if err != nil {
			contracts.WriteError(w, http.StatusBadRequest, "db_insert_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
			return
		}
		writeJSON(w, http.StatusCreated, in)
		return
	}

	s.store.mu.Lock()
	s.store.crons[in.ID] = in
	s.store.mu.Unlock()
	writeJSON(w, http.StatusCreated, in)
}

func (s *Server) a2a(w http.ResponseWriter, r *http.Request) {
	type rpcRequest struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      string          `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}
	var req rpcRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_json", err.Error(), observability.RequestIDFromContext(r.Context()))
		return
	}
	allowed := map[string]bool{"message/send": true, "message/stream": true, "tasks/get": true, "tasks/cancel": true}
	if !allowed[req.Method] {
		writeJSON(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32601, "message": "method not found"}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"accepted": true, "method": req.Method}})
}

func (s *Server) mcp(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	writeJSON(w, http.StatusOK, map[string]any{
		"jsonrpc": "2.0",
		"result": map[string]any{
			"stateless":         true,
			"session_terminate": "no-op",
			"transport":         "streamable-http",
		},
	})
}

func mkEvent(id int64, event string, payload any) streamEvent {
	data, _ := json.Marshal(payload)
	return streamEvent{ID: id, Event: event, Data: data}
}

func streamSSE(w http.ResponseWriter, r *http.Request, events []streamEvent, startFrom int64) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	for _, ev := range events {
		if ev.ID < startFrom {
			continue
		}
		_, _ = fmt.Fprintf(w, "id: %d\n", ev.ID)
		_, _ = fmt.Fprintf(w, "event: %s\n", ev.Event)
		lines := strings.Split(string(ev.Data), "\n")
		for _, ln := range lines {
			_, _ = fmt.Fprintf(w, "data: %s\n", ln)
		}
		_, _ = fmt.Fprint(w, "\n")
		flusher.Flush()
		select {
		case <-r.Context().Done():
			return
		case <-time.After(120 * time.Millisecond):
		}
	}
}

func isValidMultitaskStrategy(strategy contracts.MultitaskStrategy) bool {
	switch strategy {
	case contracts.MultitaskReject, contracts.MultitaskRollback, contracts.MultitaskInterrupt, contracts.MultitaskEnqueue:
		return true
	default:
		return false
	}
}

func envOrDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func envIntOrDefault(key string, fallback int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return parsed
}

func envBoolOrDefault(key string, fallback bool) bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv(key)))
	if v == "" {
		return fallback
	}
	switch v {
	case "1", "t", "true", "yes", "y", "on":
		return true
	case "0", "f", "false", "no", "n", "off":
		return false
	default:
		return fallback
	}
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
