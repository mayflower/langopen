package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"langopen.dev/pkg/contracts"
	pkgcrypto "langopen.dev/pkg/crypto"
	"langopen.dev/pkg/observability"
)

type contextKey string

const projectIDContextKey contextKey = "project_id"

var errProjectScope = errors.New("project_scope_violation")

type API struct {
	logger              *slog.Logger
	router              chi.Router
	mu                  sync.RWMutex
	state               state
	pg                  *pgxpool.Pool
	builder             string
	httpClient          *http.Client
	dynamic             dynamic.Interface
	syncAgentDeployment bool
	strictAgentSync     bool
	agentNamespace      string
	defaultAgentImage   string
	deploymentVarCipher *pkgcrypto.AESGCM
}

type state struct {
	Deployments         []map[string]any
	Builds              []map[string]any
	APIKeys             []map[string]any
	Audit               []map[string]any
	Secrets             []map[string]any
	RuntimeVariables    []map[string]any
	DeploymentRevisions map[string][]deploymentRevisionRecord
}

type deploymentRecord struct {
	ID                 string    `json:"id"`
	ProjectID          string    `json:"project_id"`
	RepoURL            string    `json:"repo_url"`
	GitRef             string    `json:"git_ref"`
	RepoPath           string    `json:"repo_path"`
	RuntimeProfile     string    `json:"runtime_profile"`
	Mode               string    `json:"mode"`
	CurrentImageDigest string    `json:"current_image_digest,omitempty"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

type buildRecord struct {
	ID           string    `json:"id"`
	DeploymentID string    `json:"deployment_id"`
	Status       string    `json:"status"`
	CommitSHA    string    `json:"commit_sha"`
	ImageDigest  string    `json:"image_digest,omitempty"`
	LogsRef      string    `json:"logs_ref,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type deploymentRevisionRecord struct {
	ID           string         `json:"id"`
	DeploymentID string         `json:"deployment_id"`
	RevisionID   string         `json:"revision_id"`
	ImageDigest  string         `json:"image_digest"`
	Reason       string         `json:"reason"`
	Metadata     map[string]any `json:"metadata,omitempty"`
	CreatedAt    time.Time      `json:"created_at"`
}

type apiKeyRecord struct {
	ID        string     `json:"id"`
	ProjectID string     `json:"project_id"`
	Name      string     `json:"name"`
	CreatedAt time.Time  `json:"created_at"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

type secretBindingRecord struct {
	ID           string    `json:"id"`
	DeploymentID string    `json:"deployment_id"`
	SecretName   string    `json:"secret_name"`
	TargetKey    string    `json:"target_key,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

type deploymentRuntimeVariableRecord struct {
	Key         string    `json:"key"`
	IsSecret    bool      `json:"is_secret"`
	ValueMasked string    `json:"value_masked"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func New(logger *slog.Logger) *API {
	cipher, err := pkgcrypto.NewAESGCMFromEnv()
	if err != nil {
		logger.Error("deployment_vars_cipher_init_failed", "error", err)
		panic(err)
	}
	a := &API{
		logger:              logger,
		builder:             strings.TrimRight(strings.TrimSpace(os.Getenv("BUILDER_URL")), "/"),
		httpClient:          &http.Client{Timeout: 12 * time.Second},
		syncAgentDeployment: envBoolOrDefault("CONTROL_PLANE_SYNC_AGENTDEPLOYMENT", true),
		strictAgentSync:     envBoolOrDefault("CONTROL_PLANE_SYNC_AGENTDEPLOYMENT_STRICT", false),
		agentNamespace:      envOrDefault("AGENT_DEPLOYMENT_NAMESPACE", "default"),
		defaultAgentImage:   envOrDefault("AGENT_DEPLOYMENT_IMAGE", "ghcr.io/mayflower/langopen/agent-runtime:latest"),
		deploymentVarCipher: cipher,
	}
	if a.builder == "" {
		a.builder = "http://langopen-builder"
	}
	if dsn := strings.TrimSpace(os.Getenv("POSTGRES_DSN")); dsn != "" {
		pool, err := pgxpool.New(context.Background(), dsn)
		if err != nil {
			logger.Error("control_plane_postgres_connect_failed", "error", err)
		} else {
			a.pg = pool
		}
	}
	if a.syncAgentDeployment {
		dyn, err := initDynamicClient()
		if err != nil {
			logger.Warn("control_plane_dynamic_client_init_failed", "error", err)
		} else {
			a.dynamic = dyn
		}
	}

	r := chi.NewRouter()
	r.Use(observability.CorrelationMiddleware(logger))
	r.Use(a.rbacMiddleware)

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	r.Handle("/metrics", observability.MetricsHandler())

	r.Route("/internal/v1", func(v1 chi.Router) {
		v1.Post("/sources/validate", a.validateSource)
		v1.Post("/deployments", a.createDeployment)
		v1.Get("/deployments", a.listDeployments)
		v1.Get("/deployments/{deployment_id}", a.getDeploymentByID)
		v1.Patch("/deployments/{deployment_id}", a.updateDeployment)
		v1.Post("/deployments/{deployment_id}/rollback", a.rollbackDeployment)
		v1.Delete("/deployments/{deployment_id}", a.deleteDeployment)
		v1.Post("/builds", a.triggerBuild)
		v1.Get("/builds", a.listBuilds)
		v1.Get("/builds/{build_id}", a.getBuild)
		v1.Get("/builds/{build_id}/logs", a.getBuildLogs)
		v1.Get("/api-keys", a.listAPIKeys)
		v1.Post("/api-keys", a.createAPIKey)
		v1.Post("/api-keys/{key_id}/revoke", a.revokeAPIKey)
		v1.Post("/policies/runtime", a.setRuntimePolicy)
		v1.Post("/secrets/bind", a.bindSecret)
		v1.Get("/secrets/bindings", a.listSecretBindings)
		v1.Delete("/secrets/bindings/{binding_id}", a.deleteSecretBinding)
		v1.Get("/deployments/{deployment_id}/variables", a.listDeploymentVariables)
		v1.Put("/deployments/{deployment_id}/variables", a.upsertDeploymentVariable)
		v1.Delete("/deployments/{deployment_id}/variables/{key}", a.deleteDeploymentVariable)
		v1.Get("/audit", a.listAudit)
	})
	r.Route("/v2", func(v2 chi.Router) {
		v2.Get("/deployments", a.listDeployments)
		v2.Post("/deployments", a.createDeployment)
		v2.Get("/deployments/{deployment_id}", a.getDeploymentByID)
		v2.Patch("/deployments/{deployment_id}", a.updateDeployment)
		v2.Delete("/deployments/{deployment_id}", a.deleteDeployment)
		v2.Get("/deployments/{deployment_id}/revisions", a.listDeploymentRevisions)
		v2.Post("/deployments/{deployment_id}/rollback", a.rollbackDeploymentV2)
	})
	r.Route("/v1/integrations/github", func(v1gh chi.Router) {
		v1gh.Get("/auth", a.githubAuthMetadata)
		v1gh.Post("/validate", a.githubValidatePAT)
		v1gh.Get("/repos", a.githubListRepos)
	})
	a.router = r
	return a
}

func (a *API) Router() http.Handler { return a.router }

func (a *API) rbacMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.URL.Path == "/metrics" {
			next.ServeHTTP(w, r)
			return
		}

		if a.pg != nil {
			apiKey := strings.TrimSpace(r.Header.Get(contracts.HeaderAPIKey))
			if apiKey == "" {
				contracts.WriteError(w, http.StatusUnauthorized, "missing_api_key", "X-Api-Key header is required", observability.RequestIDFromContext(r.Context()))
				return
			}
			projectID, err := a.validateAPIKey(r.Context(), apiKey)
			if err != nil {
				contracts.WriteError(w, http.StatusUnauthorized, "invalid_api_key", "API key is invalid or revoked", observability.RequestIDFromContext(r.Context()))
				return
			}
			r = r.WithContext(context.WithValue(r.Context(), projectIDContextKey, projectID))
		}

		role := contracts.ProjectRole(strings.TrimSpace(r.Header.Get(contracts.HeaderProjectRole)))
		if role == "" {
			role = contracts.RoleAdmin
		}
		if !contracts.IsValidProjectRole(role) {
			contracts.WriteError(w, http.StatusBadRequest, "invalid_role", "X-Project-Role must be one of viewer, developer, operator, admin", observability.RequestIDFromContext(r.Context()))
			return
		}

		required := requiredRoleFor(r.Method, r.URL.Path)
		if !roleAtLeast(role, required) {
			contracts.WriteError(w, http.StatusForbidden, "forbidden", "insufficient role for requested action", observability.RequestIDFromContext(r.Context()))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func requiredRoleFor(method, path string) contracts.ProjectRole {
	if method == http.MethodGet || method == http.MethodHead {
		return contracts.RoleViewer
	}
	if strings.HasPrefix(path, "/internal/v1/api-keys") {
		return contracts.RoleAdmin
	}
	if path == "/internal/v1/policies/runtime" {
		return contracts.RoleOperator
	}
	if strings.HasPrefix(path, "/internal/v1/deployments") ||
		strings.HasPrefix(path, "/v2/deployments") ||
		strings.HasPrefix(path, "/internal/v1/builds") ||
		strings.HasPrefix(path, "/internal/v1/sources/validate") ||
		strings.HasPrefix(path, "/v1/integrations/github") ||
		strings.HasPrefix(path, "/internal/v1/secrets/") {
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

func projectIDFromContext(ctx context.Context) string {
	projectID, _ := ctx.Value(projectIDContextKey).(string)
	return strings.TrimSpace(projectID)
}

func (a *API) validateAPIKey(ctx context.Context, apiKey string) (string, error) {
	if a.pg == nil {
		return "", errors.New("postgres unavailable")
	}
	hash := hashAPIKey(apiKey)
	var projectID string
	err := a.pg.QueryRow(ctx, `SELECT project_id FROM api_keys WHERE key_hash=$1 AND revoked_at IS NULL LIMIT 1`, hash).Scan(&projectID)
	if err != nil {
		return "", err
	}
	return projectID, nil
}

func (a *API) resolveProjectID(ctx context.Context, requested string) (string, error) {
	requested = strings.TrimSpace(requested)
	authorized := projectIDFromContext(ctx)
	if authorized == "" {
		return requested, nil
	}
	if requested != "" && requested != authorized {
		return "", errProjectScope
	}
	return authorized, nil
}

func (a *API) createDeployment(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ProjectID      string `json:"project_id"`
		RepoURL        string `json:"repo_url"`
		GitRef         string `json:"git_ref"`
		RepoPath       string `json:"repo_path"`
		RuntimeProfile string `json:"runtime_profile"`
		Mode           string `json:"mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_json", err.Error(), observability.RequestIDFromContext(r.Context()))
		return
	}
	if strings.TrimSpace(req.RepoURL) == "" {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_deployment", "repo_url is required", observability.RequestIDFromContext(r.Context()))
		return
	}
	projectID, err := a.resolveProjectID(r.Context(), req.ProjectID)
	if err != nil {
		contracts.WriteError(w, http.StatusForbidden, "project_scope_violation", "project_id does not match API key scope", observability.RequestIDFromContext(r.Context()))
		return
	}
	if projectID == "" {
		projectID = "proj_default"
	}
	if req.GitRef == "" {
		req.GitRef = "main"
	}
	if req.RepoPath == "" {
		req.RepoPath = "/"
	}
	if req.RuntimeProfile == "" {
		req.RuntimeProfile = "gvisor"
	}
	if req.Mode == "" {
		req.Mode = "mode_a"
	}
	if req.Mode != "mode_a" && req.Mode != "mode_b" {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_mode", "mode must be mode_a or mode_b", observability.RequestIDFromContext(r.Context()))
		return
	}

	now := time.Now().UTC()
	rec := deploymentRecord{
		ID:             contracts.NewID("dep"),
		ProjectID:      projectID,
		RepoURL:        req.RepoURL,
		GitRef:         req.GitRef,
		RepoPath:       req.RepoPath,
		RuntimeProfile: req.RuntimeProfile,
		Mode:           req.Mode,
		CreatedAt:      now,
		UpdatedAt:      now,
	}

	if a.pg != nil {
		_, err := a.pg.Exec(r.Context(), `
			INSERT INTO deployments (id, project_id, repo_url, git_ref, repo_path, runtime_profile, mode, created_at, updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		`, rec.ID, rec.ProjectID, rec.RepoURL, rec.GitRef, rec.RepoPath, rec.RuntimeProfile, rec.Mode, rec.CreatedAt, rec.UpdatedAt)
		if err != nil {
			contracts.WriteError(w, http.StatusBadRequest, "db_insert_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
			return
		}
		_ = a.writeAudit(r.Context(), rec.ProjectID, "deployment.created", map[string]any{"deployment_id": rec.ID, "repo_url": rec.RepoURL, "git_ref": rec.GitRef})
		_, _ = a.recordDeploymentRevision(r.Context(), rec, "create", map[string]any{"source": "createDeployment"})
		if err := a.syncAgentDeploymentRecord(r.Context(), rec); err != nil {
			contracts.WriteError(w, http.StatusBadGateway, "agent_deployment_sync_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
			return
		}
		writeJSON(w, http.StatusCreated, rec)
		return
	}

	a.mu.Lock()
	a.state.Deployments = append(a.state.Deployments, mapFromDeployment(rec))
	a.state.Audit = append(a.state.Audit, map[string]any{"event": "deployment.created", "timestamp": now, "deployment_id": rec.ID, "project_id": rec.ProjectID})
	a.mu.Unlock()
	_, _ = a.recordDeploymentRevision(r.Context(), rec, "create", map[string]any{"source": "createDeployment"})
	if err := a.syncAgentDeploymentRecord(r.Context(), rec); err != nil {
		contracts.WriteError(w, http.StatusBadGateway, "agent_deployment_sync_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
		return
	}
	writeJSON(w, http.StatusCreated, rec)
}

func (a *API) validateSource(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RepoPath      string `json:"repo_path"`
		LanggraphPath string `json:"langgraph_path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_json", err.Error(), observability.RequestIDFromContext(r.Context()))
		return
	}
	if strings.TrimSpace(req.RepoPath) == "" {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_source", "repo_path is required", observability.RequestIDFromContext(r.Context()))
		return
	}

	payload := map[string]any{
		"repo_path":      req.RepoPath,
		"langgraph_path": req.LanggraphPath,
	}
	resp, status, err := a.callBuilder(r.Context(), "/internal/v1/builds/validate", payload)
	if err != nil {
		contracts.WriteError(w, http.StatusBadGateway, "source_validation_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
		return
	}
	writeJSON(w, status, resp)
}

func (a *API) listDeployments(w http.ResponseWriter, r *http.Request) {
	projectID, err := a.resolveProjectID(r.Context(), r.URL.Query().Get("project_id"))
	if err != nil {
		contracts.WriteError(w, http.StatusForbidden, "project_scope_violation", "project_id does not match API key scope", observability.RequestIDFromContext(r.Context()))
		return
	}
	if a.pg != nil {
		query := `
			SELECT id, project_id, repo_url, git_ref, repo_path, runtime_profile, mode, COALESCE(current_image_digest,''), created_at, updated_at
			FROM deployments
		`
		args := []any{}
		if projectID != "" {
			query += " WHERE project_id=$1"
			args = append(args, projectID)
		}
		query += " ORDER BY updated_at DESC LIMIT 500"
		rows, err := a.pg.Query(r.Context(), query, args...)
		if err != nil {
			contracts.WriteError(w, http.StatusInternalServerError, "db_query_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
			return
		}
		defer rows.Close()
		out := make([]deploymentRecord, 0)
		for rows.Next() {
			var rec deploymentRecord
			if err := rows.Scan(&rec.ID, &rec.ProjectID, &rec.RepoURL, &rec.GitRef, &rec.RepoPath, &rec.RuntimeProfile, &rec.Mode, &rec.CurrentImageDigest, &rec.CreatedAt, &rec.UpdatedAt); err != nil {
				contracts.WriteError(w, http.StatusInternalServerError, "db_scan_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
				return
			}
			out = append(out, rec)
		}
		writeJSON(w, http.StatusOK, out)
		return
	}

	a.mu.RLock()
	defer a.mu.RUnlock()
	if projectID == "" {
		writeJSON(w, http.StatusOK, a.state.Deployments)
		return
	}
	out := make([]map[string]any, 0)
	for _, dep := range a.state.Deployments {
		if pid, _ := dep["project_id"].(string); pid == projectID {
			out = append(out, dep)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) getDeploymentByID(w http.ResponseWriter, r *http.Request) {
	deploymentID := strings.TrimSpace(chi.URLParam(r, "deployment_id"))
	projectID, err := a.resolveProjectID(r.Context(), r.URL.Query().Get("project_id"))
	if err != nil {
		contracts.WriteError(w, http.StatusForbidden, "project_scope_violation", "project_id does not match API key scope", observability.RequestIDFromContext(r.Context()))
		return
	}
	if deploymentID == "" {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_deployment_id", "deployment_id is required", observability.RequestIDFromContext(r.Context()))
		return
	}
	rec, err := a.getDeployment(r.Context(), deploymentID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			contracts.WriteError(w, http.StatusNotFound, "deployment_not_found", "deployment not found", observability.RequestIDFromContext(r.Context()))
			return
		}
		contracts.WriteError(w, http.StatusInternalServerError, "deployment_lookup_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
		return
	}
	if projectID != "" && rec.ProjectID != projectID {
		contracts.WriteError(w, http.StatusNotFound, "deployment_not_found", "deployment not found", observability.RequestIDFromContext(r.Context()))
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

func (a *API) updateDeployment(w http.ResponseWriter, r *http.Request) {
	deploymentID := strings.TrimSpace(chi.URLParam(r, "deployment_id"))
	projectID, err := a.resolveProjectID(r.Context(), r.URL.Query().Get("project_id"))
	if err != nil {
		contracts.WriteError(w, http.StatusForbidden, "project_scope_violation", "project_id does not match API key scope", observability.RequestIDFromContext(r.Context()))
		return
	}
	if deploymentID == "" {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_deployment_id", "deployment_id is required", observability.RequestIDFromContext(r.Context()))
		return
	}

	var req struct {
		RepoURL        *string `json:"repo_url"`
		GitRef         *string `json:"git_ref"`
		RepoPath       *string `json:"repo_path"`
		RuntimeProfile *string `json:"runtime_profile"`
		Mode           *string `json:"mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_json", err.Error(), observability.RequestIDFromContext(r.Context()))
		return
	}
	if req.RepoURL == nil && req.GitRef == nil && req.RepoPath == nil && req.RuntimeProfile == nil && req.Mode == nil {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_deployment_update", "at least one updatable field is required", observability.RequestIDFromContext(r.Context()))
		return
	}

	rec, err := a.getDeployment(r.Context(), deploymentID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			contracts.WriteError(w, http.StatusNotFound, "deployment_not_found", "deployment not found", observability.RequestIDFromContext(r.Context()))
			return
		}
		contracts.WriteError(w, http.StatusInternalServerError, "deployment_lookup_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
		return
	}
	if projectID != "" && rec.ProjectID != projectID {
		contracts.WriteError(w, http.StatusNotFound, "deployment_not_found", "deployment not found", observability.RequestIDFromContext(r.Context()))
		return
	}
	if req.RepoURL != nil {
		value := strings.TrimSpace(*req.RepoURL)
		if value == "" {
			contracts.WriteError(w, http.StatusBadRequest, "invalid_repo_url", "repo_url cannot be empty", observability.RequestIDFromContext(r.Context()))
			return
		}
		rec.RepoURL = value
	}
	if req.GitRef != nil {
		value := strings.TrimSpace(*req.GitRef)
		if value == "" {
			contracts.WriteError(w, http.StatusBadRequest, "invalid_git_ref", "git_ref cannot be empty", observability.RequestIDFromContext(r.Context()))
			return
		}
		rec.GitRef = value
	}
	if req.RepoPath != nil {
		value := strings.TrimSpace(*req.RepoPath)
		if value == "" {
			contracts.WriteError(w, http.StatusBadRequest, "invalid_repo_path", "repo_path cannot be empty", observability.RequestIDFromContext(r.Context()))
			return
		}
		rec.RepoPath = value
	}
	if req.RuntimeProfile != nil {
		value := strings.TrimSpace(*req.RuntimeProfile)
		if value == "" {
			contracts.WriteError(w, http.StatusBadRequest, "invalid_runtime_profile", "runtime_profile cannot be empty", observability.RequestIDFromContext(r.Context()))
			return
		}
		rec.RuntimeProfile = value
	}
	if req.Mode != nil {
		value := strings.TrimSpace(*req.Mode)
		if value != "mode_a" && value != "mode_b" {
			contracts.WriteError(w, http.StatusBadRequest, "invalid_mode", "mode must be mode_a or mode_b", observability.RequestIDFromContext(r.Context()))
			return
		}
		rec.Mode = value
	}
	rec.UpdatedAt = time.Now().UTC()

	if a.pg != nil {
		tag, err := a.pg.Exec(r.Context(), `
			UPDATE deployments
			SET repo_url=$2, git_ref=$3, repo_path=$4, runtime_profile=$5, mode=$6, updated_at=$7
			WHERE id=$1
		`, rec.ID, rec.RepoURL, rec.GitRef, rec.RepoPath, rec.RuntimeProfile, rec.Mode, rec.UpdatedAt)
		if err != nil {
			contracts.WriteError(w, http.StatusInternalServerError, "db_update_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
			return
		}
		if tag.RowsAffected() == 0 {
			contracts.WriteError(w, http.StatusNotFound, "deployment_not_found", "deployment not found", observability.RequestIDFromContext(r.Context()))
			return
		}
		_ = a.writeAudit(r.Context(), rec.ProjectID, "deployment.updated", map[string]any{
			"deployment_id":   rec.ID,
			"repo_url":        rec.RepoURL,
			"git_ref":         rec.GitRef,
			"repo_path":       rec.RepoPath,
			"runtime_profile": rec.RuntimeProfile,
			"mode":            rec.Mode,
		})
		_, _ = a.recordDeploymentRevision(r.Context(), rec, "update", map[string]any{"source": "updateDeployment"})
		if err := a.syncAgentDeploymentRecord(r.Context(), rec); err != nil {
			contracts.WriteError(w, http.StatusBadGateway, "agent_deployment_sync_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
			return
		}
		writeJSON(w, http.StatusOK, rec)
		return
	}

	a.mu.Lock()
	updated := false
	for i, item := range a.state.Deployments {
		if id, _ := item["id"].(string); id == rec.ID {
			a.state.Deployments[i] = mapFromDeployment(rec)
			updated = true
			break
		}
	}
	if !updated {
		a.mu.Unlock()
		contracts.WriteError(w, http.StatusNotFound, "deployment_not_found", "deployment not found", observability.RequestIDFromContext(r.Context()))
		return
	}
	a.state.Audit = append(a.state.Audit, map[string]any{
		"event":         "deployment.updated",
		"timestamp":     rec.UpdatedAt,
		"deployment_id": rec.ID,
		"project_id":    rec.ProjectID,
	})
	a.mu.Unlock()
	_, _ = a.recordDeploymentRevision(r.Context(), rec, "update", map[string]any{"source": "updateDeployment"})
	if err := a.syncAgentDeploymentRecord(r.Context(), rec); err != nil {
		contracts.WriteError(w, http.StatusBadGateway, "agent_deployment_sync_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

func (a *API) rollbackDeployment(w http.ResponseWriter, r *http.Request) {
	deploymentID := strings.TrimSpace(chi.URLParam(r, "deployment_id"))
	if deploymentID == "" {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_deployment_id", "deployment_id is required", observability.RequestIDFromContext(r.Context()))
		return
	}

	var req struct {
		ImageDigest string `json:"image_digest"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_json", err.Error(), observability.RequestIDFromContext(r.Context()))
		return
	}
	imageDigest := strings.TrimSpace(req.ImageDigest)
	if imageDigest == "" {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_rollback_request", "image_digest is required", observability.RequestIDFromContext(r.Context()))
		return
	}
	a.rollbackDeploymentByDigest(w, r, deploymentID, imageDigest, "digest", map[string]any{"source": "internal_v1"})
}

func (a *API) rollbackDeploymentByDigest(w http.ResponseWriter, r *http.Request, deploymentID, imageDigest, rollbackSource string, rollbackMetadata map[string]any) {
	rec, err := a.getDeployment(r.Context(), deploymentID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			contracts.WriteError(w, http.StatusNotFound, "deployment_not_found", "deployment not found", observability.RequestIDFromContext(r.Context()))
			return
		}
		contracts.WriteError(w, http.StatusInternalServerError, "deployment_lookup_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
		return
	}
	projectID, err := a.resolveProjectID(r.Context(), rec.ProjectID)
	if err != nil {
		contracts.WriteError(w, http.StatusNotFound, "deployment_not_found", "deployment not found", observability.RequestIDFromContext(r.Context()))
		return
	}

	rec.CurrentImageDigest = imageDigest
	rec.UpdatedAt = time.Now().UTC()

	if a.pg != nil {
		tag, err := a.pg.Exec(r.Context(), `
			UPDATE deployments
			SET current_image_digest=$2, updated_at=$3
			WHERE id=$1
		`, rec.ID, rec.CurrentImageDigest, rec.UpdatedAt)
		if err != nil {
			contracts.WriteError(w, http.StatusInternalServerError, "db_update_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
			return
		}
		if tag.RowsAffected() == 0 {
			contracts.WriteError(w, http.StatusNotFound, "deployment_not_found", "deployment not found", observability.RequestIDFromContext(r.Context()))
			return
		}
		_ = a.writeAudit(r.Context(), projectID, "deployment.rolled_back", map[string]any{
			"deployment_id": rec.ID,
			"image_digest":  rec.CurrentImageDigest,
			"source":        rollbackSource,
		})
		_, _ = a.recordDeploymentRevision(r.Context(), rec, "rollback", rollbackMetadata)
		if err := a.syncAgentDeploymentRecord(r.Context(), rec); err != nil {
			contracts.WriteError(w, http.StatusBadGateway, "agent_deployment_sync_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
			return
		}
		writeJSON(w, http.StatusOK, rec)
		return
	}

	a.mu.Lock()
	updated := false
	for i, item := range a.state.Deployments {
		if id, _ := item["id"].(string); id == rec.ID {
			a.state.Deployments[i] = mapFromDeployment(rec)
			updated = true
			break
		}
	}
	if !updated {
		a.mu.Unlock()
		contracts.WriteError(w, http.StatusNotFound, "deployment_not_found", "deployment not found", observability.RequestIDFromContext(r.Context()))
		return
	}
	a.state.Audit = append(a.state.Audit, map[string]any{
		"event":         "deployment.rolled_back",
		"timestamp":     rec.UpdatedAt,
		"deployment_id": rec.ID,
		"project_id":    rec.ProjectID,
		"image_digest":  rec.CurrentImageDigest,
		"source":        rollbackSource,
	})
	a.mu.Unlock()
	_, _ = a.recordDeploymentRevision(r.Context(), rec, "rollback", rollbackMetadata)
	if err := a.syncAgentDeploymentRecord(r.Context(), rec); err != nil {
		contracts.WriteError(w, http.StatusBadGateway, "agent_deployment_sync_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

func (a *API) rollbackDeploymentV2(w http.ResponseWriter, r *http.Request) {
	deploymentID := strings.TrimSpace(chi.URLParam(r, "deployment_id"))
	if deploymentID == "" {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_deployment_id", "deployment_id is required", observability.RequestIDFromContext(r.Context()))
		return
	}
	var req struct {
		RevisionID  string `json:"revision_id"`
		ImageDigest string `json:"image_digest"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_json", err.Error(), observability.RequestIDFromContext(r.Context()))
		return
	}
	revisionID := strings.TrimSpace(req.RevisionID)
	imageDigest := strings.TrimSpace(req.ImageDigest)
	if revisionID == "" && imageDigest == "" {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_rollback_request", "revision_id or image_digest is required", observability.RequestIDFromContext(r.Context()))
		return
	}
	if imageDigest == "" {
		rev, err := a.getDeploymentRevisionByRevisionID(r.Context(), deploymentID, revisionID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				contracts.WriteError(w, http.StatusNotFound, "revision_not_found", "deployment revision not found", observability.RequestIDFromContext(r.Context()))
				return
			}
			contracts.WriteError(w, http.StatusInternalServerError, "revision_lookup_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
			return
		}
		imageDigest = strings.TrimSpace(rev.ImageDigest)
		if imageDigest == "" {
			contracts.WriteError(w, http.StatusBadRequest, "invalid_revision", "revision does not contain image_digest", observability.RequestIDFromContext(r.Context()))
			return
		}
	}
	a.rollbackDeploymentByDigest(w, r, deploymentID, imageDigest, "v2", map[string]any{
		"revision_id": revisionID,
		"source":      "v2",
	})
}

func (a *API) listDeploymentRevisions(w http.ResponseWriter, r *http.Request) {
	deploymentID := strings.TrimSpace(chi.URLParam(r, "deployment_id"))
	if deploymentID == "" {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_deployment_id", "deployment_id is required", observability.RequestIDFromContext(r.Context()))
		return
	}
	deployment, err := a.getDeployment(r.Context(), deploymentID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			contracts.WriteError(w, http.StatusNotFound, "deployment_not_found", "deployment not found", observability.RequestIDFromContext(r.Context()))
			return
		}
		contracts.WriteError(w, http.StatusInternalServerError, "deployment_lookup_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
		return
	}
	if _, err := a.resolveProjectID(r.Context(), deployment.ProjectID); err != nil {
		contracts.WriteError(w, http.StatusNotFound, "deployment_not_found", "deployment not found", observability.RequestIDFromContext(r.Context()))
		return
	}

	revisions, err := a.listDeploymentRevisionsForDeployment(r.Context(), deploymentID)
	if err != nil {
		contracts.WriteError(w, http.StatusInternalServerError, "revision_list_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": revisions, "total": len(revisions)})
}

func (a *API) githubAuthMetadata(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimSpace(r.Header.Get("X-Github-Token"))
	if token == "" {
		token = strings.TrimSpace(os.Getenv("GITHUB_PAT"))
	}
	installURL := strings.TrimSpace(os.Getenv("GITHUB_APP_INSTALL_URL"))
	writeJSON(w, http.StatusOK, map[string]any{
		"provider":            "github",
		"auth_mode":           "pat",
		"configured":          token != "",
		"installation_flow":   map[string]any{"supported": installURL != "", "install_url": installURL},
		"supported_endpoints": []string{"/v1/integrations/github/auth", "/v1/integrations/github/validate", "/v1/integrations/github/repos"},
	})
}

func (a *API) githubValidatePAT(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	token := strings.TrimSpace(req.Token)
	if token == "" {
		token = strings.TrimSpace(r.Header.Get("X-Github-Token"))
	}
	if token == "" {
		token = strings.TrimSpace(os.Getenv("GITHUB_PAT"))
	}
	if token == "" {
		contracts.WriteError(w, http.StatusBadRequest, "missing_github_pat", "provide token or configure GITHUB_PAT", observability.RequestIDFromContext(r.Context()))
		return
	}
	baseURL := strings.TrimRight(envOrDefault("GITHUB_API_BASE_URL", "https://api.github.com"), "/")
	user, status, err := a.githubRequest(r.Context(), token, baseURL+"/user")
	if err != nil {
		contracts.WriteError(w, http.StatusBadGateway, "github_request_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
		return
	}
	if status >= 400 {
		writeJSON(w, status, map[string]any{
			"provider": "github",
			"valid":    false,
			"status":   status,
			"error":    user,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"provider": "github",
		"valid":    true,
		"user": map[string]any{
			"login": user["login"],
			"id":    user["id"],
		},
	})
}

func (a *API) githubListRepos(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimSpace(r.Header.Get("X-Github-Token"))
	if token == "" {
		token = strings.TrimSpace(os.Getenv("GITHUB_PAT"))
	}
	if token == "" {
		contracts.WriteError(w, http.StatusBadRequest, "missing_github_pat", "set X-Github-Token or configure GITHUB_PAT", observability.RequestIDFromContext(r.Context()))
		return
	}
	page := clampInt(parseIntDefault(r.URL.Query().Get("page"), 1), 1, 1000)
	perPage := clampInt(parseIntDefault(r.URL.Query().Get("per_page"), 50), 1, 100)
	visibility := strings.TrimSpace(r.URL.Query().Get("visibility"))
	if visibility == "" {
		visibility = "all"
	}
	baseURL := strings.TrimRight(envOrDefault("GITHUB_API_BASE_URL", "https://api.github.com"), "/")
	u := fmt.Sprintf("%s/user/repos?sort=updated&per_page=%d&page=%d&visibility=%s", baseURL, perPage, page, url.QueryEscape(visibility))
	response, status, err := a.githubRequest(r.Context(), token, u)
	if err != nil {
		contracts.WriteError(w, http.StatusBadGateway, "github_request_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
		return
	}
	if status >= 400 {
		writeJSON(w, status, map[string]any{
			"provider": "github",
			"status":   status,
			"error":    response,
		})
		return
	}

	rawItems, _ := response["items"].([]any)
	items := make([]map[string]any, 0, len(rawItems))
	for _, it := range rawItems {
		repo, _ := it.(map[string]any)
		if repo == nil {
			continue
		}
		items = append(items, map[string]any{
			"id":          repo["id"],
			"name":        repo["name"],
			"full_name":   repo["full_name"],
			"private":     repo["private"],
			"default_ref": repo["default_branch"],
			"html_url":    repo["html_url"],
			"clone_url":   repo["clone_url"],
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"provider": "github",
		"items":    items,
		"page":     page,
		"per_page": perPage,
		"total":    len(items),
	})
}

func (a *API) deleteDeployment(w http.ResponseWriter, r *http.Request) {
	deploymentID := strings.TrimSpace(chi.URLParam(r, "deployment_id"))
	projectID, err := a.resolveProjectID(r.Context(), r.URL.Query().Get("project_id"))
	if err != nil {
		contracts.WriteError(w, http.StatusForbidden, "project_scope_violation", "project_id does not match API key scope", observability.RequestIDFromContext(r.Context()))
		return
	}
	if deploymentID == "" {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_deployment_id", "deployment_id is required", observability.RequestIDFromContext(r.Context()))
		return
	}

	rec, err := a.getDeployment(r.Context(), deploymentID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			contracts.WriteError(w, http.StatusNotFound, "deployment_not_found", "deployment not found", observability.RequestIDFromContext(r.Context()))
			return
		}
		contracts.WriteError(w, http.StatusInternalServerError, "deployment_lookup_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
		return
	}
	if projectID != "" && rec.ProjectID != projectID {
		contracts.WriteError(w, http.StatusNotFound, "deployment_not_found", "deployment not found", observability.RequestIDFromContext(r.Context()))
		return
	}

	if a.pg != nil {
		tx, err := a.pg.BeginTx(r.Context(), pgx.TxOptions{})
		if err != nil {
			contracts.WriteError(w, http.StatusInternalServerError, "db_begin_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
			return
		}
		defer func() { _ = tx.Rollback(r.Context()) }()

		_, _ = tx.Exec(r.Context(), `DELETE FROM webhook_deliveries WHERE run_id IN (SELECT r.id FROM runs r JOIN assistants a ON a.id = r.assistant_id WHERE a.deployment_id=$1)`, deploymentID)
		_, _ = tx.Exec(r.Context(), `DELETE FROM checkpoints WHERE run_id IN (SELECT r.id FROM runs r JOIN assistants a ON a.id = r.assistant_id WHERE a.deployment_id=$1)`, deploymentID)
		_, _ = tx.Exec(r.Context(), `DELETE FROM checkpoints WHERE thread_id IN (SELECT t.id FROM threads t JOIN assistants a ON a.id = t.assistant_id WHERE a.deployment_id=$1)`, deploymentID)
		_, _ = tx.Exec(r.Context(), `DELETE FROM cron_executions WHERE cron_job_id IN (SELECT cj.id FROM cron_jobs cj JOIN assistants a ON a.id = cj.assistant_id WHERE a.deployment_id=$1)`, deploymentID)
		_, _ = tx.Exec(r.Context(), `DELETE FROM cron_jobs WHERE assistant_id IN (SELECT id FROM assistants WHERE deployment_id=$1)`, deploymentID)
		_, _ = tx.Exec(r.Context(), `DELETE FROM runs WHERE assistant_id IN (SELECT id FROM assistants WHERE deployment_id=$1)`, deploymentID)
		_, _ = tx.Exec(r.Context(), `DELETE FROM threads WHERE assistant_id IN (SELECT id FROM assistants WHERE deployment_id=$1)`, deploymentID)
		_, _ = tx.Exec(r.Context(), `DELETE FROM assistants WHERE deployment_id=$1`, deploymentID)
		_, _ = tx.Exec(r.Context(), `DELETE FROM builds WHERE deployment_id=$1`, deploymentID)
		_, _ = tx.Exec(r.Context(), `DELETE FROM deployment_revisions WHERE deployment_id=$1`, deploymentID)
		_, _ = tx.Exec(r.Context(), `DELETE FROM deployment_secret_bindings WHERE deployment_id=$1`, deploymentID)
		_, _ = tx.Exec(r.Context(), `DELETE FROM deployment_runtime_variables WHERE deployment_id=$1`, deploymentID)
		tag, err := tx.Exec(r.Context(), `DELETE FROM deployments WHERE id=$1`, deploymentID)
		if err != nil {
			contracts.WriteError(w, http.StatusInternalServerError, "db_delete_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
			return
		}
		if tag.RowsAffected() == 0 {
			contracts.WriteError(w, http.StatusNotFound, "deployment_not_found", "deployment not found", observability.RequestIDFromContext(r.Context()))
			return
		}
		if err := tx.Commit(r.Context()); err != nil {
			contracts.WriteError(w, http.StatusInternalServerError, "db_commit_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
			return
		}
		_ = a.writeAudit(r.Context(), rec.ProjectID, "deployment.deleted", map[string]any{
			"deployment_id": rec.ID,
		})
		if err := a.deleteAgentDeployment(r.Context(), rec.ID); err != nil {
			contracts.WriteError(w, http.StatusBadGateway, "agent_deployment_sync_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "deleted", "deployment_id": deploymentID})
		return
	}

	a.mu.Lock()
	remainingDeployments := make([]map[string]any, 0, len(a.state.Deployments))
	found := false
	for _, dep := range a.state.Deployments {
		if id, _ := dep["id"].(string); id == deploymentID {
			found = true
			continue
		}
		remainingDeployments = append(remainingDeployments, dep)
	}
	if !found {
		a.mu.Unlock()
		contracts.WriteError(w, http.StatusNotFound, "deployment_not_found", "deployment not found", observability.RequestIDFromContext(r.Context()))
		return
	}
	a.state.Deployments = remainingDeployments

	remainingBuilds := make([]map[string]any, 0, len(a.state.Builds))
	for _, build := range a.state.Builds {
		if depID, _ := build["deployment_id"].(string); depID == deploymentID {
			continue
		}
		remainingBuilds = append(remainingBuilds, build)
	}
	a.state.Builds = remainingBuilds
	delete(a.state.DeploymentRevisions, deploymentID)

	remainingSecrets := make([]map[string]any, 0, len(a.state.Secrets))
	for _, secret := range a.state.Secrets {
		if depID, _ := secret["deployment_id"].(string); depID == deploymentID {
			continue
		}
		remainingSecrets = append(remainingSecrets, secret)
	}
	a.state.Secrets = remainingSecrets
	remainingVars := make([]map[string]any, 0, len(a.state.RuntimeVariables))
	for _, item := range a.state.RuntimeVariables {
		if depID, _ := item["deployment_id"].(string); depID == deploymentID {
			continue
		}
		remainingVars = append(remainingVars, item)
	}
	a.state.RuntimeVariables = remainingVars

	a.state.Audit = append(a.state.Audit, map[string]any{
		"event":         "deployment.deleted",
		"timestamp":     time.Now().UTC(),
		"deployment_id": deploymentID,
		"project_id":    rec.ProjectID,
	})
	a.mu.Unlock()
	if err := a.deleteAgentDeployment(r.Context(), deploymentID); err != nil {
		contracts.WriteError(w, http.StatusBadGateway, "agent_deployment_sync_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "deleted", "deployment_id": deploymentID})
}

func (a *API) triggerBuild(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DeploymentID  string `json:"deployment_id"`
		CommitSHA     string `json:"commit_sha"`
		ImageName     string `json:"image_name"`
		RepoPath      string `json:"repo_path"`
		LanggraphPath string `json:"langgraph_path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_json", err.Error(), observability.RequestIDFromContext(r.Context()))
		return
	}
	if strings.TrimSpace(req.DeploymentID) == "" || strings.TrimSpace(req.CommitSHA) == "" || strings.TrimSpace(req.ImageName) == "" {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_build_request", "deployment_id, commit_sha, and image_name are required", observability.RequestIDFromContext(r.Context()))
		return
	}

	dep, err := a.getDeployment(r.Context(), req.DeploymentID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			contracts.WriteError(w, http.StatusNotFound, "deployment_not_found", "deployment not found", observability.RequestIDFromContext(r.Context()))
			return
		}
		contracts.WriteError(w, http.StatusInternalServerError, "deployment_lookup_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
		return
	}
	if _, err := a.resolveProjectID(r.Context(), dep.ProjectID); err != nil {
		contracts.WriteError(w, http.StatusNotFound, "deployment_not_found", "deployment not found", observability.RequestIDFromContext(r.Context()))
		return
	}

	payload := map[string]any{
		"project_id":     dep.ProjectID,
		"deployment_id":  dep.ID,
		"repo_url":       dep.RepoURL,
		"git_ref":        dep.GitRef,
		"repo_path":      req.RepoPath,
		"langgraph_path": req.LanggraphPath,
		"image_name":     req.ImageName,
		"commit_sha":     req.CommitSHA,
	}
	if payload["repo_path"] == "" {
		payload["repo_path"] = dep.RepoPath
	}

	builderResp, status, err := a.callBuilder(r.Context(), "/internal/v1/builds/trigger", payload)
	if err != nil {
		fallbackID := contracts.NewID("build")
		fallbackLogsRef := "inline://builds/" + fallbackID
		now := time.Now().UTC()
		fallback := map[string]any{
			"id":            fallbackID,
			"deployment_id": dep.ID,
			"status":        "queued",
			"commit_sha":    req.CommitSHA,
			"logs_ref":      fallbackLogsRef,
			"created_at":    now,
			"updated_at":    now,
			"warning":       "builder unavailable; build queued placeholder created",
		}
		if a.pg != nil {
			_, insertErr := a.pg.Exec(r.Context(), `
				INSERT INTO builds (id, deployment_id, status, commit_sha, image_digest, logs_ref, created_at, updated_at)
				VALUES ($1,$2,$3,$4,'',$5,$6,$7)
			`, fallbackID, dep.ID, "queued", req.CommitSHA, fallbackLogsRef, now, now)
			if insertErr != nil {
				contracts.WriteError(w, http.StatusInternalServerError, "db_insert_failed", insertErr.Error(), observability.RequestIDFromContext(r.Context()))
				return
			}
		} else {
			a.mu.Lock()
			a.state.Builds = append(a.state.Builds, fallback)
			a.mu.Unlock()
		}
		_ = a.writeAudit(r.Context(), dep.ProjectID, "build.triggered", map[string]any{"deployment_id": dep.ID, "build_id": fallback["id"], "fallback": true})
		writeJSON(w, http.StatusAccepted, fallback)
		return
	}
	if status >= 400 {
		if a.pg != nil {
			_ = a.writeAudit(r.Context(), dep.ProjectID, "build.trigger_failed", map[string]any{
				"deployment_id": dep.ID,
				"commit_sha":    req.CommitSHA,
				"status":        status,
				"response":      builderResp,
			})
		}
		writeJSON(w, status, builderResp)
		return
	}

	if a.pg != nil {
		var buildID, digest string
		if v, ok := builderResp["id"].(string); ok {
			buildID = v
		}
		if v, ok := builderResp["image_digest"].(string); ok {
			digest = v
		}
		imageRef := strings.TrimSpace(digest)
		if strings.TrimSpace(req.ImageName) != "" && strings.TrimSpace(digest) != "" {
			imageRef = strings.TrimSpace(req.ImageName) + "@" + strings.TrimSpace(digest)
		}
		if buildID != "" {
			_, _ = a.pg.Exec(r.Context(), `UPDATE deployments SET current_image_digest=COALESCE(NULLIF($2,''), current_image_digest), updated_at=NOW() WHERE id=$1`, dep.ID, imageRef)
			dep.CurrentImageDigest = imageRef
			dep.UpdatedAt = time.Now().UTC()
			_, _ = a.recordDeploymentRevision(r.Context(), dep, "build", map[string]any{
				"build_id":   buildID,
				"commit_sha": req.CommitSHA,
				"status":     builderResp["status"],
			})
			if err := a.syncAgentDeploymentRecord(r.Context(), dep); err != nil {
				contracts.WriteError(w, http.StatusBadGateway, "agent_deployment_sync_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
				return
			}
		}
		_ = a.writeAudit(r.Context(), dep.ProjectID, "build.triggered", map[string]any{"deployment_id": dep.ID, "build_id": buildID, "commit_sha": req.CommitSHA, "status": builderResp["status"]})
	}
	writeJSON(w, status, builderResp)
}

func (a *API) listBuilds(w http.ResponseWriter, r *http.Request) {
	projectID, err := a.resolveProjectID(r.Context(), r.URL.Query().Get("project_id"))
	if err != nil {
		contracts.WriteError(w, http.StatusForbidden, "project_scope_violation", "project_id does not match API key scope", observability.RequestIDFromContext(r.Context()))
		return
	}
	if a.pg != nil {
		query := `
			SELECT b.id, b.deployment_id, b.status, b.commit_sha, COALESCE(b.image_digest,''), COALESCE(b.logs_ref,''), b.created_at, b.updated_at
			FROM builds b
		`
		args := []any{}
		if projectID != "" {
			query += `
				JOIN deployments d ON d.id = b.deployment_id
				WHERE d.project_id=$1
			`
			args = append(args, projectID)
		}
		query += " ORDER BY b.created_at DESC LIMIT 500"
		rows, err := a.pg.Query(r.Context(), query, args...)
		if err != nil {
			contracts.WriteError(w, http.StatusInternalServerError, "db_query_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
			return
		}
		defer rows.Close()
		out := make([]buildRecord, 0)
		for rows.Next() {
			var rec buildRecord
			if err := rows.Scan(&rec.ID, &rec.DeploymentID, &rec.Status, &rec.CommitSHA, &rec.ImageDigest, &rec.LogsRef, &rec.CreatedAt, &rec.UpdatedAt); err != nil {
				contracts.WriteError(w, http.StatusInternalServerError, "db_scan_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
				return
			}
			out = append(out, rec)
		}
		writeJSON(w, http.StatusOK, out)
		return
	}

	a.mu.RLock()
	defer a.mu.RUnlock()
	if projectID == "" {
		writeJSON(w, http.StatusOK, a.state.Builds)
		return
	}
	allowedDeployments := map[string]struct{}{}
	for _, dep := range a.state.Deployments {
		if pid, _ := dep["project_id"].(string); pid != projectID {
			continue
		}
		if id, _ := dep["id"].(string); id != "" {
			allowedDeployments[id] = struct{}{}
		}
	}
	filtered := make([]map[string]any, 0)
	for _, build := range a.state.Builds {
		deploymentID, _ := build["deployment_id"].(string)
		if _, ok := allowedDeployments[deploymentID]; ok {
			filtered = append(filtered, build)
		}
	}
	writeJSON(w, http.StatusOK, filtered)
}

func (a *API) getBuild(w http.ResponseWriter, r *http.Request) {
	buildID := strings.TrimSpace(chi.URLParam(r, "build_id"))
	projectID, err := a.resolveProjectID(r.Context(), r.URL.Query().Get("project_id"))
	if err != nil {
		contracts.WriteError(w, http.StatusForbidden, "project_scope_violation", "project_id does not match API key scope", observability.RequestIDFromContext(r.Context()))
		return
	}
	if buildID == "" {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_build_id", "build_id is required", observability.RequestIDFromContext(r.Context()))
		return
	}

	if projectID == "" {
		builderPath := "/internal/v1/builds/" + url.PathEscape(buildID)
		if resp, status, err := a.callBuilderMethod(r.Context(), http.MethodGet, builderPath, nil); err == nil {
			writeJSON(w, status, resp)
			return
		}
	}

	if a.pg != nil {
		var rec buildRecord
		query := `
			SELECT b.id, b.deployment_id, b.status, b.commit_sha, COALESCE(b.image_digest,''), COALESCE(b.logs_ref,''), b.created_at, b.updated_at
			FROM builds b
		`
		args := []any{buildID}
		if projectID == "" {
			query += ` WHERE b.id=$1`
		} else {
			query += `
				JOIN deployments d ON d.id = b.deployment_id
				WHERE b.id=$1 AND d.project_id=$2
			`
			args = append(args, projectID)
		}
		err := a.pg.QueryRow(r.Context(), query, args...).Scan(&rec.ID, &rec.DeploymentID, &rec.Status, &rec.CommitSHA, &rec.ImageDigest, &rec.LogsRef, &rec.CreatedAt, &rec.UpdatedAt)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				contracts.WriteError(w, http.StatusNotFound, "build_not_found", "build not found", observability.RequestIDFromContext(r.Context()))
				return
			}
			contracts.WriteError(w, http.StatusInternalServerError, "db_query_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
			return
		}
		writeJSON(w, http.StatusOK, rec)
		return
	}

	a.mu.RLock()
	defer a.mu.RUnlock()
	deploymentProjects := map[string]string{}
	if projectID != "" {
		for _, dep := range a.state.Deployments {
			depID, _ := dep["id"].(string)
			depProjectID, _ := dep["project_id"].(string)
			deploymentProjects[depID] = depProjectID
		}
	}
	for _, item := range a.state.Builds {
		if id, _ := item["id"].(string); id == buildID {
			if projectID != "" {
				deploymentID, _ := item["deployment_id"].(string)
				if deploymentProjects[deploymentID] != projectID {
					contracts.WriteError(w, http.StatusNotFound, "build_not_found", "build not found", observability.RequestIDFromContext(r.Context()))
					return
				}
			}
			writeJSON(w, http.StatusOK, item)
			return
		}
	}
	contracts.WriteError(w, http.StatusNotFound, "build_not_found", "build not found", observability.RequestIDFromContext(r.Context()))
}

func (a *API) getBuildLogs(w http.ResponseWriter, r *http.Request) {
	buildID := strings.TrimSpace(chi.URLParam(r, "build_id"))
	projectID, err := a.resolveProjectID(r.Context(), r.URL.Query().Get("project_id"))
	if err != nil {
		contracts.WriteError(w, http.StatusForbidden, "project_scope_violation", "project_id does not match API key scope", observability.RequestIDFromContext(r.Context()))
		return
	}
	if buildID == "" {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_build_id", "build_id is required", observability.RequestIDFromContext(r.Context()))
		return
	}
	if projectID != "" {
		if a.pg != nil {
			var exists bool
			err := a.pg.QueryRow(r.Context(), `
				SELECT EXISTS(
					SELECT 1
					FROM builds b
					JOIN deployments d ON d.id = b.deployment_id
					WHERE b.id=$1 AND d.project_id=$2
				)
			`, buildID, projectID).Scan(&exists)
			if err != nil {
				contracts.WriteError(w, http.StatusInternalServerError, "db_query_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
				return
			}
			if !exists {
				contracts.WriteError(w, http.StatusNotFound, "build_not_found", "build not found", observability.RequestIDFromContext(r.Context()))
				return
			}
		} else {
			a.mu.RLock()
			deploymentProjects := map[string]string{}
			for _, dep := range a.state.Deployments {
				depID, _ := dep["id"].(string)
				depProjectID, _ := dep["project_id"].(string)
				deploymentProjects[depID] = depProjectID
			}
			found := false
			allowed := false
			for _, build := range a.state.Builds {
				id, _ := build["id"].(string)
				if id != buildID {
					continue
				}
				found = true
				deploymentID, _ := build["deployment_id"].(string)
				if deploymentProjects[deploymentID] == projectID {
					allowed = true
				}
				break
			}
			a.mu.RUnlock()
			if !found || !allowed {
				contracts.WriteError(w, http.StatusNotFound, "build_not_found", "build not found", observability.RequestIDFromContext(r.Context()))
				return
			}
		}
	}
	builderPath := "/internal/v1/builds/" + url.PathEscape(buildID) + "/logs"
	resp, status, err := a.callBuilderMethod(r.Context(), http.MethodGet, builderPath, nil)
	if err != nil {
		if a.pg != nil {
			var logsRef string
			query := `SELECT COALESCE(logs_ref,'') FROM builds WHERE id=$1`
			args := []any{buildID}
			if projectID != "" {
				query = `
					SELECT COALESCE(b.logs_ref,'')
					FROM builds b
					JOIN deployments d ON d.id = b.deployment_id
					WHERE b.id=$1 AND d.project_id=$2
				`
				args = append(args, projectID)
			}
			if dbErr := a.pg.QueryRow(r.Context(), query, args...).Scan(&logsRef); dbErr == nil {
				writeJSON(w, http.StatusOK, map[string]any{
					"build_id": buildID,
					"logs_ref": logsRef,
					"logs":     "builder unavailable; use logs_ref to retrieve persisted logs",
				})
				return
			}
		} else {
			a.mu.RLock()
			for _, item := range a.state.Builds {
				id, _ := item["id"].(string)
				if id != buildID {
					continue
				}
				logsRef, _ := item["logs_ref"].(string)
				a.mu.RUnlock()
				writeJSON(w, http.StatusOK, map[string]any{
					"build_id": buildID,
					"logs_ref": logsRef,
					"logs":     "builder unavailable; use logs_ref to retrieve persisted logs",
				})
				return
			}
			a.mu.RUnlock()
		}
		contracts.WriteError(w, http.StatusBadGateway, "build_logs_fetch_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
		return
	}
	writeJSON(w, status, resp)
}

func (a *API) setRuntimePolicy(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DeploymentID   string `json:"deployment_id"`
		ProjectID      string `json:"project_id"`
		RuntimeProfile string `json:"runtime_profile"`
		Mode           string `json:"mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_json", err.Error(), observability.RequestIDFromContext(r.Context()))
		return
	}
	if req.DeploymentID == "" {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_request", "deployment_id is required", observability.RequestIDFromContext(r.Context()))
		return
	}
	if req.RuntimeProfile == "" {
		req.RuntimeProfile = "gvisor"
	}
	if req.Mode == "" {
		req.Mode = "mode_a"
	}
	if req.Mode != "mode_a" && req.Mode != "mode_b" {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_mode", "mode must be mode_a or mode_b", observability.RequestIDFromContext(r.Context()))
		return
	}
	projectID, err := a.resolveProjectID(r.Context(), req.ProjectID)
	if err != nil {
		contracts.WriteError(w, http.StatusForbidden, "project_scope_violation", "project_id does not match API key scope", observability.RequestIDFromContext(r.Context()))
		return
	}
	req.ProjectID = projectID

	dep, depErr := a.getDeployment(r.Context(), req.DeploymentID)
	if depErr != nil {
		if errors.Is(depErr, sql.ErrNoRows) {
			contracts.WriteError(w, http.StatusNotFound, "deployment_not_found", "deployment not found", observability.RequestIDFromContext(r.Context()))
			return
		}
		contracts.WriteError(w, http.StatusInternalServerError, "deployment_lookup_failed", depErr.Error(), observability.RequestIDFromContext(r.Context()))
		return
	}
	if req.ProjectID != "" && dep.ProjectID != req.ProjectID {
		contracts.WriteError(w, http.StatusNotFound, "deployment_not_found", "deployment not found", observability.RequestIDFromContext(r.Context()))
		return
	}

	if a.pg != nil {
		tag, err := a.pg.Exec(r.Context(), `UPDATE deployments SET runtime_profile=$2, mode=$3, updated_at=NOW() WHERE id=$1`, req.DeploymentID, req.RuntimeProfile, req.Mode)
		if err != nil {
			contracts.WriteError(w, http.StatusInternalServerError, "db_update_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
			return
		}
		if tag.RowsAffected() == 0 {
			contracts.WriteError(w, http.StatusNotFound, "deployment_not_found", "deployment not found", observability.RequestIDFromContext(r.Context()))
			return
		}
		_ = a.writeAudit(r.Context(), dep.ProjectID, "policy.runtime.updated", map[string]any{"deployment_id": req.DeploymentID, "runtime_profile": req.RuntimeProfile, "mode": req.Mode})
		dep.RuntimeProfile = req.RuntimeProfile
		dep.Mode = req.Mode
		dep.UpdatedAt = time.Now().UTC()
		if err := a.syncAgentDeploymentRecord(r.Context(), dep); err != nil {
			contracts.WriteError(w, http.StatusBadGateway, "agent_deployment_sync_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "applied", "policy": req})
		return
	}

	a.mu.Lock()
	updated := false
	var updatedRec deploymentRecord
	for i, item := range a.state.Deployments {
		id, _ := item["id"].(string)
		if id != req.DeploymentID {
			continue
		}
		rec := deploymentFromMap(item)
		rec.RuntimeProfile = req.RuntimeProfile
		rec.Mode = req.Mode
		rec.UpdatedAt = time.Now().UTC()
		a.state.Deployments[i] = mapFromDeployment(rec)
		updated = true
		updatedRec = rec
		break
	}
	a.state.Audit = append(a.state.Audit, map[string]any{"event": "policy.runtime.updated", "timestamp": time.Now().UTC(), "policy": req})
	a.mu.Unlock()
	if !updated {
		contracts.WriteError(w, http.StatusNotFound, "deployment_not_found", "deployment not found", observability.RequestIDFromContext(r.Context()))
		return
	}
	if err := a.syncAgentDeploymentRecord(r.Context(), updatedRec); err != nil {
		contracts.WriteError(w, http.StatusBadGateway, "agent_deployment_sync_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "applied", "policy": req})
}

func (a *API) listAPIKeys(w http.ResponseWriter, r *http.Request) {
	projectID, err := a.resolveProjectID(r.Context(), r.URL.Query().Get("project_id"))
	if err != nil {
		contracts.WriteError(w, http.StatusForbidden, "project_scope_violation", "project_id does not match API key scope", observability.RequestIDFromContext(r.Context()))
		return
	}
	if projectID == "" {
		projectID = "proj_default"
	}

	if a.pg != nil {
		rows, err := a.pg.Query(r.Context(), `
			SELECT id, project_id, name, created_at, revoked_at
			FROM api_keys
			WHERE project_id=$1
			ORDER BY created_at DESC
			LIMIT 500
		`, projectID)
		if err != nil {
			contracts.WriteError(w, http.StatusInternalServerError, "db_query_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
			return
		}
		defer rows.Close()
		out := make([]apiKeyRecord, 0)
		for rows.Next() {
			var rec apiKeyRecord
			var revokedAt sql.NullTime
			if err := rows.Scan(&rec.ID, &rec.ProjectID, &rec.Name, &rec.CreatedAt, &revokedAt); err != nil {
				contracts.WriteError(w, http.StatusInternalServerError, "db_scan_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
				return
			}
			if revokedAt.Valid {
				rt := revokedAt.Time
				rec.RevokedAt = &rt
			}
			out = append(out, rec)
		}
		writeJSON(w, http.StatusOK, out)
		return
	}

	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]map[string]any, 0)
	for _, item := range a.state.APIKeys {
		if p, _ := item["project_id"].(string); p == projectID {
			out = append(out, map[string]any{
				"id":         item["id"],
				"project_id": item["project_id"],
				"name":       item["name"],
				"created_at": item["created_at"],
				"revoked_at": item["revoked_at"],
			})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) createAPIKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ProjectID string `json:"project_id"`
		Name      string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_json", err.Error(), observability.RequestIDFromContext(r.Context()))
		return
	}
	projectID, err := a.resolveProjectID(r.Context(), req.ProjectID)
	if err != nil {
		contracts.WriteError(w, http.StatusForbidden, "project_scope_violation", "project_id does not match API key scope", observability.RequestIDFromContext(r.Context()))
		return
	}
	if projectID == "" {
		projectID = "proj_default"
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = "api-key"
	}

	rawKey, err := generateAPIKey()
	if err != nil {
		contracts.WriteError(w, http.StatusInternalServerError, "api_key_generation_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
		return
	}
	keyID := contracts.NewID("key")
	now := time.Now().UTC()
	keyHash := hashAPIKey(rawKey)

	if a.pg != nil {
		_, err := a.pg.Exec(r.Context(), `
			INSERT INTO api_keys (id, project_id, name, key_hash, created_at, revoked_at)
			VALUES ($1,$2,$3,$4,$5,NULL)
		`, keyID, projectID, name, keyHash, now)
		if err != nil {
			contracts.WriteError(w, http.StatusBadRequest, "db_insert_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
			return
		}
		_ = a.writeAudit(r.Context(), projectID, "api_key.created", map[string]any{"key_id": keyID, "name": name})
		writeJSON(w, http.StatusCreated, map[string]any{
			"id":         keyID,
			"project_id": projectID,
			"name":       name,
			"key":        rawKey,
			"created_at": now,
		})
		return
	}

	entry := map[string]any{
		"id":         keyID,
		"project_id": projectID,
		"name":       name,
		"created_at": now,
		"revoked_at": nil,
		"key_hash":   keyHash,
	}
	a.mu.Lock()
	a.state.APIKeys = append(a.state.APIKeys, entry)
	a.mu.Unlock()
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":         keyID,
		"project_id": projectID,
		"name":       name,
		"key":        rawKey,
		"created_at": now,
	})
}

func (a *API) revokeAPIKey(w http.ResponseWriter, r *http.Request) {
	keyID := strings.TrimSpace(chi.URLParam(r, "key_id"))
	projectID, err := a.resolveProjectID(r.Context(), r.URL.Query().Get("project_id"))
	if err != nil {
		contracts.WriteError(w, http.StatusForbidden, "project_scope_violation", "project_id does not match API key scope", observability.RequestIDFromContext(r.Context()))
		return
	}
	if keyID == "" {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_key_id", "key_id is required", observability.RequestIDFromContext(r.Context()))
		return
	}

	if a.pg != nil {
		query := `
			UPDATE api_keys
			SET revoked_at=NOW()
			WHERE id=$1 AND revoked_at IS NULL
			RETURNING project_id
		`
		args := []any{keyID}
		if projectID != "" {
			query = `
				UPDATE api_keys
				SET revoked_at=NOW()
				WHERE id=$1 AND revoked_at IS NULL AND project_id=$2
				RETURNING project_id
			`
			args = append(args, projectID)
		}
		var revokedProjectID string
		err := a.pg.QueryRow(r.Context(), query, args...).Scan(&revokedProjectID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				contracts.WriteError(w, http.StatusNotFound, "api_key_not_found", "api key not found", observability.RequestIDFromContext(r.Context()))
				return
			}
			contracts.WriteError(w, http.StatusInternalServerError, "db_update_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
			return
		}
		_ = a.writeAudit(r.Context(), revokedProjectID, "api_key.revoked", map[string]any{"key_id": keyID})
		writeJSON(w, http.StatusOK, map[string]any{"status": "revoked", "key_id": keyID})
		return
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	for _, item := range a.state.APIKeys {
		if id, _ := item["id"].(string); id == keyID {
			if projectID != "" {
				if itemProjectID, _ := item["project_id"].(string); itemProjectID != projectID {
					contracts.WriteError(w, http.StatusNotFound, "api_key_not_found", "api key not found", observability.RequestIDFromContext(r.Context()))
					return
				}
			}
			now := time.Now().UTC()
			item["revoked_at"] = now
			writeJSON(w, http.StatusOK, map[string]any{"status": "revoked", "key_id": keyID})
			return
		}
	}
	contracts.WriteError(w, http.StatusNotFound, "api_key_not_found", "api key not found", observability.RequestIDFromContext(r.Context()))
}

func (a *API) bindSecret(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DeploymentID string `json:"deployment_id"`
		ProjectID    string `json:"project_id"`
		SecretName   string `json:"secret_name"`
		TargetKey    string `json:"target_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_json", err.Error(), observability.RequestIDFromContext(r.Context()))
		return
	}
	req.DeploymentID = strings.TrimSpace(req.DeploymentID)
	projectID, err := a.resolveProjectID(r.Context(), req.ProjectID)
	if err != nil {
		contracts.WriteError(w, http.StatusForbidden, "project_scope_violation", "project_id does not match API key scope", observability.RequestIDFromContext(r.Context()))
		return
	}
	req.ProjectID = projectID
	req.SecretName = strings.TrimSpace(req.SecretName)
	req.TargetKey = strings.TrimSpace(req.TargetKey)
	if req.DeploymentID == "" || req.SecretName == "" {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_secret_binding", "deployment_id and secret_name are required", observability.RequestIDFromContext(r.Context()))
		return
	}
	dep, err := a.getDeployment(r.Context(), req.DeploymentID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			contracts.WriteError(w, http.StatusNotFound, "deployment_not_found", "deployment not found", observability.RequestIDFromContext(r.Context()))
			return
		}
		contracts.WriteError(w, http.StatusInternalServerError, "deployment_lookup_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
		return
	}
	if req.ProjectID != "" && dep.ProjectID != req.ProjectID {
		contracts.WriteError(w, http.StatusNotFound, "deployment_not_found", "deployment not found", observability.RequestIDFromContext(r.Context()))
		return
	}

	event := map[string]any{"event": "secret.bound", "timestamp": time.Now().UTC(), "binding": req}
	if a.pg != nil {
		_, err = a.pg.Exec(r.Context(), `
			INSERT INTO deployment_secret_bindings (id, deployment_id, secret_name, target_key, created_at)
			VALUES ($1,$2,$3,NULLIF($4,''),NOW())
			ON CONFLICT (deployment_id, secret_name, target_key) DO NOTHING
		`, contracts.NewID("sb"), req.DeploymentID, req.SecretName, req.TargetKey)
		if err != nil {
			contracts.WriteError(w, http.StatusBadRequest, "db_insert_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
			return
		}
		_ = a.writeAudit(r.Context(), dep.ProjectID, "secret.bound", map[string]any{
			"deployment_id": req.DeploymentID,
			"secret_name":   req.SecretName,
			"target_key":    req.TargetKey,
		})
		writeJSON(w, http.StatusOK, map[string]any{"status": "bound", "deployment_id": req.DeploymentID, "secret_name": req.SecretName})
		return
	} else {
		a.mu.Lock()
		exists := false
		for _, item := range a.state.Secrets {
			if dep, _ := item["deployment_id"].(string); dep == req.DeploymentID {
				if sec, _ := item["secret_name"].(string); sec == req.SecretName {
					if key, _ := item["target_key"].(string); key == req.TargetKey {
						exists = true
						break
					}
				}
			}
		}
		if !exists {
			a.state.Secrets = append(a.state.Secrets, map[string]any{
				"id":            contracts.NewID("sb"),
				"deployment_id": req.DeploymentID,
				"secret_name":   req.SecretName,
				"target_key":    req.TargetKey,
				"created_at":    time.Now().UTC(),
			})
		}
		a.state.Audit = append(a.state.Audit, event)
		a.mu.Unlock()
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "bound", "deployment_id": req.DeploymentID, "secret_name": req.SecretName})
}

func (a *API) listSecretBindings(w http.ResponseWriter, r *http.Request) {
	deploymentID := strings.TrimSpace(r.URL.Query().Get("deployment_id"))
	projectID, err := a.resolveProjectID(r.Context(), r.URL.Query().Get("project_id"))
	if err != nil {
		contracts.WriteError(w, http.StatusForbidden, "project_scope_violation", "project_id does not match API key scope", observability.RequestIDFromContext(r.Context()))
		return
	}
	if deploymentID == "" {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_request", "deployment_id is required", observability.RequestIDFromContext(r.Context()))
		return
	}
	dep, err := a.getDeployment(r.Context(), deploymentID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			contracts.WriteError(w, http.StatusNotFound, "deployment_not_found", "deployment not found", observability.RequestIDFromContext(r.Context()))
			return
		}
		contracts.WriteError(w, http.StatusInternalServerError, "deployment_lookup_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
		return
	}
	if projectID != "" && dep.ProjectID != projectID {
		contracts.WriteError(w, http.StatusNotFound, "deployment_not_found", "deployment not found", observability.RequestIDFromContext(r.Context()))
		return
	}

	if a.pg != nil {
		rows, err := a.pg.Query(r.Context(), `
			SELECT id, deployment_id, secret_name, COALESCE(target_key,''), created_at
			FROM deployment_secret_bindings
			WHERE deployment_id=$1
			ORDER BY created_at DESC
			LIMIT 200
		`, deploymentID)
		if err != nil {
			contracts.WriteError(w, http.StatusInternalServerError, "db_query_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
			return
		}
		defer rows.Close()
		out := make([]secretBindingRecord, 0)
		for rows.Next() {
			var rec secretBindingRecord
			if err := rows.Scan(&rec.ID, &rec.DeploymentID, &rec.SecretName, &rec.TargetKey, &rec.CreatedAt); err != nil {
				contracts.WriteError(w, http.StatusInternalServerError, "db_scan_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
				return
			}
			out = append(out, rec)
		}
		writeJSON(w, http.StatusOK, out)
		return
	}

	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]map[string]any, 0)
	for _, item := range a.state.Secrets {
		if dep, _ := item["deployment_id"].(string); dep == deploymentID {
			out = append(out, item)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) deleteSecretBinding(w http.ResponseWriter, r *http.Request) {
	bindingID := strings.TrimSpace(chi.URLParam(r, "binding_id"))
	projectID, err := a.resolveProjectID(r.Context(), r.URL.Query().Get("project_id"))
	if err != nil {
		contracts.WriteError(w, http.StatusForbidden, "project_scope_violation", "project_id does not match API key scope", observability.RequestIDFromContext(r.Context()))
		return
	}
	if bindingID == "" {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_binding_id", "binding_id is required", observability.RequestIDFromContext(r.Context()))
		return
	}

	if a.pg != nil {
		var deploymentID string
		query := `
			DELETE FROM deployment_secret_bindings
			WHERE id=$1
			RETURNING deployment_id
		`
		args := []any{bindingID}
		if projectID != "" {
			query = `
				DELETE FROM deployment_secret_bindings dsb
				USING deployments d
				WHERE dsb.id=$1 AND d.id = dsb.deployment_id AND d.project_id=$2
				RETURNING dsb.deployment_id
			`
			args = append(args, projectID)
		}
		err := a.pg.QueryRow(r.Context(), query, args...).Scan(&deploymentID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				contracts.WriteError(w, http.StatusNotFound, "secret_binding_not_found", "secret binding not found", observability.RequestIDFromContext(r.Context()))
				return
			}
			contracts.WriteError(w, http.StatusInternalServerError, "db_delete_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
			return
		}

		projectID := "proj_default"
		if dep, err := a.getDeployment(r.Context(), deploymentID); err == nil {
			projectID = dep.ProjectID
		}
		_ = a.writeAudit(r.Context(), projectID, "secret.unbound", map[string]any{
			"binding_id":    bindingID,
			"deployment_id": deploymentID,
		})
		writeJSON(w, http.StatusOK, map[string]any{"status": "deleted", "binding_id": bindingID})
		return
	}

	a.mu.Lock()
	next := make([]map[string]any, 0, len(a.state.Secrets))
	found := false
	deploymentOfBinding := ""
	for _, item := range a.state.Secrets {
		id, _ := item["id"].(string)
		if id == bindingID {
			found = true
			deploymentOfBinding, _ = item["deployment_id"].(string)
			continue
		}
		next = append(next, item)
	}
	if !found {
		a.mu.Unlock()
		contracts.WriteError(w, http.StatusNotFound, "secret_binding_not_found", "secret binding not found", observability.RequestIDFromContext(r.Context()))
		return
	}
	if projectID != "" {
		deploymentProject := ""
		for _, dep := range a.state.Deployments {
			id, _ := dep["id"].(string)
			if id == deploymentOfBinding {
				deploymentProject, _ = dep["project_id"].(string)
				break
			}
		}
		if deploymentProject != projectID {
			a.mu.Unlock()
			contracts.WriteError(w, http.StatusNotFound, "secret_binding_not_found", "secret binding not found", observability.RequestIDFromContext(r.Context()))
			return
		}
	}
	a.state.Secrets = next
	a.state.Audit = append(a.state.Audit, map[string]any{
		"event":      "secret.unbound",
		"timestamp":  time.Now().UTC(),
		"binding_id": bindingID,
	})
	a.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"status": "deleted", "binding_id": bindingID})
}

func (a *API) listDeploymentVariables(w http.ResponseWriter, r *http.Request) {
	deploymentID := strings.TrimSpace(chi.URLParam(r, "deployment_id"))
	projectID, err := a.resolveProjectID(r.Context(), r.URL.Query().Get("project_id"))
	if err != nil {
		contracts.WriteError(w, http.StatusForbidden, "project_scope_violation", "project_id does not match API key scope", observability.RequestIDFromContext(r.Context()))
		return
	}
	if deploymentID == "" {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_deployment_id", "deployment_id is required", observability.RequestIDFromContext(r.Context()))
		return
	}
	dep, err := a.getDeployment(r.Context(), deploymentID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			contracts.WriteError(w, http.StatusNotFound, "deployment_not_found", "deployment not found", observability.RequestIDFromContext(r.Context()))
			return
		}
		contracts.WriteError(w, http.StatusInternalServerError, "deployment_lookup_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
		return
	}
	if projectID != "" && dep.ProjectID != projectID {
		contracts.WriteError(w, http.StatusNotFound, "deployment_not_found", "deployment not found", observability.RequestIDFromContext(r.Context()))
		return
	}

	if a.pg != nil {
		rows, err := a.pg.Query(r.Context(), `
			SELECT key, is_secret, updated_at
			FROM deployment_runtime_variables
			WHERE deployment_id=$1
			ORDER BY updated_at DESC
			LIMIT 500
		`, deploymentID)
		if err != nil {
			contracts.WriteError(w, http.StatusInternalServerError, "db_query_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
			return
		}
		defer rows.Close()
		out := make([]deploymentRuntimeVariableRecord, 0)
		for rows.Next() {
			var rec deploymentRuntimeVariableRecord
			if err := rows.Scan(&rec.Key, &rec.IsSecret, &rec.UpdatedAt); err != nil {
				contracts.WriteError(w, http.StatusInternalServerError, "db_scan_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
				return
			}
			rec.ValueMasked = maskRuntimeVariableValue()
			out = append(out, rec)
		}
		writeJSON(w, http.StatusOK, out)
		return
	}

	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]deploymentRuntimeVariableRecord, 0)
	for _, item := range a.state.RuntimeVariables {
		if depID, _ := item["deployment_id"].(string); depID != deploymentID {
			continue
		}
		key, _ := item["key"].(string)
		isSecret, _ := item["is_secret"].(bool)
		updatedAt, _ := item["updated_at"].(time.Time)
		out = append(out, deploymentRuntimeVariableRecord{
			Key:         key,
			IsSecret:    isSecret,
			ValueMasked: maskRuntimeVariableValue(),
			UpdatedAt:   updatedAt,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) upsertDeploymentVariable(w http.ResponseWriter, r *http.Request) {
	deploymentID := strings.TrimSpace(chi.URLParam(r, "deployment_id"))
	if deploymentID == "" {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_deployment_id", "deployment_id is required", observability.RequestIDFromContext(r.Context()))
		return
	}
	var req struct {
		ProjectID string `json:"project_id"`
		Key       string `json:"key"`
		Value     string `json:"value"`
		IsSecret  *bool  `json:"is_secret"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_json", err.Error(), observability.RequestIDFromContext(r.Context()))
		return
	}
	projectID, err := a.resolveProjectID(r.Context(), req.ProjectID)
	if err != nil {
		contracts.WriteError(w, http.StatusForbidden, "project_scope_violation", "project_id does not match API key scope", observability.RequestIDFromContext(r.Context()))
		return
	}
	key := strings.TrimSpace(req.Key)
	if key == "" {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_variable", "key is required", observability.RequestIDFromContext(r.Context()))
		return
	}
	dep, err := a.getDeployment(r.Context(), deploymentID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			contracts.WriteError(w, http.StatusNotFound, "deployment_not_found", "deployment not found", observability.RequestIDFromContext(r.Context()))
			return
		}
		contracts.WriteError(w, http.StatusInternalServerError, "deployment_lookup_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
		return
	}
	if projectID != "" && dep.ProjectID != projectID {
		contracts.WriteError(w, http.StatusNotFound, "deployment_not_found", "deployment not found", observability.RequestIDFromContext(r.Context()))
		return
	}
	if a.deploymentVarCipher == nil {
		contracts.WriteError(w, http.StatusInternalServerError, "deployment_vars_cipher_missing", "deployment vars encryption is not configured", observability.RequestIDFromContext(r.Context()))
		return
	}
	isSecret := true
	if req.IsSecret != nil {
		isSecret = *req.IsSecret
	}
	ciphertext, err := a.deploymentVarCipher.EncryptString(req.Value)
	if err != nil {
		contracts.WriteError(w, http.StatusInternalServerError, "deployment_var_encrypt_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
		return
	}
	now := time.Now().UTC()
	if a.pg != nil {
		_, err := a.pg.Exec(r.Context(), `
			INSERT INTO deployment_runtime_variables (id, deployment_id, key, value_ciphertext, is_secret, created_at, updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$6)
			ON CONFLICT (deployment_id, key)
			DO UPDATE SET value_ciphertext=EXCLUDED.value_ciphertext, is_secret=EXCLUDED.is_secret, updated_at=EXCLUDED.updated_at
		`, contracts.NewID("drv"), deploymentID, key, ciphertext, isSecret, now)
		if err != nil {
			contracts.WriteError(w, http.StatusInternalServerError, "db_upsert_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
			return
		}
		_ = a.writeAudit(r.Context(), dep.ProjectID, "deployment.variable.upserted", map[string]any{
			"deployment_id": deploymentID,
			"key":           key,
			"is_secret":     isSecret,
		})
		writeJSON(w, http.StatusOK, deploymentRuntimeVariableRecord{
			Key:         key,
			IsSecret:    isSecret,
			ValueMasked: maskRuntimeVariableValue(),
			UpdatedAt:   now,
		})
		return
	}

	a.mu.Lock()
	replaced := false
	for i, item := range a.state.RuntimeVariables {
		if depID, _ := item["deployment_id"].(string); depID == deploymentID {
			if existingKey, _ := item["key"].(string); existingKey == key {
				a.state.RuntimeVariables[i] = map[string]any{
					"id":               item["id"],
					"deployment_id":    deploymentID,
					"key":              key,
					"value_ciphertext": ciphertext,
					"is_secret":        isSecret,
					"created_at":       item["created_at"],
					"updated_at":       now,
				}
				replaced = true
				break
			}
		}
	}
	if !replaced {
		a.state.RuntimeVariables = append(a.state.RuntimeVariables, map[string]any{
			"id":               contracts.NewID("drv"),
			"deployment_id":    deploymentID,
			"key":              key,
			"value_ciphertext": ciphertext,
			"is_secret":        isSecret,
			"created_at":       now,
			"updated_at":       now,
		})
	}
	a.state.Audit = append(a.state.Audit, map[string]any{
		"event":         "deployment.variable.upserted",
		"timestamp":     now,
		"deployment_id": deploymentID,
		"key":           key,
		"is_secret":     isSecret,
	})
	a.mu.Unlock()

	writeJSON(w, http.StatusOK, deploymentRuntimeVariableRecord{
		Key:         key,
		IsSecret:    isSecret,
		ValueMasked: maskRuntimeVariableValue(),
		UpdatedAt:   now,
	})
}

func (a *API) deleteDeploymentVariable(w http.ResponseWriter, r *http.Request) {
	deploymentID := strings.TrimSpace(chi.URLParam(r, "deployment_id"))
	key := strings.TrimSpace(chi.URLParam(r, "key"))
	if deploymentID == "" {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_deployment_id", "deployment_id is required", observability.RequestIDFromContext(r.Context()))
		return
	}
	if key == "" {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_variable_key", "key is required", observability.RequestIDFromContext(r.Context()))
		return
	}
	projectID, err := a.resolveProjectID(r.Context(), r.URL.Query().Get("project_id"))
	if err != nil {
		contracts.WriteError(w, http.StatusForbidden, "project_scope_violation", "project_id does not match API key scope", observability.RequestIDFromContext(r.Context()))
		return
	}
	dep, err := a.getDeployment(r.Context(), deploymentID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			contracts.WriteError(w, http.StatusNotFound, "deployment_not_found", "deployment not found", observability.RequestIDFromContext(r.Context()))
			return
		}
		contracts.WriteError(w, http.StatusInternalServerError, "deployment_lookup_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
		return
	}
	if projectID != "" && dep.ProjectID != projectID {
		contracts.WriteError(w, http.StatusNotFound, "deployment_not_found", "deployment not found", observability.RequestIDFromContext(r.Context()))
		return
	}

	if a.pg != nil {
		tag, err := a.pg.Exec(r.Context(), `DELETE FROM deployment_runtime_variables WHERE deployment_id=$1 AND key=$2`, deploymentID, key)
		if err != nil {
			contracts.WriteError(w, http.StatusInternalServerError, "db_delete_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
			return
		}
		if tag.RowsAffected() == 0 {
			contracts.WriteError(w, http.StatusNotFound, "deployment_variable_not_found", "deployment variable not found", observability.RequestIDFromContext(r.Context()))
			return
		}
		_ = a.writeAudit(r.Context(), dep.ProjectID, "deployment.variable.deleted", map[string]any{
			"deployment_id": deploymentID,
			"key":           key,
		})
		writeJSON(w, http.StatusOK, map[string]any{"status": "deleted", "key": key})
		return
	}

	a.mu.Lock()
	next := make([]map[string]any, 0, len(a.state.RuntimeVariables))
	found := false
	for _, item := range a.state.RuntimeVariables {
		if depID, _ := item["deployment_id"].(string); depID == deploymentID {
			if existingKey, _ := item["key"].(string); existingKey == key {
				found = true
				continue
			}
		}
		next = append(next, item)
	}
	if !found {
		a.mu.Unlock()
		contracts.WriteError(w, http.StatusNotFound, "deployment_variable_not_found", "deployment variable not found", observability.RequestIDFromContext(r.Context()))
		return
	}
	a.state.RuntimeVariables = next
	a.state.Audit = append(a.state.Audit, map[string]any{
		"event":         "deployment.variable.deleted",
		"timestamp":     time.Now().UTC(),
		"deployment_id": deploymentID,
		"key":           key,
	})
	a.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"status": "deleted", "key": key})
}

func maskRuntimeVariableValue() string {
	return "********"
}

func (a *API) listAudit(w http.ResponseWriter, r *http.Request) {
	projectID, err := a.resolveProjectID(r.Context(), r.URL.Query().Get("project_id"))
	if err != nil {
		contracts.WriteError(w, http.StatusForbidden, "project_scope_violation", "project_id does not match API key scope", observability.RequestIDFromContext(r.Context()))
		return
	}
	eventType := strings.TrimSpace(r.URL.Query().Get("event"))
	if a.pg != nil {
		query := `SELECT id, project_id, actor_id, event_type, payload, created_at FROM audit_logs`
		args := []any{}
		conditions := make([]string, 0, 2)
		if projectID != "" {
			args = append(args, projectID)
			conditions = append(conditions, fmt.Sprintf("project_id=$%d", len(args)))
		}
		if eventType != "" {
			args = append(args, eventType)
			conditions = append(conditions, fmt.Sprintf("event_type=$%d", len(args)))
		}
		if len(conditions) > 0 {
			query += " WHERE " + strings.Join(conditions, " AND ")
		}
		query += " ORDER BY created_at DESC LIMIT 500"
		rows, err := a.pg.Query(r.Context(), query, args...)
		if err != nil {
			contracts.WriteError(w, http.StatusInternalServerError, "db_query_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
			return
		}
		defer rows.Close()
		out := make([]map[string]any, 0)
		for rows.Next() {
			var id, projectID sql.NullString
			var actorID sql.NullString
			var eventType string
			var payloadRaw []byte
			var createdAt time.Time
			if err := rows.Scan(&id, &projectID, &actorID, &eventType, &payloadRaw, &createdAt); err != nil {
				contracts.WriteError(w, http.StatusInternalServerError, "db_scan_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
				return
			}
			payload := map[string]any{}
			_ = json.Unmarshal(payloadRaw, &payload)
			out = append(out, map[string]any{
				"id":         id.String,
				"project_id": projectID.String,
				"actor_id":   actorID.String,
				"event":      eventType,
				"payload":    payload,
				"created_at": createdAt,
			})
		}
		writeJSON(w, http.StatusOK, out)
		return
	}

	a.mu.RLock()
	defer a.mu.RUnlock()
	if projectID == "" && eventType == "" {
		writeJSON(w, http.StatusOK, a.state.Audit)
		return
	}
	out := make([]map[string]any, 0)
	for _, item := range a.state.Audit {
		if projectID != "" {
			if itemProjectID, _ := item["project_id"].(string); itemProjectID != projectID {
				continue
			}
		}
		if eventType != "" {
			itemEvent, _ := item["event"].(string)
			if itemEvent != eventType {
				continue
			}
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) getDeployment(ctx context.Context, deploymentID string) (deploymentRecord, error) {
	authorizedProjectID := projectIDFromContext(ctx)
	if a.pg != nil {
		var rec deploymentRecord
		err := a.pg.QueryRow(ctx, `
			SELECT id, project_id, repo_url, git_ref, repo_path, runtime_profile, mode, COALESCE(current_image_digest,''), created_at, updated_at
			FROM deployments WHERE id=$1
		`, deploymentID).Scan(&rec.ID, &rec.ProjectID, &rec.RepoURL, &rec.GitRef, &rec.RepoPath, &rec.RuntimeProfile, &rec.Mode, &rec.CurrentImageDigest, &rec.CreatedAt, &rec.UpdatedAt)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return deploymentRecord{}, sql.ErrNoRows
			}
			return deploymentRecord{}, err
		}
		if authorizedProjectID != "" && rec.ProjectID != authorizedProjectID {
			return deploymentRecord{}, sql.ErrNoRows
		}
		return rec, nil
	}

	a.mu.RLock()
	defer a.mu.RUnlock()
	for _, item := range a.state.Deployments {
		if id, _ := item["id"].(string); id == deploymentID {
			rec := deploymentFromMap(item)
			if authorizedProjectID != "" && rec.ProjectID != authorizedProjectID {
				return deploymentRecord{}, sql.ErrNoRows
			}
			return rec, nil
		}
	}
	return deploymentRecord{}, sql.ErrNoRows
}

func (a *API) callBuilder(ctx context.Context, path string, payload map[string]any) (map[string]any, int, error) {
	return a.callBuilderMethod(ctx, http.MethodPost, path, payload)
}

func (a *API) callBuilderMethod(ctx context.Context, method, path string, payload map[string]any) (map[string]any, int, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, 0, err
	}
	url := a.builder + path
	var reader io.Reader
	if payload != nil && (method == http.MethodPost || method == http.MethodPut || method == http.MethodPatch) {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, 0, err
	}
	if reader != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, resp.StatusCode, fmt.Errorf("decode builder response: %w", err)
		}
	}
	return out, resp.StatusCode, nil
}

func (a *API) writeAudit(ctx context.Context, projectID, eventType string, payload map[string]any) error {
	if a.pg == nil {
		return nil
	}
	raw, _ := json.Marshal(payload)
	_, err := a.pg.Exec(ctx, `
		INSERT INTO audit_logs (id, project_id, actor_id, event_type, payload, created_at)
		VALUES ($1,$2,$3,$4,$5,NOW())
	`, contracts.NewID("audit"), projectID, "system", eventType, raw)
	return err
}

func (a *API) recordDeploymentRevision(ctx context.Context, dep deploymentRecord, reason string, metadata map[string]any) (deploymentRevisionRecord, error) {
	record := deploymentRevisionRecord{
		ID:           contracts.NewID("dep_rev"),
		DeploymentID: dep.ID,
		RevisionID:   contracts.NewID("rev"),
		ImageDigest:  strings.TrimSpace(dep.CurrentImageDigest),
		Reason:       strings.TrimSpace(reason),
		Metadata:     metadata,
		CreatedAt:    time.Now().UTC(),
	}
	if record.Reason == "" {
		record.Reason = "update"
	}

	if a.pg != nil {
		metaRaw, _ := json.Marshal(record.Metadata)
		_, err := a.pg.Exec(ctx, `
			INSERT INTO deployment_revisions (id, deployment_id, revision_id, image_digest, repo_url, git_ref, repo_path, runtime_profile, mode, reason, metadata, created_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		`, record.ID, record.DeploymentID, record.RevisionID, record.ImageDigest, dep.RepoURL, dep.GitRef, dep.RepoPath, dep.RuntimeProfile, dep.Mode, record.Reason, metaRaw, record.CreatedAt)
		return record, err
	}

	a.mu.Lock()
	if a.state.DeploymentRevisions == nil {
		a.state.DeploymentRevisions = map[string][]deploymentRevisionRecord{}
	}
	a.state.DeploymentRevisions[record.DeploymentID] = append(a.state.DeploymentRevisions[record.DeploymentID], record)
	a.mu.Unlock()
	return record, nil
}

func (a *API) listDeploymentRevisionsForDeployment(ctx context.Context, deploymentID string) ([]deploymentRevisionRecord, error) {
	if a.pg != nil {
		rows, err := a.pg.Query(ctx, `
			SELECT id, deployment_id, revision_id, COALESCE(image_digest,''), reason, COALESCE(metadata, '{}'::jsonb), created_at
			FROM deployment_revisions
			WHERE deployment_id=$1
			ORDER BY created_at DESC
			LIMIT 200
		`, deploymentID)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		out := make([]deploymentRevisionRecord, 0, 32)
		for rows.Next() {
			var record deploymentRevisionRecord
			var metadataRaw []byte
			if err := rows.Scan(&record.ID, &record.DeploymentID, &record.RevisionID, &record.ImageDigest, &record.Reason, &metadataRaw, &record.CreatedAt); err != nil {
				return nil, err
			}
			record.Metadata = map[string]any{}
			_ = json.Unmarshal(metadataRaw, &record.Metadata)
			out = append(out, record)
		}
		return out, rows.Err()
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	items := append([]deploymentRevisionRecord(nil), a.state.DeploymentRevisions[deploymentID]...)
	for i, j := 0, len(items)-1; i < j; i, j = i+1, j-1 {
		items[i], items[j] = items[j], items[i]
	}
	return items, nil
}

func (a *API) getDeploymentRevisionByRevisionID(ctx context.Context, deploymentID, revisionID string) (deploymentRevisionRecord, error) {
	if a.pg != nil {
		var record deploymentRevisionRecord
		var metadataRaw []byte
		err := a.pg.QueryRow(ctx, `
			SELECT id, deployment_id, revision_id, COALESCE(image_digest,''), reason, COALESCE(metadata, '{}'::jsonb), created_at
			FROM deployment_revisions
			WHERE deployment_id=$1 AND revision_id=$2
			ORDER BY created_at DESC
			LIMIT 1
		`, deploymentID, revisionID).Scan(&record.ID, &record.DeploymentID, &record.RevisionID, &record.ImageDigest, &record.Reason, &metadataRaw, &record.CreatedAt)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return deploymentRevisionRecord{}, sql.ErrNoRows
			}
			return deploymentRevisionRecord{}, err
		}
		record.Metadata = map[string]any{}
		_ = json.Unmarshal(metadataRaw, &record.Metadata)
		return record, nil
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	for _, record := range a.state.DeploymentRevisions[deploymentID] {
		if record.RevisionID == revisionID {
			return record, nil
		}
	}
	return deploymentRevisionRecord{}, sql.ErrNoRows
}

func (a *API) githubRequest(ctx context.Context, token, rawURL string) (map[string]any, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if len(body) == 0 {
		return map[string]any{}, resp.StatusCode, nil
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err == nil {
		return obj, resp.StatusCode, nil
	}
	var arr []any
	if err := json.Unmarshal(body, &arr); err == nil {
		return map[string]any{"items": arr}, resp.StatusCode, nil
	}
	return map[string]any{"raw": string(body)}, resp.StatusCode, nil
}

func parseIntDefault(raw string, fallback int) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return value
}

func clampInt(value, min, max int) int {
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}

func generateAPIKey() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "lko_" + base64.RawURLEncoding.EncodeToString(buf), nil
}

func hashAPIKey(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func (a *API) syncAgentDeploymentRecord(ctx context.Context, dep deploymentRecord) error {
	if !a.syncAgentDeployment || a.dynamic == nil {
		return nil
	}
	namespace := strings.TrimSpace(a.agentNamespace)
	if namespace == "" {
		namespace = "default"
	}
	resource := schema.GroupVersionResource{
		Group:    "platform.langopen.dev",
		Version:  "v1alpha1",
		Resource: "agentdeployments",
	}
	desired := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "platform.langopen.dev/v1alpha1",
			"kind":       "AgentDeployment",
			"metadata": map[string]any{
				"name": dep.ID,
				"labels": map[string]any{
					"project_id": dep.ProjectID,
				},
			},
			"spec": map[string]any{
				"image":            a.agentImageForDeployment(dep),
				"runtimeClassName": runtimeClassForProfile(dep.RuntimeProfile),
				"mode":             normalizeMode(dep.Mode),
			},
		},
	}

	current, err := a.dynamic.Resource(resource).Namespace(namespace).Get(ctx, dep.ID, metav1.GetOptions{})
	if err != nil {
		if !k8serrors.IsNotFound(err) {
			return a.handleAgentSyncError("get", dep.ID, err)
		}
		_, err = a.dynamic.Resource(resource).Namespace(namespace).Create(ctx, desired, metav1.CreateOptions{})
		return a.handleAgentSyncError("create", dep.ID, err)
	}

	current.Object["spec"] = desired.Object["spec"]
	labels := map[string]any{}
	if metadata, ok := current.Object["metadata"].(map[string]any); ok {
		if existingLabels, ok := metadata["labels"].(map[string]any); ok {
			labels = existingLabels
		}
	}
	labels["project_id"] = dep.ProjectID
	if metadata, ok := current.Object["metadata"].(map[string]any); ok {
		metadata["labels"] = labels
	}
	_, err = a.dynamic.Resource(resource).Namespace(namespace).Update(ctx, current, metav1.UpdateOptions{})
	return a.handleAgentSyncError("update", dep.ID, err)
}

func (a *API) deleteAgentDeployment(ctx context.Context, deploymentID string) error {
	if !a.syncAgentDeployment || a.dynamic == nil {
		return nil
	}
	namespace := strings.TrimSpace(a.agentNamespace)
	if namespace == "" {
		namespace = "default"
	}
	resource := schema.GroupVersionResource{
		Group:    "platform.langopen.dev",
		Version:  "v1alpha1",
		Resource: "agentdeployments",
	}
	err := a.dynamic.Resource(resource).Namespace(namespace).Delete(ctx, deploymentID, metav1.DeleteOptions{})
	if err != nil && !k8serrors.IsNotFound(err) {
		return a.handleAgentSyncError("delete", deploymentID, err)
	}
	return nil
}

func (a *API) handleAgentSyncError(operation, deploymentID string, err error) error {
	if err == nil {
		return nil
	}
	if a.strictAgentSync {
		return err
	}
	a.logger.Warn("agent_deployment_sync_best_effort_failed",
		"operation", operation,
		"deployment_id", deploymentID,
		"error", err,
	)
	return nil
}

func (a *API) agentImageForDeployment(dep deploymentRecord) string {
	image := strings.TrimSpace(dep.CurrentImageDigest)
	if image == "" {
		image = strings.TrimSpace(a.defaultAgentImage)
	}
	if image == "" {
		image = "ghcr.io/mayflower/langopen/agent-runtime:latest"
	}
	return image
}

func runtimeClassForProfile(profile string) string {
	profile = strings.TrimSpace(strings.ToLower(profile))
	if profile == "" {
		return "gvisor"
	}
	if strings.HasPrefix(profile, "kata") {
		return profile
	}
	return "gvisor"
}

func normalizeMode(mode string) string {
	mode = strings.TrimSpace(strings.ToLower(mode))
	if mode == "mode_b" {
		return "mode_b"
	}
	return "mode_a"
}

func initDynamicClient() (dynamic.Interface, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		kubeconfig := strings.TrimSpace(os.Getenv("KUBECONFIG"))
		if kubeconfig == "" {
			home, homeErr := os.UserHomeDir()
			if homeErr == nil {
				kubeconfig = home + "/.kube/config"
			}
		}
		if kubeconfig != "" {
			cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		}
	}
	if err != nil {
		return nil, err
	}
	return dynamic.NewForConfig(cfg)
}

func envOrDefault(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func envBoolOrDefault(key string, fallback bool) bool {
	value := strings.TrimSpace(strings.ToLower(os.Getenv(key)))
	if value == "" {
		return fallback
	}
	switch value {
	case "1", "t", "true", "yes", "y", "on":
		return true
	case "0", "f", "false", "no", "n", "off":
		return false
	default:
		return fallback
	}
}

func mapFromDeployment(rec deploymentRecord) map[string]any {
	return map[string]any{
		"id":                   rec.ID,
		"project_id":           rec.ProjectID,
		"repo_url":             rec.RepoURL,
		"git_ref":              rec.GitRef,
		"repo_path":            rec.RepoPath,
		"runtime_profile":      rec.RuntimeProfile,
		"mode":                 rec.Mode,
		"current_image_digest": rec.CurrentImageDigest,
		"created_at":           rec.CreatedAt,
		"updated_at":           rec.UpdatedAt,
	}
}

func deploymentFromMap(in map[string]any) deploymentRecord {
	rec := deploymentRecord{}
	rec.ID, _ = in["id"].(string)
	rec.ProjectID, _ = in["project_id"].(string)
	rec.RepoURL, _ = in["repo_url"].(string)
	rec.GitRef, _ = in["git_ref"].(string)
	rec.RepoPath, _ = in["repo_path"].(string)
	rec.RuntimeProfile, _ = in["runtime_profile"].(string)
	rec.Mode, _ = in["mode"].(string)
	rec.CurrentImageDigest, _ = in["current_image_digest"].(string)
	return rec
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
