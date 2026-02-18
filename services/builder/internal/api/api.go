package api

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
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"langopen.dev/builder/internal/langgraph"
	"langopen.dev/pkg/contracts"
	"langopen.dev/pkg/observability"
)

type API struct {
	logger *slog.Logger
	router chi.Router
	pg     *pgxpool.Pool

	mu     sync.RWMutex
	builds map[string]buildRecord
	logs   map[string]string
}

type buildRecord struct {
	ID           string            `json:"id"`
	DeploymentID string            `json:"deployment_id"`
	Status       string            `json:"status"`
	CommitSHA    string            `json:"commit_sha"`
	ImageName    string            `json:"image_name,omitempty"`
	ImageDigest  string            `json:"image_digest,omitempty"`
	LogsRef      string            `json:"logs_ref,omitempty"`
	LokiLabels   map[string]string `json:"loki_labels,omitempty"`
	CreatedAt    time.Time         `json:"created_at"`
	UpdatedAt    time.Time         `json:"updated_at"`
}

func New(logger *slog.Logger) *API {
	a := &API{
		logger: logger,
		builds: map[string]buildRecord{},
		logs:   map[string]string{},
	}
	if dsn := strings.TrimSpace(os.Getenv("POSTGRES_DSN")); dsn != "" {
		pool, err := pgxpool.New(context.Background(), dsn)
		if err != nil {
			logger.Error("builder_postgres_connect_failed", "error", err)
		} else {
			a.pg = pool
		}
	}

	r := chi.NewRouter()
	r.Use(observability.CorrelationMiddleware(logger))

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	r.Post("/internal/v1/builds/validate", a.validate)
	r.Post("/internal/v1/builds/job", a.buildJob)
	r.Post("/internal/v1/builds/trigger", a.triggerBuild)
	r.Get("/internal/v1/builds/{build_id}", a.getBuild)
	r.Get("/internal/v1/builds/{build_id}/logs", a.getBuildLogs)

	a.router = r
	return a
}

func (a *API) Router() http.Handler { return a.router }

func (a *API) validate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RepoPath      string `json:"repo_path"`
		LanggraphPath string `json:"langgraph_path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_json", err.Error(), observability.RequestIDFromContext(r.Context()))
		return
	}
	configPath := req.LanggraphPath
	if configPath == "" {
		configPath = filepath.Join(req.RepoPath, "langgraph.json")
	}
	result := langgraph.Validate(req.RepoPath, configPath)
	status := http.StatusOK
	if !result.Valid {
		status = http.StatusUnprocessableEntity
	}
	writeJSON(w, status, result)
}

func (a *API) buildJob(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RepoURL   string `json:"repo_url"`
		GitRef    string `json:"git_ref"`
		ImageName string `json:"image_name"`
		CommitSHA string `json:"commit_sha"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_json", err.Error(), observability.RequestIDFromContext(r.Context()))
		return
	}
	job, err := langgraph.BuildKitJobSpec(req.RepoURL, req.GitRef, req.ImageName, req.CommitSHA)
	if err != nil {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_request", err.Error(), observability.RequestIDFromContext(r.Context()))
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (a *API) triggerBuild(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DeploymentID string `json:"deployment_id"`
		RepoURL      string `json:"repo_url"`
		GitRef       string `json:"git_ref"`
		RepoPath     string `json:"repo_path"`
		Langgraph    string `json:"langgraph_path"`
		ImageName    string `json:"image_name"`
		CommitSHA    string `json:"commit_sha"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_json", err.Error(), observability.RequestIDFromContext(r.Context()))
		return
	}
	if strings.TrimSpace(req.DeploymentID) == "" || strings.TrimSpace(req.ImageName) == "" || strings.TrimSpace(req.CommitSHA) == "" {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_request", "deployment_id, image_name, and commit_sha are required", observability.RequestIDFromContext(r.Context()))
		return
	}
	if req.GitRef == "" {
		req.GitRef = "main"
	}
	if req.RepoPath != "" {
		configPath := req.Langgraph
		if configPath == "" {
			configPath = filepath.Join(req.RepoPath, "langgraph.json")
		}
		validation := langgraph.Validate(req.RepoPath, configPath)
		if !validation.Valid {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
				"status":     "failed_validation",
				"validation": validation,
			})
			return
		}
	}

	buildID := contracts.NewID("build")
	now := time.Now().UTC()
	digest := digestFor(req.ImageName, req.CommitSHA)
	labels := map[string]string{
		"service":       "builder",
		"build_id":      buildID,
		"deployment_id": req.DeploymentID,
		"commit_sha":    req.CommitSHA,
	}
	logLines := []string{
		fmt.Sprintf("build_id=%s status=queued", buildID),
		"builder=buildkit-rootless image=moby/buildkit:rootless",
		fmt.Sprintf("source repo=%s ref=%s", req.RepoURL, req.GitRef),
		fmt.Sprintf("target image=%s:%s digest=%s", req.ImageName, req.CommitSHA, digest),
		"status=succeeded",
	}
	logs := strings.Join(logLines, "\n")
	logsRef := "inline://builds/" + buildID

	rec := buildRecord{
		ID:           buildID,
		DeploymentID: req.DeploymentID,
		Status:       "succeeded",
		CommitSHA:    req.CommitSHA,
		ImageName:    req.ImageName,
		ImageDigest:  digest,
		LogsRef:      logsRef,
		LokiLabels:   labels,
		CreatedAt:    now,
		UpdatedAt:    now,
	}

	a.mu.Lock()
	a.builds[buildID] = rec
	a.logs[buildID] = logs
	a.mu.Unlock()

	if a.pg != nil {
		_, err := a.pg.Exec(r.Context(), `
			INSERT INTO builds (id, deployment_id, status, commit_sha, image_digest, logs_ref, created_at, updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		`, rec.ID, rec.DeploymentID, rec.Status, rec.CommitSHA, rec.ImageDigest, rec.LogsRef, rec.CreatedAt, rec.UpdatedAt)
		if err != nil {
			contracts.WriteError(w, http.StatusInternalServerError, "db_insert_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
			return
		}
	}

	writeJSON(w, http.StatusAccepted, rec)
}

func (a *API) getBuild(w http.ResponseWriter, r *http.Request) {
	buildID := strings.TrimSpace(chi.URLParam(r, "build_id"))
	if buildID == "" {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_build_id", "build_id is required", observability.RequestIDFromContext(r.Context()))
		return
	}

	a.mu.RLock()
	if rec, ok := a.builds[buildID]; ok {
		a.mu.RUnlock()
		writeJSON(w, http.StatusOK, rec)
		return
	}
	a.mu.RUnlock()

	if a.pg == nil {
		contracts.WriteError(w, http.StatusNotFound, "build_not_found", "build not found", observability.RequestIDFromContext(r.Context()))
		return
	}

	rec, err := a.getBuildFromPostgres(r.Context(), buildID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			contracts.WriteError(w, http.StatusNotFound, "build_not_found", "build not found", observability.RequestIDFromContext(r.Context()))
			return
		}
		contracts.WriteError(w, http.StatusInternalServerError, "db_query_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

func (a *API) getBuildLogs(w http.ResponseWriter, r *http.Request) {
	buildID := strings.TrimSpace(chi.URLParam(r, "build_id"))
	if buildID == "" {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_build_id", "build_id is required", observability.RequestIDFromContext(r.Context()))
		return
	}

	a.mu.RLock()
	logs, ok := a.logs[buildID]
	a.mu.RUnlock()
	if ok {
		writeJSON(w, http.StatusOK, map[string]any{"build_id": buildID, "logs": logs})
		return
	}

	if a.pg == nil {
		contracts.WriteError(w, http.StatusNotFound, "build_logs_not_found", "build logs not found", observability.RequestIDFromContext(r.Context()))
		return
	}

	rec, err := a.getBuildFromPostgres(r.Context(), buildID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			contracts.WriteError(w, http.StatusNotFound, "build_not_found", "build not found", observability.RequestIDFromContext(r.Context()))
			return
		}
		contracts.WriteError(w, http.StatusInternalServerError, "db_query_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"build_id": rec.ID,
		"logs_ref": rec.LogsRef,
		"logs":     "logs persisted by reference only; check external log store",
	})
}

func (a *API) getBuildFromPostgres(ctx context.Context, buildID string) (buildRecord, error) {
	var rec buildRecord
	err := a.pg.QueryRow(ctx, `
		SELECT id, deployment_id, status, commit_sha, COALESCE(image_digest,''), COALESCE(logs_ref,''), created_at, updated_at
		FROM builds WHERE id=$1
	`, buildID).Scan(&rec.ID, &rec.DeploymentID, &rec.Status, &rec.CommitSHA, &rec.ImageDigest, &rec.LogsRef, &rec.CreatedAt, &rec.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return buildRecord{}, sql.ErrNoRows
		}
		return buildRecord{}, err
	}
	return rec, nil
}

func digestFor(imageName, commitSHA string) string {
	sum := sha256.Sum256([]byte(imageName + "@" + commitSHA))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
