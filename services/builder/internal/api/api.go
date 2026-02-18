package api

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"langopen.dev/builder/internal/langgraph"
	"langopen.dev/pkg/contracts"
	"langopen.dev/pkg/observability"
)

type API struct {
	logger         *slog.Logger
	router         chi.Router
	pg             *pgxpool.Pool
	executeJobs    bool
	buildNamespace string
	kube           kubernetes.Interface

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
	JobName      string            `json:"job_name,omitempty"`
	JobSubmitted bool              `json:"job_submitted"`
	LogsRef      string            `json:"logs_ref,omitempty"`
	LokiLabels   map[string]string `json:"loki_labels,omitempty"`
	CreatedAt    time.Time         `json:"created_at"`
	UpdatedAt    time.Time         `json:"updated_at"`
}

func New(logger *slog.Logger) *API {
	a := &API{
		logger:         logger,
		builds:         map[string]buildRecord{},
		logs:           map[string]string{},
		executeJobs:    envBoolOrDefault("BUILDKIT_EXECUTE_JOBS", false),
		buildNamespace: envOrDefault("BUILDKIT_NAMESPACE", "default"),
	}
	if dsn := strings.TrimSpace(os.Getenv("POSTGRES_DSN")); dsn != "" {
		pool, err := pgxpool.New(context.Background(), dsn)
		if err != nil {
			logger.Error("builder_postgres_connect_failed", "error", err)
		} else {
			a.pg = pool
		}
	}
	if a.executeJobs {
		kube, err := initKubeClient()
		if err != nil {
			logger.Error("builder_kube_client_init_failed", "error", err)
			a.executeJobs = false
		} else {
			a.kube = kube
		}
	}

	r := chi.NewRouter()
	r.Use(observability.CorrelationMiddleware(logger))

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	r.Handle("/metrics", observability.MetricsHandler())
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
	buildStatus := "succeeded"
	jobName := ""
	jobSubmitted := false
	logsRef := "inline://builds/" + buildID

	if a.executeJobs {
		jobSpec, err := langgraph.BuildKitJobSpec(req.RepoURL, req.GitRef, req.ImageName, req.CommitSHA)
		if err != nil {
			contracts.WriteError(w, http.StatusBadRequest, "invalid_request", err.Error(), observability.RequestIDFromContext(r.Context()))
			return
		}
		jobName, err = a.submitBuildJob(r.Context(), jobSpec)
		if err != nil {
			contracts.WriteError(w, http.StatusBadGateway, "build_job_submit_failed", err.Error(), observability.RequestIDFromContext(r.Context()))
			return
		}
		buildStatus = "queued"
		jobSubmitted = true
		logsRef = fmt.Sprintf("k8s://%s/jobs/%s", a.buildNamespace, jobName)
	}

	labels := map[string]string{
		"service":       "builder",
		"build_id":      buildID,
		"deployment_id": req.DeploymentID,
		"commit_sha":    req.CommitSHA,
	}
	logLines := []string{
		fmt.Sprintf("build_id=%s status=%s", buildID, buildStatus),
		"builder=buildkit-rootless image=moby/buildkit:rootless",
		fmt.Sprintf("source repo=%s ref=%s", req.RepoURL, req.GitRef),
		fmt.Sprintf("target image=%s:%s digest=%s", req.ImageName, req.CommitSHA, digest),
	}
	if jobSubmitted {
		logLines = append(logLines, fmt.Sprintf("job_submitted namespace=%s name=%s", a.buildNamespace, jobName))
	} else {
		logLines = append(logLines, "status=succeeded")
	}
	logs := strings.Join(logLines, "\n")

	rec := buildRecord{
		ID:           buildID,
		DeploymentID: req.DeploymentID,
		Status:       buildStatus,
		CommitSHA:    req.CommitSHA,
		ImageName:    req.ImageName,
		ImageDigest:  digest,
		JobName:      jobName,
		JobSubmitted: jobSubmitted,
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
		rec = a.refreshBuildRuntimeStatus(r.Context(), rec)
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
	rec = a.refreshBuildRuntimeStatus(r.Context(), rec)
	writeJSON(w, http.StatusOK, rec)
}

func (a *API) getBuildLogs(w http.ResponseWriter, r *http.Request) {
	buildID := strings.TrimSpace(chi.URLParam(r, "build_id"))
	if buildID == "" {
		contracts.WriteError(w, http.StatusBadRequest, "invalid_build_id", "build_id is required", observability.RequestIDFromContext(r.Context()))
		return
	}

	a.mu.RLock()
	rec, recOK := a.builds[buildID]
	logs, ok := a.logs[buildID]
	a.mu.RUnlock()
	if recOK && a.kube != nil {
		if namespace, jobName, hasJob := parseK8sJobRef(rec, a.buildNamespace); hasJob {
			jobLogs, err := a.fetchJobLogs(r.Context(), namespace, jobName)
			if err == nil && strings.TrimSpace(jobLogs) != "" {
				a.mu.Lock()
				a.logs[buildID] = jobLogs
				a.mu.Unlock()
				writeJSON(w, http.StatusOK, map[string]any{"build_id": buildID, "logs_ref": rec.LogsRef, "logs": jobLogs})
				return
			}
		}
	}
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
	rec = a.refreshBuildRuntimeStatus(r.Context(), rec)
	if a.kube != nil {
		if namespace, jobName, hasJob := parseK8sJobRef(rec, a.buildNamespace); hasJob {
			jobLogs, err := a.fetchJobLogs(r.Context(), namespace, jobName)
			if err == nil && strings.TrimSpace(jobLogs) != "" {
				a.mu.Lock()
				a.logs[buildID] = jobLogs
				a.mu.Unlock()
				writeJSON(w, http.StatusOK, map[string]any{"build_id": buildID, "logs_ref": rec.LogsRef, "logs": jobLogs})
				return
			}
		}
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
	if _, jobName, ok := parseLogsRef(rec.LogsRef); ok {
		rec.JobName = jobName
		rec.JobSubmitted = true
	}
	return rec, nil
}

func (a *API) refreshBuildRuntimeStatus(ctx context.Context, rec buildRecord) buildRecord {
	if a.kube == nil {
		return rec
	}
	namespace, jobName, ok := parseK8sJobRef(rec, a.buildNamespace)
	if !ok {
		return rec
	}

	nextStatus, err := a.lookupJobStatus(ctx, namespace, jobName)
	if err != nil {
		a.logger.Warn("builder_job_status_lookup_failed", "build_id", rec.ID, "namespace", namespace, "job_name", jobName, "error", err)
		return rec
	}
	if nextStatus == "" || nextStatus == rec.Status {
		return rec
	}

	rec.Status = nextStatus
	rec.JobName = jobName
	rec.JobSubmitted = true
	rec.UpdatedAt = time.Now().UTC()
	a.mu.Lock()
	a.builds[rec.ID] = rec
	a.mu.Unlock()
	if a.pg != nil {
		_, _ = a.pg.Exec(ctx, `UPDATE builds SET status=$2, updated_at=$3 WHERE id=$1`, rec.ID, rec.Status, rec.UpdatedAt)
	}
	return rec
}

func (a *API) lookupJobStatus(ctx context.Context, namespace, jobName string) (string, error) {
	job, err := a.kube.BatchV1().Jobs(namespace).Get(ctx, jobName, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	if job.Status.Succeeded > 0 {
		return "succeeded", nil
	}
	if job.Status.Failed > 0 {
		return "failed", nil
	}
	for _, cond := range job.Status.Conditions {
		if cond.Type == batchv1.JobComplete && cond.Status == corev1.ConditionTrue {
			return "succeeded", nil
		}
		if cond.Type == batchv1.JobFailed && cond.Status == corev1.ConditionTrue {
			return "failed", nil
		}
	}
	if job.Status.Active > 0 {
		return "running", nil
	}
	return "queued", nil
}

func (a *API) fetchJobLogs(ctx context.Context, namespace, jobName string) (string, error) {
	pods, err := a.kube.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "job-name=" + jobName,
	})
	if err != nil {
		return "", err
	}
	if len(pods.Items) == 0 {
		return "", errors.New("job pod not found")
	}
	pod := pods.Items[0]
	logOptions := &corev1.PodLogOptions{Container: "buildkit"}
	stream, err := a.kube.CoreV1().Pods(namespace).GetLogs(pod.Name, logOptions).Stream(ctx)
	if err != nil {
		logOptions.Container = ""
		stream, err = a.kube.CoreV1().Pods(namespace).GetLogs(pod.Name, logOptions).Stream(ctx)
		if err != nil {
			return "", err
		}
	}
	defer stream.Close()
	raw, err := io.ReadAll(stream)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func parseK8sJobRef(rec buildRecord, fallbackNamespace string) (string, string, bool) {
	if rec.JobName != "" {
		namespace := fallbackNamespace
		if ns, _, ok := parseLogsRef(rec.LogsRef); ok {
			namespace = ns
		}
		return namespace, rec.JobName, true
	}
	return parseLogsRef(rec.LogsRef)
}

func parseLogsRef(logsRef string) (string, string, bool) {
	if !strings.HasPrefix(logsRef, "k8s://") {
		return "", "", false
	}
	trimmed := strings.TrimPrefix(logsRef, "k8s://")
	parts := strings.Split(trimmed, "/jobs/")
	if len(parts) != 2 {
		return "", "", false
	}
	namespace := strings.TrimSpace(parts[0])
	jobName := strings.TrimSpace(parts[1])
	if namespace == "" || jobName == "" {
		return "", "", false
	}
	return namespace, jobName, true
}

func (a *API) submitBuildJob(ctx context.Context, jobSpec map[string]any) (string, error) {
	if a.kube == nil {
		return "", errors.New("kubernetes client is not configured")
	}
	raw, err := json.Marshal(jobSpec)
	if err != nil {
		return "", err
	}
	var job batchv1.Job
	if err := json.Unmarshal(raw, &job); err != nil {
		return "", fmt.Errorf("decode job spec: %w", err)
	}
	if job.Namespace == "" {
		job.Namespace = a.buildNamespace
	}
	if job.Spec.Template.Spec.RestartPolicy == "" {
		job.Spec.Template.Spec.RestartPolicy = corev1.RestartPolicyNever
	}
	if job.Name != "" {
		job.GenerateName = job.Name + "-"
		job.Name = ""
	}

	created, err := a.kube.BatchV1().Jobs(job.Namespace).Create(ctx, &job, metav1.CreateOptions{})
	if err != nil {
		if k8serrors.IsAlreadyExists(err) {
			job.Name = ""
			if job.GenerateName == "" {
				job.GenerateName = "buildkit-"
			}
			created, err = a.kube.BatchV1().Jobs(job.Namespace).Create(ctx, &job, metav1.CreateOptions{})
		}
	}
	if err != nil {
		return "", err
	}
	return created.Name, nil
}

func initKubeClient() (kubernetes.Interface, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		kubeconfig := strings.TrimSpace(os.Getenv("KUBECONFIG"))
		if kubeconfig == "" {
			home, homeErr := os.UserHomeDir()
			if homeErr == nil {
				kubeconfig = filepath.Join(home, ".kube", "config")
			}
		}
		if kubeconfig != "" {
			cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		}
	}
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(cfg)
}

func digestFor(imageName, commitSHA string) string {
	sum := sha256.Sum256([]byte(imageName + "@" + commitSHA))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func envOrDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
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

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
