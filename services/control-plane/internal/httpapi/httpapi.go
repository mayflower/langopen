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
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"langopen.dev/pkg/contracts"
	"langopen.dev/pkg/observability"
)

type API struct {
	logger     *slog.Logger
	router     chi.Router
	mu         sync.RWMutex
	state      state
	pg         *pgxpool.Pool
	builder    string
	httpClient *http.Client
}

type state struct {
	Deployments []map[string]any
	Builds      []map[string]any
	APIKeys     []map[string]any
	Audit       []map[string]any
	Secrets     []map[string]any
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

func New(logger *slog.Logger) *API {
	a := &API{
		logger:     logger,
		builder:    strings.TrimRight(strings.TrimSpace(os.Getenv("BUILDER_URL")), "/"),
		httpClient: &http.Client{Timeout: 12 * time.Second},
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
		v1.Get("/audit", a.listAudit)
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
		strings.HasPrefix(path, "/internal/v1/builds") ||
		strings.HasPrefix(path, "/internal/v1/sources/validate") ||
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
	if req.ProjectID == "" {
		req.ProjectID = "proj_default"
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
		ProjectID:      req.ProjectID,
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
		writeJSON(w, http.StatusCreated, rec)
		return
	}

	a.mu.Lock()
	a.state.Deployments = append(a.state.Deployments, mapFromDeployment(rec))
	a.state.Audit = append(a.state.Audit, map[string]any{"event": "deployment.created", "timestamp": now, "deployment_id": rec.ID, "project_id": rec.ProjectID})
	a.mu.Unlock()
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
	projectID := strings.TrimSpace(r.URL.Query().Get("project_id"))
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
	writeJSON(w, http.StatusOK, rec)
}

func (a *API) updateDeployment(w http.ResponseWriter, r *http.Request) {
	deploymentID := strings.TrimSpace(chi.URLParam(r, "deployment_id"))
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
	writeJSON(w, http.StatusOK, rec)
}

func (a *API) deleteDeployment(w http.ResponseWriter, r *http.Request) {
	deploymentID := strings.TrimSpace(chi.URLParam(r, "deployment_id"))
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
		_, _ = tx.Exec(r.Context(), `DELETE FROM deployment_secret_bindings WHERE deployment_id=$1`, deploymentID)
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

	remainingSecrets := make([]map[string]any, 0, len(a.state.Secrets))
	for _, secret := range a.state.Secrets {
		if depID, _ := secret["deployment_id"].(string); depID == deploymentID {
			continue
		}
		remainingSecrets = append(remainingSecrets, secret)
	}
	a.state.Secrets = remainingSecrets

	a.state.Audit = append(a.state.Audit, map[string]any{
		"event":         "deployment.deleted",
		"timestamp":     time.Now().UTC(),
		"deployment_id": deploymentID,
		"project_id":    rec.ProjectID,
	})
	a.mu.Unlock()
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

	payload := map[string]any{
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
		fallback := map[string]any{
			"id":            contracts.NewID("build"),
			"deployment_id": dep.ID,
			"status":        "queued",
			"commit_sha":    req.CommitSHA,
			"created_at":    time.Now().UTC(),
			"updated_at":    time.Now().UTC(),
			"warning":       "builder unavailable; build queued placeholder created",
		}
		a.mu.Lock()
		a.state.Builds = append(a.state.Builds, fallback)
		a.mu.Unlock()
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
		if buildID != "" {
			_, _ = a.pg.Exec(r.Context(), `UPDATE deployments SET current_image_digest=COALESCE(NULLIF($2,''), current_image_digest), updated_at=NOW() WHERE id=$1`, dep.ID, digest)
		}
		_ = a.writeAudit(r.Context(), dep.ProjectID, "build.triggered", map[string]any{"deployment_id": dep.ID, "build_id": buildID, "commit_sha": req.CommitSHA, "status": builderResp["status"]})
	}
	writeJSON(w, status, builderResp)
}

func (a *API) listBuilds(w http.ResponseWriter, r *http.Request) {
	projectID := strings.TrimSpace(r.URL.Query().Get("project_id"))
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
	projectID := strings.TrimSpace(r.URL.Query().Get("project_id"))
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
	projectID := strings.TrimSpace(r.URL.Query().Get("project_id"))
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
		contracts.WriteError(w, http.StatusBadGateway, "build_logs_fetch_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
		return
	}
	writeJSON(w, status, resp)
}

func (a *API) setRuntimePolicy(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DeploymentID   string `json:"deployment_id"`
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
		dep, _ := a.getDeployment(r.Context(), req.DeploymentID)
		_ = a.writeAudit(r.Context(), dep.ProjectID, "policy.runtime.updated", map[string]any{"deployment_id": req.DeploymentID, "runtime_profile": req.RuntimeProfile, "mode": req.Mode})
		writeJSON(w, http.StatusOK, map[string]any{"status": "applied", "policy": req})
		return
	}

	a.mu.Lock()
	a.state.Audit = append(a.state.Audit, map[string]any{"event": "policy.runtime.updated", "timestamp": time.Now().UTC(), "policy": req})
	a.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"status": "applied", "policy": req})
}

func (a *API) listAPIKeys(w http.ResponseWriter, r *http.Request) {
	projectID := strings.TrimSpace(r.URL.Query().Get("project_id"))
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
	projectID := strings.TrimSpace(req.ProjectID)
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
	if keyID == "" {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_key_id", "key_id is required", observability.RequestIDFromContext(r.Context()))
		return
	}

	if a.pg != nil {
		var projectID string
		err := a.pg.QueryRow(r.Context(), `
			UPDATE api_keys
			SET revoked_at=NOW()
			WHERE id=$1 AND revoked_at IS NULL
			RETURNING project_id
		`, keyID).Scan(&projectID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				contracts.WriteError(w, http.StatusNotFound, "api_key_not_found", "api key not found", observability.RequestIDFromContext(r.Context()))
				return
			}
			contracts.WriteError(w, http.StatusInternalServerError, "db_update_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
			return
		}
		_ = a.writeAudit(r.Context(), projectID, "api_key.revoked", map[string]any{"key_id": keyID})
		writeJSON(w, http.StatusOK, map[string]any{"status": "revoked", "key_id": keyID})
		return
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	for _, item := range a.state.APIKeys {
		if id, _ := item["id"].(string); id == keyID {
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
		SecretName   string `json:"secret_name"`
		TargetKey    string `json:"target_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_json", err.Error(), observability.RequestIDFromContext(r.Context()))
		return
	}
	req.DeploymentID = strings.TrimSpace(req.DeploymentID)
	req.SecretName = strings.TrimSpace(req.SecretName)
	req.TargetKey = strings.TrimSpace(req.TargetKey)
	if req.DeploymentID == "" || req.SecretName == "" {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_secret_binding", "deployment_id and secret_name are required", observability.RequestIDFromContext(r.Context()))
		return
	}

	event := map[string]any{"event": "secret.bound", "timestamp": time.Now().UTC(), "binding": req}
	if a.pg != nil {
		_, err := a.pg.Exec(r.Context(), `
			INSERT INTO deployment_secret_bindings (id, deployment_id, secret_name, target_key, created_at)
			VALUES ($1,$2,$3,NULLIF($4,''),NOW())
			ON CONFLICT (deployment_id, secret_name, target_key) DO NOTHING
		`, contracts.NewID("sb"), req.DeploymentID, req.SecretName, req.TargetKey)
		if err != nil {
			contracts.WriteError(w, http.StatusBadRequest, "db_insert_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
			return
		}

		projectID := "proj_default"
		if dep, err := a.getDeployment(r.Context(), req.DeploymentID); err == nil {
			projectID = dep.ProjectID
		}
		_ = a.writeAudit(r.Context(), projectID, "secret.bound", map[string]any{
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
	if deploymentID == "" {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_request", "deployment_id is required", observability.RequestIDFromContext(r.Context()))
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
	if bindingID == "" {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_binding_id", "binding_id is required", observability.RequestIDFromContext(r.Context()))
		return
	}

	if a.pg != nil {
		var deploymentID string
		err := a.pg.QueryRow(r.Context(), `
			DELETE FROM deployment_secret_bindings
			WHERE id=$1
			RETURNING deployment_id
		`, bindingID).Scan(&deploymentID)
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
	defer a.mu.Unlock()
	next := make([]map[string]any, 0, len(a.state.Secrets))
	found := false
	for _, item := range a.state.Secrets {
		id, _ := item["id"].(string)
		if id == bindingID {
			found = true
			continue
		}
		next = append(next, item)
	}
	if !found {
		contracts.WriteError(w, http.StatusNotFound, "secret_binding_not_found", "secret binding not found", observability.RequestIDFromContext(r.Context()))
		return
	}
	a.state.Secrets = next
	a.state.Audit = append(a.state.Audit, map[string]any{
		"event":      "secret.unbound",
		"timestamp":  time.Now().UTC(),
		"binding_id": bindingID,
	})
	writeJSON(w, http.StatusOK, map[string]any{"status": "deleted", "binding_id": bindingID})
}

func (a *API) listAudit(w http.ResponseWriter, r *http.Request) {
	projectID := strings.TrimSpace(r.URL.Query().Get("project_id"))
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
		return rec, nil
	}

	a.mu.RLock()
	defer a.mu.RUnlock()
	for _, item := range a.state.Deployments {
		if id, _ := item["id"].(string); id == deploymentID {
			return deploymentFromMap(item), nil
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
