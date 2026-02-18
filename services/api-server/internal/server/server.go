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
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"langopen.dev/pkg/contracts"
	"langopen.dev/pkg/observability"
)

type contextKey string

const (
	projectIDContextKey   contextKey = "project_id"
	projectRoleContextKey contextKey = "project_role"
)

var errRunConflict = errors.New("run_conflict")

var errProjectScope = errors.New("project_scope_violation")

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
	r.Handle("/metrics", observability.MetricsHandler())
	r.Get("/docs", s.docs)
	r.Get("/openapi.json", s.openapi)

	r.Route("/api/v1", func(api chi.Router) {
		api.Get("/assistants", s.listAssistants)
		api.Post("/assistants", s.createAssistant)
		api.Get("/assistants/{assistant_id}", s.getAssistant)
		api.Patch("/assistants/{assistant_id}", s.updateAssistant)
		api.Delete("/assistants/{assistant_id}", s.deleteAssistant)
		api.Get("/threads", s.listThreads)
		api.Post("/threads", s.createThread)
		api.Get("/threads/{thread_id}", s.getThread)
		api.Patch("/threads/{thread_id}", s.updateThread)
		api.Delete("/threads/{thread_id}", s.deleteThread)
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
		api.Get("/crons/{cron_id}", s.getCron)
		api.Patch("/crons/{cron_id}", s.updateCron)
		api.Delete("/crons/{cron_id}", s.deleteCron)
		api.Get("/system", s.systemInfo)
		api.Get("/system/health", s.systemHealth)
		api.Get("/system/attention", s.systemAttention)
	})

	r.Post("/a2a/{assistant_id}", s.a2a)
	r.Post("/mcp", s.mcp)

	s.router = r
	return s, nil
}

func (s *Server) Router() http.Handler { return s.router }

func projectIDFromContext(ctx context.Context) string {
	projectID, _ := ctx.Value(projectIDContextKey).(string)
	return strings.TrimSpace(projectID)
}

func projectRoleFromContext(ctx context.Context) contracts.ProjectRole {
	role, _ := ctx.Value(projectRoleContextKey).(contracts.ProjectRole)
	return role
}

func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.URL.Path == "/metrics" || r.URL.Path == "/docs" || r.URL.Path == "/openapi.json" {
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
		role := contracts.ProjectRole(strings.TrimSpace(r.Header.Get(contracts.HeaderProjectRole)))
		if role == "" {
			role = contracts.RoleAdmin
		}
		if !contracts.IsValidProjectRole(role) {
			contracts.WriteError(w, http.StatusBadRequest, "invalid_role", "X-Project-Role must be one of viewer, developer, operator, admin", observability.RequestIDFromContext(r.Context()))
			return
		}
		required := requiredDataRoleFor(r.Method, r.URL.Path)
		if !roleAtLeast(role, required) {
			contracts.WriteError(w, http.StatusForbidden, "forbidden", "insufficient role for requested action", observability.RequestIDFromContext(r.Context()))
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
		r = r.WithContext(context.WithValue(r.Context(), projectRoleContextKey, role))

		next.ServeHTTP(w, r)
	})
}

func requiredDataRoleFor(method, path string) contracts.ProjectRole {
	if method == http.MethodGet || method == http.MethodHead {
		return contracts.RoleViewer
	}
	if strings.HasPrefix(path, "/api/v1/system") {
		return contracts.RoleViewer
	}
	if strings.HasPrefix(path, "/api/v1/store/items") ||
		strings.HasPrefix(path, "/api/v1/assistants") ||
		strings.HasPrefix(path, "/api/v1/threads") ||
		strings.HasPrefix(path, "/api/v1/runs") ||
		strings.HasPrefix(path, "/api/v1/crons") ||
		strings.HasPrefix(path, "/a2a/") ||
		strings.HasPrefix(path, "/mcp") {
		return contracts.RoleDeveloper
	}
	return contracts.RoleAdmin
}

func roleAtLeast(actual, required contracts.ProjectRole) bool {
	rank := map[contracts.ProjectRole]int{
		contracts.RoleViewer:    1,
		contracts.RoleDeveloper: 2,
		contracts.RoleOperator:  3,
		contracts.RoleAdmin:     4,
	}
	return rank[actual] >= rank[required]
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

func (s *Server) writeDataPlaneAudit(ctx context.Context, projectID, eventType string, payload map[string]any) {
	if s.pg == nil {
		return
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		s.logger.Warn("audit_payload_marshal_failed", "event_type", eventType, "error", err)
		return
	}
	var projectArg any
	if strings.TrimSpace(projectID) != "" {
		projectArg = strings.TrimSpace(projectID)
	}
	if _, err := s.pg.Exec(ctx, `
		INSERT INTO audit_logs (id, project_id, actor_id, event_type, payload, created_at)
		VALUES ($1, $2, $3, $4, $5, NOW())
	`, contracts.NewID("audit"), projectArg, "agent-server", eventType, raw); err != nil {
		s.logger.Warn("audit_write_failed", "event_type", eventType, "project_id", projectID, "error", err)
	}
}

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (s *Server) docs(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <title>LangOpen API Docs</title>
  <link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist@5/swagger-ui.css" />
  <style>
    body { margin: 0; background: #fafafa; }
    .fallback { font-family: sans-serif; margin: 1rem; }
  </style>
</head>
<body>
  <div id="swagger-ui"></div>
  <noscript>
    <div class="fallback">OpenAPI spec: <a href="/openapi.json">/openapi.json</a></div>
  </noscript>
  <script src="https://unpkg.com/swagger-ui-dist@5/swagger-ui-bundle.js"></script>
  <script>
    window.ui = SwaggerUIBundle({
      url: "/openapi.json",
      dom_id: "#swagger-ui",
      deepLinking: true
    });
  </script>
</body>
</html>`))
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
		projectID := projectIDFromContext(r.Context())
		var (
			rows pgx.Rows
			err  error
		)
		if projectID == "" {
			rows, err = s.pg.Query(r.Context(), `SELECT id, deployment_id, graph_id, config, version FROM assistants ORDER BY created_at DESC LIMIT 200`)
		} else {
			rows, err = s.pg.Query(r.Context(), `
				SELECT a.id, a.deployment_id, a.graph_id, a.config, a.version
				FROM assistants a
				JOIN deployments d ON d.id = a.deployment_id
				WHERE d.project_id=$1
				ORDER BY a.created_at DESC
				LIMIT 200
			`, projectID)
		}
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
		projectID := projectIDFromContext(r.Context())
		if projectID != "" {
			var deploymentAllowed bool
			err := s.pg.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM deployments WHERE id=$1 AND project_id=$2)`, in.DeploymentID, projectID).Scan(&deploymentAllowed)
			if err != nil {
				contracts.WriteError(w, http.StatusInternalServerError, "db_query_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
				return
			}
			if !deploymentAllowed {
				contracts.WriteError(w, http.StatusForbidden, "project_scope_violation", "deployment is not accessible for this API key project", observability.RequestIDFromContext(r.Context()))
				return
			}
		}
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

func (s *Server) getAssistant(w http.ResponseWriter, r *http.Request) {
	assistantID := strings.TrimSpace(chi.URLParam(r, "assistant_id"))
	if assistantID == "" {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_assistant_id", "assistant_id is required", observability.RequestIDFromContext(r.Context()))
		return
	}
	if s.pg != nil {
		assistant, err := s.getAssistantFromPostgres(r.Context(), projectIDFromContext(r.Context()), assistantID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				contracts.WriteError(w, http.StatusNotFound, "assistant_not_found", "assistant not found", observability.RequestIDFromContext(r.Context()))
				return
			}
			contracts.WriteError(w, http.StatusInternalServerError, "db_query_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
			return
		}
		writeJSON(w, http.StatusOK, assistant)
		return
	}

	s.store.mu.RLock()
	assistant, ok := s.store.assistants[assistantID]
	s.store.mu.RUnlock()
	if !ok {
		contracts.WriteError(w, http.StatusNotFound, "assistant_not_found", "assistant not found", observability.RequestIDFromContext(r.Context()))
		return
	}
	writeJSON(w, http.StatusOK, assistant)
}

func (s *Server) updateAssistant(w http.ResponseWriter, r *http.Request) {
	assistantID := strings.TrimSpace(chi.URLParam(r, "assistant_id"))
	if assistantID == "" {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_assistant_id", "assistant_id is required", observability.RequestIDFromContext(r.Context()))
		return
	}
	var req struct {
		GraphID *string            `json:"graph_id"`
		Config  *map[string]string `json:"config"`
		Version *string            `json:"version"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_json", err.Error(), observability.RequestIDFromContext(r.Context()))
		return
	}
	if req.GraphID == nil && req.Config == nil && req.Version == nil {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_assistant_update", "at least one of graph_id, config, version is required", observability.RequestIDFromContext(r.Context()))
		return
	}

	var (
		graphID *string
		version *string
		config  map[string]string
	)
	if req.GraphID != nil {
		v := strings.TrimSpace(*req.GraphID)
		graphID = &v
	}
	if req.Version != nil {
		v := strings.TrimSpace(*req.Version)
		version = &v
	}
	if req.Config != nil {
		config = *req.Config
	}

	if s.pg != nil {
		assistant, err := s.updateAssistantInPostgres(r.Context(), projectIDFromContext(r.Context()), assistantID, graphID, version, req.Config != nil, config)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				contracts.WriteError(w, http.StatusNotFound, "assistant_not_found", "assistant not found", observability.RequestIDFromContext(r.Context()))
				return
			}
			contracts.WriteError(w, http.StatusBadRequest, "db_update_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
			return
		}
		writeJSON(w, http.StatusOK, assistant)
		return
	}

	s.store.mu.Lock()
	assistant, ok := s.store.assistants[assistantID]
	if !ok {
		s.store.mu.Unlock()
		contracts.WriteError(w, http.StatusNotFound, "assistant_not_found", "assistant not found", observability.RequestIDFromContext(r.Context()))
		return
	}
	if graphID != nil && *graphID != "" {
		assistant.GraphID = *graphID
	}
	if version != nil && *version != "" {
		assistant.Version = *version
	}
	if req.Config != nil {
		assistant.Config = map[string]string{}
		for k, v := range config {
			assistant.Config[k] = v
		}
	}
	s.store.assistants[assistantID] = assistant
	s.store.mu.Unlock()
	writeJSON(w, http.StatusOK, assistant)
}

func (s *Server) deleteAssistant(w http.ResponseWriter, r *http.Request) {
	assistantID := strings.TrimSpace(chi.URLParam(r, "assistant_id"))
	if assistantID == "" {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_assistant_id", "assistant_id is required", observability.RequestIDFromContext(r.Context()))
		return
	}
	if s.pg != nil {
		if err := s.deleteAssistantFromPostgres(r.Context(), projectIDFromContext(r.Context()), assistantID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				contracts.WriteError(w, http.StatusNotFound, "assistant_not_found", "assistant not found", observability.RequestIDFromContext(r.Context()))
				return
			}
			contracts.WriteError(w, http.StatusInternalServerError, "db_delete_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}

	s.store.mu.Lock()
	if _, ok := s.store.assistants[assistantID]; !ok {
		s.store.mu.Unlock()
		contracts.WriteError(w, http.StatusNotFound, "assistant_not_found", "assistant not found", observability.RequestIDFromContext(r.Context()))
		return
	}
	delete(s.store.assistants, assistantID)
	for threadID, thread := range s.store.threads {
		if thread.AssistantID == assistantID {
			delete(s.store.threads, threadID)
		}
	}
	for runID, run := range s.store.runs {
		if run.AssistantID == assistantID {
			delete(s.store.runs, runID)
			delete(s.store.events, runID)
		}
	}
	for cronID, cron := range s.store.crons {
		if cron.AssistantID == assistantID {
			delete(s.store.crons, cronID)
		}
	}
	s.store.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) listThreads(w http.ResponseWriter, r *http.Request) {
	if s.pg != nil {
		projectID := projectIDFromContext(r.Context())
		var (
			rows pgx.Rows
			err  error
		)
		if projectID == "" {
			rows, err = s.pg.Query(r.Context(), `SELECT id, assistant_id, metadata, updated_at FROM threads ORDER BY updated_at DESC LIMIT 200`)
		} else {
			rows, err = s.pg.Query(r.Context(), `
				SELECT t.id, t.assistant_id, t.metadata, t.updated_at
				FROM threads t
				JOIN assistants a ON a.id = t.assistant_id
				JOIN deployments d ON d.id = a.deployment_id
				WHERE d.project_id=$1
				ORDER BY t.updated_at DESC
				LIMIT 200
			`, projectID)
		}
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
		projectID := projectIDFromContext(r.Context())
		if projectID != "" {
			var assistantAllowed bool
			err := s.pg.QueryRow(r.Context(), `
				SELECT EXISTS(
					SELECT 1
					FROM assistants a
					JOIN deployments d ON d.id = a.deployment_id
					WHERE a.id=$1 AND d.project_id=$2
				)
			`, in.AssistantID, projectID).Scan(&assistantAllowed)
			if err != nil {
				contracts.WriteError(w, http.StatusInternalServerError, "db_query_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
				return
			}
			if !assistantAllowed {
				contracts.WriteError(w, http.StatusForbidden, "project_scope_violation", "assistant is not accessible for this API key project", observability.RequestIDFromContext(r.Context()))
				return
			}
		}
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

func (s *Server) getThread(w http.ResponseWriter, r *http.Request) {
	threadID := strings.TrimSpace(chi.URLParam(r, "thread_id"))
	if threadID == "" {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_thread_id", "thread_id is required", observability.RequestIDFromContext(r.Context()))
		return
	}
	if s.pg != nil {
		thread, err := s.getThreadFromPostgres(r.Context(), projectIDFromContext(r.Context()), threadID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				contracts.WriteError(w, http.StatusNotFound, "thread_not_found", "thread not found", observability.RequestIDFromContext(r.Context()))
				return
			}
			contracts.WriteError(w, http.StatusInternalServerError, "db_query_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
			return
		}
		writeJSON(w, http.StatusOK, thread)
		return
	}

	s.store.mu.RLock()
	thread, ok := s.store.threads[threadID]
	s.store.mu.RUnlock()
	if !ok {
		contracts.WriteError(w, http.StatusNotFound, "thread_not_found", "thread not found", observability.RequestIDFromContext(r.Context()))
		return
	}
	writeJSON(w, http.StatusOK, thread)
}

func (s *Server) updateThread(w http.ResponseWriter, r *http.Request) {
	threadID := strings.TrimSpace(chi.URLParam(r, "thread_id"))
	if threadID == "" {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_thread_id", "thread_id is required", observability.RequestIDFromContext(r.Context()))
		return
	}
	var req struct {
		Metadata *map[string]string `json:"metadata"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_json", err.Error(), observability.RequestIDFromContext(r.Context()))
		return
	}
	if req.Metadata == nil {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_thread_update", "metadata is required", observability.RequestIDFromContext(r.Context()))
		return
	}
	metadata := *req.Metadata

	if s.pg != nil {
		thread, err := s.updateThreadInPostgres(r.Context(), projectIDFromContext(r.Context()), threadID, metadata)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				contracts.WriteError(w, http.StatusNotFound, "thread_not_found", "thread not found", observability.RequestIDFromContext(r.Context()))
				return
			}
			contracts.WriteError(w, http.StatusInternalServerError, "db_update_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
			return
		}
		writeJSON(w, http.StatusOK, thread)
		return
	}

	s.store.mu.Lock()
	thread, ok := s.store.threads[threadID]
	if !ok {
		s.store.mu.Unlock()
		contracts.WriteError(w, http.StatusNotFound, "thread_not_found", "thread not found", observability.RequestIDFromContext(r.Context()))
		return
	}
	thread.Metadata = map[string]string{}
	for k, v := range metadata {
		thread.Metadata[k] = v
	}
	thread.UpdatedAt = time.Now().UTC()
	s.store.threads[threadID] = thread
	s.store.mu.Unlock()
	writeJSON(w, http.StatusOK, thread)
}

func (s *Server) deleteThread(w http.ResponseWriter, r *http.Request) {
	threadID := strings.TrimSpace(chi.URLParam(r, "thread_id"))
	if threadID == "" {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_thread_id", "thread_id is required", observability.RequestIDFromContext(r.Context()))
		return
	}
	if s.pg != nil {
		if err := s.deleteThreadFromPostgres(r.Context(), projectIDFromContext(r.Context()), threadID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				contracts.WriteError(w, http.StatusNotFound, "thread_not_found", "thread not found", observability.RequestIDFromContext(r.Context()))
				return
			}
			contracts.WriteError(w, http.StatusInternalServerError, "db_delete_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}

	s.store.mu.Lock()
	if _, ok := s.store.threads[threadID]; !ok {
		s.store.mu.Unlock()
		contracts.WriteError(w, http.StatusNotFound, "thread_not_found", "thread not found", observability.RequestIDFromContext(r.Context()))
		return
	}
	delete(s.store.threads, threadID)
	for runID, run := range s.store.runs {
		if run.ThreadID == threadID {
			delete(s.store.runs, runID)
			delete(s.store.events, runID)
		}
	}
	s.store.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
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
		run, err = s.createRunInPostgres(r.Context(), projectIDFromContext(r.Context()), threadID, req.AssistantID, strategy, streamResumable, strings.TrimSpace(req.WebhookURL))
		if err != nil {
			if errors.Is(err, errRunConflict) {
				contracts.WriteError(w, http.StatusConflict, "run_conflict", "thread already has active run", observability.RequestIDFromContext(r.Context()))
				return
			}
			if errors.Is(err, errProjectScope) {
				contracts.WriteError(w, http.StatusForbidden, "project_scope_violation", "thread or assistant is not accessible for this API key project", observability.RequestIDFromContext(r.Context()))
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
		run, err = s.createStatelessRunInPostgres(r.Context(), projectIDFromContext(r.Context()), req.AssistantID, strategy, streamResumable, strings.TrimSpace(req.WebhookURL))
		if err != nil {
			if errors.Is(err, errProjectScope) {
				contracts.WriteError(w, http.StatusForbidden, "project_scope_violation", "assistant is not accessible for this API key project", observability.RequestIDFromContext(r.Context()))
				return
			}
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

func (s *Server) createRunInPostgres(ctx context.Context, projectID, threadID, assistantID string, strategy contracts.MultitaskStrategy, streamResumable bool, webhookURL string) (contracts.Run, error) {
	tx, err := s.pg.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return contracts.Run{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if projectID != "" {
		var assistantAllowed bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS(
				SELECT 1
				FROM assistants a
				JOIN deployments d ON d.id = a.deployment_id
				WHERE a.id=$1 AND d.project_id=$2
			)
		`, assistantID, projectID).Scan(&assistantAllowed); err != nil {
			return contracts.Run{}, err
		}
		if !assistantAllowed {
			return contracts.Run{}, errProjectScope
		}
	}

	var threadExists bool
	threadScopeQuery := `SELECT EXISTS(SELECT 1 FROM threads WHERE id=$1)`
	threadScopeArgs := []any{threadID}
	if projectID != "" {
		threadScopeQuery = `
			SELECT EXISTS(
				SELECT 1
				FROM threads t
				JOIN assistants a ON a.id = t.assistant_id
				JOIN deployments d ON d.id = a.deployment_id
				WHERE t.id=$1 AND d.project_id=$2
			)
		`
		threadScopeArgs = append(threadScopeArgs, projectID)
	}
	if err := tx.QueryRow(ctx, threadScopeQuery, threadScopeArgs...).Scan(&threadExists); err != nil {
		return contracts.Run{}, err
	}
	if !threadExists {
		if projectID != "" {
			var threadExistsGlobal bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM threads WHERE id=$1)`, threadID).Scan(&threadExistsGlobal); err != nil {
				return contracts.Run{}, err
			}
			if threadExistsGlobal {
				return contracts.Run{}, errProjectScope
			}
		}
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
		projectID := projectIDFromContext(r.Context())
		if err := s.cancelRunInPostgres(r.Context(), projectID, runID, action); err != nil {
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
		s.writeDataPlaneAudit(r.Context(), projectID, "run.cancelled", map[string]any{
			"run_id": runID,
			"action": action,
		})
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

func (s *Server) cancelRunInPostgres(ctx context.Context, projectID, runID, action string) error {
	tx, err := s.pg.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var exists bool
	scopeQuery := `SELECT EXISTS(SELECT 1 FROM runs WHERE id=$1)`
	scopeArgs := []any{runID}
	if projectID != "" {
		scopeQuery = `
			SELECT EXISTS(
				SELECT 1
				FROM runs r
				JOIN assistants a ON a.id = r.assistant_id
				JOIN deployments d ON d.id = a.deployment_id
				WHERE r.id=$1 AND d.project_id=$2
			)
		`
		scopeArgs = append(scopeArgs, projectID)
	}
	if err := tx.QueryRow(ctx, scopeQuery, scopeArgs...).Scan(&exists); err != nil {
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
		run, err := s.getRunFromPostgres(r.Context(), projectIDFromContext(r.Context()), runID)
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

func (s *Server) getRunFromPostgres(ctx context.Context, projectID, runID string) (contracts.Run, error) {
	var run contracts.Run
	var status string
	var strategy string
	query := `
		SELECT r.id, COALESCE(r.thread_id,''), r.assistant_id, r.status, r.multitask_strategy, r.stream_resumable, r.created_at, r.updated_at
		FROM runs r
	`
	args := []any{runID}
	if projectID == "" {
		query += ` WHERE r.id=$1`
	} else {
		query += `
			JOIN assistants a ON a.id = r.assistant_id
			JOIN deployments d ON d.id = a.deployment_id
			WHERE r.id=$1 AND d.project_id=$2
		`
		args = append(args, projectID)
	}
	err := s.pg.QueryRow(ctx, query, args...).Scan(&run.ID, &run.ThreadID, &run.AssistantID, &status, &strategy, &run.StreamResumable, &run.CreatedAt, &run.UpdatedAt)
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

func (s *Server) getAssistantFromPostgres(ctx context.Context, projectID, assistantID string) (contracts.Assistant, error) {
	var (
		assistant contracts.Assistant
		raw       []byte
	)
	query := `SELECT a.id, a.deployment_id, a.graph_id, a.config, a.version FROM assistants a`
	args := []any{assistantID}
	if projectID == "" {
		query += ` WHERE a.id=$1`
	} else {
		query += `
			JOIN deployments d ON d.id = a.deployment_id
			WHERE a.id=$1 AND d.project_id=$2
		`
		args = append(args, projectID)
	}
	err := s.pg.QueryRow(ctx, query, args...).Scan(&assistant.ID, &assistant.DeploymentID, &assistant.GraphID, &raw, &assistant.Version)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return contracts.Assistant{}, sql.ErrNoRows
		}
		return contracts.Assistant{}, err
	}
	assistant.Config = map[string]string{}
	_ = json.Unmarshal(raw, &assistant.Config)
	return assistant, nil
}

func (s *Server) updateAssistantInPostgres(ctx context.Context, projectID, assistantID string, graphID, version *string, configSet bool, config map[string]string) (contracts.Assistant, error) {
	var (
		configRaw []byte
		err       error
	)
	if configSet {
		configRaw, err = json.Marshal(config)
		if err != nil {
			return contracts.Assistant{}, err
		}
	}

	query := `
		UPDATE assistants a
		SET graph_id = COALESCE(NULLIF($2,''), a.graph_id),
		    version = COALESCE(NULLIF($3,''), a.version),
		    config = CASE WHEN $4::boolean THEN $5::jsonb ELSE a.config END
	`
	args := []any{
		assistantID,
		nilString(graphID),
		nilString(version),
		configSet,
		configRaw,
	}
	if projectID == "" {
		query += ` WHERE a.id=$1`
	} else {
		query += `
			FROM deployments d
			WHERE a.id=$1 AND d.id = a.deployment_id AND d.project_id=$6
		`
		args = append(args, projectID)
	}
	if _, err := s.pg.Exec(ctx, query, args...); err != nil {
		return contracts.Assistant{}, err
	}
	return s.getAssistantFromPostgres(ctx, projectID, assistantID)
}

func (s *Server) deleteAssistantFromPostgres(ctx context.Context, projectID, assistantID string) error {
	tx, err := s.pg.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var exists bool
	checkQuery := `SELECT EXISTS(SELECT 1 FROM assistants WHERE id=$1)`
	checkArgs := []any{assistantID}
	if projectID != "" {
		checkQuery = `
			SELECT EXISTS(
				SELECT 1
				FROM assistants a
				JOIN deployments d ON d.id = a.deployment_id
				WHERE a.id=$1 AND d.project_id=$2
			)
		`
		checkArgs = append(checkArgs, projectID)
	}
	if err := tx.QueryRow(ctx, checkQuery, checkArgs...).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return sql.ErrNoRows
	}

	_, _ = tx.Exec(ctx, `DELETE FROM webhook_deliveries WHERE run_id IN (SELECT id FROM runs WHERE assistant_id=$1)`, assistantID)
	_, _ = tx.Exec(ctx, `DELETE FROM checkpoints WHERE run_id IN (SELECT id FROM runs WHERE assistant_id=$1)`, assistantID)
	_, _ = tx.Exec(ctx, `DELETE FROM checkpoints WHERE thread_id IN (SELECT id FROM threads WHERE assistant_id=$1)`, assistantID)
	_, _ = tx.Exec(ctx, `DELETE FROM cron_executions WHERE cron_job_id IN (SELECT id FROM cron_jobs WHERE assistant_id=$1)`, assistantID)
	_, _ = tx.Exec(ctx, `DELETE FROM cron_jobs WHERE assistant_id=$1`, assistantID)
	_, _ = tx.Exec(ctx, `DELETE FROM runs WHERE assistant_id=$1`, assistantID)
	_, _ = tx.Exec(ctx, `DELETE FROM threads WHERE assistant_id=$1`, assistantID)
	_, err = tx.Exec(ctx, `DELETE FROM assistants WHERE id=$1`, assistantID)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Server) getThreadFromPostgres(ctx context.Context, projectID, threadID string) (contracts.Thread, error) {
	var (
		thread contracts.Thread
		raw    []byte
	)
	query := `SELECT t.id, t.assistant_id, t.metadata, t.updated_at FROM threads t`
	args := []any{threadID}
	if projectID == "" {
		query += ` WHERE t.id=$1`
	} else {
		query += `
			JOIN assistants a ON a.id = t.assistant_id
			JOIN deployments d ON d.id = a.deployment_id
			WHERE t.id=$1 AND d.project_id=$2
		`
		args = append(args, projectID)
	}
	err := s.pg.QueryRow(ctx, query, args...).Scan(&thread.ID, &thread.AssistantID, &raw, &thread.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return contracts.Thread{}, sql.ErrNoRows
		}
		return contracts.Thread{}, err
	}
	thread.Metadata = map[string]string{}
	_ = json.Unmarshal(raw, &thread.Metadata)
	return thread, nil
}

func (s *Server) deleteThreadFromPostgres(ctx context.Context, projectID, threadID string) error {
	tx, err := s.pg.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var exists bool
	checkQuery := `SELECT EXISTS(SELECT 1 FROM threads WHERE id=$1)`
	checkArgs := []any{threadID}
	if projectID != "" {
		checkQuery = `
			SELECT EXISTS(
				SELECT 1
				FROM threads t
				JOIN assistants a ON a.id = t.assistant_id
				JOIN deployments d ON d.id = a.deployment_id
				WHERE t.id=$1 AND d.project_id=$2
			)
		`
		checkArgs = append(checkArgs, projectID)
	}
	if err := tx.QueryRow(ctx, checkQuery, checkArgs...).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return sql.ErrNoRows
	}

	_, _ = tx.Exec(ctx, `DELETE FROM webhook_deliveries WHERE run_id IN (SELECT id FROM runs WHERE thread_id=$1)`, threadID)
	_, _ = tx.Exec(ctx, `DELETE FROM checkpoints WHERE run_id IN (SELECT id FROM runs WHERE thread_id=$1)`, threadID)
	_, _ = tx.Exec(ctx, `DELETE FROM checkpoints WHERE thread_id=$1`, threadID)
	_, _ = tx.Exec(ctx, `DELETE FROM runs WHERE thread_id=$1`, threadID)
	_, err = tx.Exec(ctx, `DELETE FROM threads WHERE id=$1`, threadID)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Server) updateThreadInPostgres(ctx context.Context, projectID, threadID string, metadata map[string]string) (contracts.Thread, error) {
	raw, err := json.Marshal(metadata)
	if err != nil {
		return contracts.Thread{}, err
	}

	var tag pgconn.CommandTag
	if projectID == "" {
		tag, err = s.pg.Exec(ctx, `UPDATE threads SET metadata=$2, updated_at=NOW() WHERE id=$1`, threadID, raw)
	} else {
		tag, err = s.pg.Exec(ctx, `
			UPDATE threads t
			SET metadata=$2, updated_at=NOW()
			FROM assistants a
			JOIN deployments d ON d.id = a.deployment_id
			WHERE t.id=$1 AND a.id = t.assistant_id AND d.project_id=$3
		`, threadID, raw, projectID)
	}
	if err != nil {
		return contracts.Thread{}, err
	}
	if tag.RowsAffected() == 0 {
		return contracts.Thread{}, sql.ErrNoRows
	}
	return s.getThreadFromPostgres(ctx, projectID, threadID)
}

func nilString(input *string) any {
	if input == nil {
		return nil
	}
	return *input
}

func (s *Server) listStoreItems(w http.ResponseWriter, r *http.Request) {
	namespaceFilter := strings.TrimSpace(r.URL.Query().Get("namespace"))
	projectID := projectIDFromContext(r.Context())

	if s.pg != nil {
		items, err := s.listStoreItemsFromPostgres(r.Context(), projectID, namespaceFilter)
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
		item, err := s.upsertStoreItemInPostgres(r.Context(), scopedNamespace(projectIDFromContext(r.Context()), in.Namespace), in.Key, in.Value, in.Metadata)
		if err != nil {
			contracts.WriteError(w, http.StatusInternalServerError, "db_insert_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
			return
		}
		item.Namespace = in.Namespace
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
		item, err := s.getStoreItemFromPostgres(r.Context(), scopedNamespace(projectIDFromContext(r.Context()), namespace), key)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				contracts.WriteError(w, http.StatusNotFound, "store_item_not_found", "store item not found", observability.RequestIDFromContext(r.Context()))
				return
			}
			contracts.WriteError(w, http.StatusInternalServerError, "db_query_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
			return
		}
		item.Namespace = namespace
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
		if err := s.deleteStoreItemFromPostgres(r.Context(), scopedNamespace(projectIDFromContext(r.Context()), namespace), key); err != nil {
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

func (s *Server) systemAttention(w http.ResponseWriter, r *http.Request) {
	now := time.Now().UTC()
	stuckThresholdSeconds := int64(envIntOrDefault("STUCK_RUN_SECONDS", 300))
	response := map[string]any{
		"stuck_runs":              int64(0),
		"pending_runs":            int64(0),
		"error_runs":              int64(0),
		"webhook_dead_letters":    int64(0),
		"stuck_threshold_seconds": stuckThresholdSeconds,
		"timestamp":               now,
	}

	if s.pg != nil {
		projectID := projectIDFromContext(r.Context())
		var (
			stuckRuns   int64
			pendingRuns int64
			errorRuns   int64
			deadLetters int64
		)
		if projectID == "" {
			_ = s.pg.QueryRow(r.Context(), `
				SELECT COUNT(1)
				FROM runs
				WHERE status='running' AND updated_at < NOW() - ($1::bigint * INTERVAL '1 second')
			`, stuckThresholdSeconds).Scan(&stuckRuns)
			_ = s.pg.QueryRow(r.Context(), `SELECT COUNT(1) FROM runs WHERE status='pending'`).Scan(&pendingRuns)
			_ = s.pg.QueryRow(r.Context(), `SELECT COUNT(1) FROM runs WHERE status='error'`).Scan(&errorRuns)
			_ = s.pg.QueryRow(r.Context(), `
				SELECT COUNT(1)
				FROM webhook_deliveries
				WHERE status IN ('dead_letter', 'failed')
			`).Scan(&deadLetters)
		} else {
			_ = s.pg.QueryRow(r.Context(), `
				SELECT COUNT(1)
				FROM runs r
				JOIN assistants a ON a.id = r.assistant_id
				JOIN deployments d ON d.id = a.deployment_id
				WHERE d.project_id=$1 AND r.status='running' AND r.updated_at < NOW() - ($2::bigint * INTERVAL '1 second')
			`, projectID, stuckThresholdSeconds).Scan(&stuckRuns)
			_ = s.pg.QueryRow(r.Context(), `
				SELECT COUNT(1)
				FROM runs r
				JOIN assistants a ON a.id = r.assistant_id
				JOIN deployments d ON d.id = a.deployment_id
				WHERE d.project_id=$1 AND r.status='pending'
			`, projectID).Scan(&pendingRuns)
			_ = s.pg.QueryRow(r.Context(), `
				SELECT COUNT(1)
				FROM runs r
				JOIN assistants a ON a.id = r.assistant_id
				JOIN deployments d ON d.id = a.deployment_id
				WHERE d.project_id=$1 AND r.status='error'
			`, projectID).Scan(&errorRuns)
			_ = s.pg.QueryRow(r.Context(), `
				SELECT COUNT(1)
				FROM webhook_deliveries wd
				JOIN runs r ON r.id = wd.run_id
				JOIN assistants a ON a.id = r.assistant_id
				JOIN deployments d ON d.id = a.deployment_id
				WHERE d.project_id=$1 AND wd.status IN ('dead_letter', 'failed')
			`, projectID).Scan(&deadLetters)
		}
		response["stuck_runs"] = stuckRuns
		response["pending_runs"] = pendingRuns
		response["error_runs"] = errorRuns
		response["webhook_dead_letters"] = deadLetters

		writeJSON(w, http.StatusOK, response)
		return
	}

	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	var stuckRuns int64
	var pendingRuns int64
	var errorRuns int64
	for _, run := range s.store.runs {
		switch run.Status {
		case contracts.RunStatusPending:
			pendingRuns++
		case contracts.RunStatusError:
			errorRuns++
		case contracts.RunStatusRunning:
			if now.Sub(run.UpdatedAt) > time.Duration(stuckThresholdSeconds)*time.Second {
				stuckRuns++
			}
		}
	}
	response["stuck_runs"] = stuckRuns
	response["pending_runs"] = pendingRuns
	response["error_runs"] = errorRuns
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) createStatelessRunInPostgres(ctx context.Context, projectID, assistantID string, strategy contracts.MultitaskStrategy, streamResumable bool, webhookURL string) (contracts.Run, error) {
	if projectID != "" {
		var assistantAllowed bool
		err := s.pg.QueryRow(ctx, `
			SELECT EXISTS(
				SELECT 1
				FROM assistants a
				JOIN deployments d ON d.id = a.deployment_id
				WHERE a.id=$1 AND d.project_id=$2
			)
		`, assistantID, projectID).Scan(&assistantAllowed)
		if err != nil {
			return contracts.Run{}, err
		}
		if !assistantAllowed {
			return contracts.Run{}, errProjectScope
		}
	}

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

func (s *Server) listStoreItemsFromPostgres(ctx context.Context, projectID, namespaceFilter string) ([]storeRecord, error) {
	var (
		rows pgx.Rows
		err  error
	)
	if projectID == "" {
		if namespaceFilter == "" {
			rows, err = s.pg.Query(ctx, `SELECT id, namespace, key, value, metadata, created_at FROM store_items ORDER BY created_at DESC LIMIT 500`)
		} else {
			rows, err = s.pg.Query(ctx, `SELECT id, namespace, key, value, metadata, created_at FROM store_items WHERE namespace=$1 ORDER BY created_at DESC LIMIT 500`, namespaceFilter)
		}
	} else {
		if namespaceFilter == "" {
			rows, err = s.pg.Query(ctx, `
				SELECT id, namespace, key, value, metadata, created_at
				FROM store_items
				WHERE namespace LIKE $1
				ORDER BY created_at DESC
				LIMIT 500
			`, scopedNamespace(projectID, "%"))
		} else {
			rows, err = s.pg.Query(ctx, `
				SELECT id, namespace, key, value, metadata, created_at
				FROM store_items
				WHERE namespace=$1
				ORDER BY created_at DESC
				LIMIT 500
			`, scopedNamespace(projectID, namespaceFilter))
		}
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
		if projectID != "" {
			item.Namespace = unscopedNamespace(projectID, item.Namespace)
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

func scopedNamespace(projectID, namespace string) string {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return namespace
	}
	return projectID + ":" + namespace
}

func unscopedNamespace(projectID, namespace string) string {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return namespace
	}
	prefix := projectID + ":"
	if strings.HasPrefix(namespace, prefix) {
		return strings.TrimPrefix(namespace, prefix)
	}
	return namespace
}

func (s *Server) getCronFromPostgres(ctx context.Context, cronID string) (contracts.CronJob, error) {
	var cronJob contracts.CronJob
	err := s.pg.QueryRow(ctx, `SELECT id, assistant_id, schedule, enabled FROM cron_jobs WHERE id=$1`, cronID).
		Scan(&cronJob.ID, &cronJob.AssistantID, &cronJob.Schedule, &cronJob.Enabled)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return contracts.CronJob{}, sql.ErrNoRows
		}
		return contracts.CronJob{}, err
	}
	return cronJob, nil
}

func (s *Server) updateCronInPostgres(ctx context.Context, cronID string, schedule any, enabled any) (contracts.CronJob, error) {
	var cronJob contracts.CronJob
	err := s.pg.QueryRow(ctx, `
		UPDATE cron_jobs
		SET schedule=COALESCE($2, schedule), enabled=COALESCE($3, enabled), updated_at=NOW()
		WHERE id=$1
		RETURNING id, assistant_id, schedule, enabled
	`, cronID, schedule, enabled).Scan(&cronJob.ID, &cronJob.AssistantID, &cronJob.Schedule, &cronJob.Enabled)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return contracts.CronJob{}, sql.ErrNoRows
		}
		return contracts.CronJob{}, err
	}
	return cronJob, nil
}

func (s *Server) deleteCronFromPostgres(ctx context.Context, cronID string) error {
	tag, err := s.pg.Exec(ctx, `DELETE FROM cron_jobs WHERE id=$1`, cronID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Server) listCrons(w http.ResponseWriter, r *http.Request) {
	if s.pg != nil {
		projectID := projectIDFromContext(r.Context())
		var (
			rows pgx.Rows
			err  error
		)
		if projectID == "" {
			rows, err = s.pg.Query(r.Context(), `SELECT id, assistant_id, schedule, enabled FROM cron_jobs ORDER BY created_at DESC LIMIT 200`)
		} else {
			rows, err = s.pg.Query(r.Context(), `
				SELECT c.id, c.assistant_id, c.schedule, c.enabled
				FROM cron_jobs c
				JOIN assistants a ON a.id = c.assistant_id
				JOIN deployments d ON d.id = a.deployment_id
				WHERE d.project_id=$1
				ORDER BY c.created_at DESC
				LIMIT 200
			`, projectID)
		}
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
	var req struct {
		AssistantID string `json:"assistant_id"`
		Schedule    string `json:"schedule"`
		Enabled     *bool  `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_json", err.Error(), observability.RequestIDFromContext(r.Context()))
		return
	}
	schedule := strings.TrimSpace(req.Schedule)
	if schedule == "" {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_cron", "schedule is required", observability.RequestIDFromContext(r.Context()))
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	assistantID := strings.TrimSpace(req.AssistantID)
	if assistantID == "" {
		assistantID = "asst_default"
	}
	in := contracts.CronJob{
		ID:          contracts.NewID("cron"),
		AssistantID: assistantID,
		Schedule:    schedule,
		Enabled:     enabled,
	}

	if s.pg != nil {
		projectID := projectIDFromContext(r.Context())
		if projectID != "" {
			var assistantAllowed bool
			err := s.pg.QueryRow(r.Context(), `
				SELECT EXISTS(
					SELECT 1
					FROM assistants a
					JOIN deployments d ON d.id = a.deployment_id
					WHERE a.id=$1 AND d.project_id=$2
				)
			`, in.AssistantID, projectID).Scan(&assistantAllowed)
			if err != nil {
				contracts.WriteError(w, http.StatusInternalServerError, "db_query_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
				return
			}
			if !assistantAllowed {
				contracts.WriteError(w, http.StatusForbidden, "project_scope_violation", "assistant is not accessible for this API key project", observability.RequestIDFromContext(r.Context()))
				return
			}
		}
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

func (s *Server) getCron(w http.ResponseWriter, r *http.Request) {
	cronID := strings.TrimSpace(chi.URLParam(r, "cron_id"))
	if cronID == "" {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_cron_id", "cron_id is required", observability.RequestIDFromContext(r.Context()))
		return
	}

	if s.pg != nil {
		cronJob, err := s.getCronFromPostgres(r.Context(), cronID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				contracts.WriteError(w, http.StatusNotFound, "cron_not_found", "cron not found", observability.RequestIDFromContext(r.Context()))
				return
			}
			contracts.WriteError(w, http.StatusInternalServerError, "db_query_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
			return
		}
		writeJSON(w, http.StatusOK, cronJob)
		return
	}

	s.store.mu.RLock()
	cronJob, ok := s.store.crons[cronID]
	s.store.mu.RUnlock()
	if !ok {
		contracts.WriteError(w, http.StatusNotFound, "cron_not_found", "cron not found", observability.RequestIDFromContext(r.Context()))
		return
	}
	writeJSON(w, http.StatusOK, cronJob)
}

func (s *Server) updateCron(w http.ResponseWriter, r *http.Request) {
	cronID := strings.TrimSpace(chi.URLParam(r, "cron_id"))
	if cronID == "" {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_cron_id", "cron_id is required", observability.RequestIDFromContext(r.Context()))
		return
	}

	var req struct {
		Schedule *string `json:"schedule"`
		Enabled  *bool   `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_json", err.Error(), observability.RequestIDFromContext(r.Context()))
		return
	}
	if req.Schedule == nil && req.Enabled == nil {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_cron_update", "at least one of schedule or enabled is required", observability.RequestIDFromContext(r.Context()))
		return
	}

	var schedule any
	if req.Schedule != nil {
		next := strings.TrimSpace(*req.Schedule)
		if next == "" {
			contracts.WriteError(w, http.StatusBadRequest, "invalid_cron_update", "schedule must be non-empty when provided", observability.RequestIDFromContext(r.Context()))
			return
		}
		schedule = next
	}
	var enabled any
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	if s.pg != nil {
		cronJob, err := s.updateCronInPostgres(r.Context(), cronID, schedule, enabled)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				contracts.WriteError(w, http.StatusNotFound, "cron_not_found", "cron not found", observability.RequestIDFromContext(r.Context()))
				return
			}
			contracts.WriteError(w, http.StatusBadRequest, "db_update_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
			return
		}
		writeJSON(w, http.StatusOK, cronJob)
		return
	}

	s.store.mu.Lock()
	cronJob, ok := s.store.crons[cronID]
	if !ok {
		s.store.mu.Unlock()
		contracts.WriteError(w, http.StatusNotFound, "cron_not_found", "cron not found", observability.RequestIDFromContext(r.Context()))
		return
	}
	if schedule != nil {
		cronJob.Schedule = schedule.(string)
	}
	if enabled != nil {
		cronJob.Enabled = enabled.(bool)
	}
	s.store.crons[cronID] = cronJob
	s.store.mu.Unlock()
	writeJSON(w, http.StatusOK, cronJob)
}

func (s *Server) deleteCron(w http.ResponseWriter, r *http.Request) {
	cronID := strings.TrimSpace(chi.URLParam(r, "cron_id"))
	if cronID == "" {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_cron_id", "cron_id is required", observability.RequestIDFromContext(r.Context()))
		return
	}

	if s.pg != nil {
		if err := s.deleteCronFromPostgres(r.Context(), cronID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				contracts.WriteError(w, http.StatusNotFound, "cron_not_found", "cron not found", observability.RequestIDFromContext(r.Context()))
				return
			}
			contracts.WriteError(w, http.StatusInternalServerError, "db_delete_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}

	s.store.mu.Lock()
	if _, ok := s.store.crons[cronID]; !ok {
		s.store.mu.Unlock()
		contracts.WriteError(w, http.StatusNotFound, "cron_not_found", "cron not found", observability.RequestIDFromContext(r.Context()))
		return
	}
	delete(s.store.crons, cronID)
	s.store.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) a2a(w http.ResponseWriter, r *http.Request) {
	type rpcRequest struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      string          `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}
	type rpcParams struct {
		ContextID string `json:"contextId"`
		TaskID    string `json:"taskId"`
		Message   struct {
			ContextID string `json:"contextId"`
		} `json:"message"`
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

	var params rpcParams
	_ = json.Unmarshal(req.Params, &params)

	threadID := strings.TrimSpace(params.ContextID)
	if threadID == "" {
		threadID = strings.TrimSpace(params.Message.ContextID)
	}
	if threadID == "" {
		threadID = "thread_default"
	}
	if req.Method == "message/stream" {
		startFrom := parseStartFrom(r.Header.Get("Last-Event-ID"))
		events := []streamEvent{
			mkEvent(1, "message_received", map[string]any{"thread_id": threadID, "assistant_id": chi.URLParam(r, "assistant_id")}),
			mkEvent(2, "message_completed", map[string]any{"thread_id": threadID, "status": "ok"}),
		}
		streamSSE(w, r, events, startFrom)
		return
	}
	if req.Method == "tasks/cancel" && !envBoolOrDefault("A2A_TASKS_CANCEL_SUPPORTED", false) {
		writeJSON(w, http.StatusOK, map[string]any{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"error": map[string]any{
				"code":    -32001,
				"message": "tasks/cancel unsupported",
			},
		})
		return
	}

	result := map[string]any{
		"accepted":  true,
		"method":    req.Method,
		"thread_id": threadID,
	}
	if req.Method == "tasks/get" || req.Method == "tasks/cancel" {
		taskID := strings.TrimSpace(params.TaskID)
		if taskID == "" {
			taskID = contracts.NewID("task")
		}
		result["task_id"] = taskID
		result["status"] = "ok"
	}
	writeJSON(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
}

func (s *Server) mcp(w http.ResponseWriter, r *http.Request) {
	type rpcRequest struct {
		Method string `json:"method"`
	}
	var req rpcRequest
	_ = json.NewDecoder(r.Body).Decode(&req)

	result := map[string]any{
		"jsonrpc": "2.0",
		"result": map[string]any{
			"stateless":         true,
			"session_terminate": "no-op",
			"transport":         "streamable-http",
		},
	}
	if strings.EqualFold(strings.TrimSpace(req.Method), "session/terminate") {
		result["result"] = map[string]any{"ok": true, "compatibility": "no-op"}
	}
	w.Header().Set("Content-Type", "application/json")
	writeJSON(w, http.StatusOK, result)
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
