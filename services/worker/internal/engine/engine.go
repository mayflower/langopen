package engine

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
	"github.com/robfig/cron/v3"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"langopen.dev/pkg/contracts"
)

var (
	workerMetricsOnce       sync.Once
	workerQueueDepthGauge   prometheus.Gauge
	workerActiveRunsGauge   prometheus.Gauge
	sandboxAllocationsTotal *prometheus.CounterVec
	workerStuckRunsGauge    prometheus.Gauge
	webhookDeadLettersGauge prometheus.Gauge
	workerBackendUpGauge    *prometheus.GaugeVec
	webhookDeliveriesTotal  *prometheus.CounterVec
)

type Config struct {
	PostgresDSN string
	RedisAddr   string
	QueueKey    string
}

type Worker struct {
	cfg                 Config
	logger              *slog.Logger
	pg                  *pgxpool.Pool
	redis               *redis.Client
	httpClient          *http.Client
	runMode             string
	sandboxEnabled      bool
	sandboxNamespace    string
	sandboxTemplate     string
	dynamic             dynamic.Interface
	cronTickInterval    time.Duration
	webhookPollInterval time.Duration
	webhookMaxAttempts  int
	stuckRunThreshold   time.Duration
	executorMode        string
	executor            runExecutor
	modeBSandboxStrict  bool
}

type pendingRun struct {
	ID              string
	ThreadID        string
	AssistantID     string
	StreamResumable bool
	Input           any
	Command         any
	Configurable    any
	Metadata        map[string]any
	CheckpointID    string
	GraphID         string
	RepoPath        string
	RepoURL         string
	GitRef          string
}

type runCorrelation struct {
	OrgID        string
	ProjectID    string
	DeploymentID string
	AssistantID  string
	ThreadID     string
	RunID        string
}

type streamEvent struct {
	ID    int64           `json:"id"`
	Event string          `json:"event"`
	Data  json.RawMessage `json:"data"`
}

type webhookDelivery struct {
	ID        string
	RunID     string
	URL       string
	Attempts  int
	Status    string
	UpdatedAt time.Time
}

type runExecutor interface {
	Execute(ctx context.Context, run pendingRun) (executeResult, error)
}

type executeResult struct {
	Status contracts.RunStatus
	Output any
	Error  any
	Events []executorEvent
}

type executorEvent struct {
	Name    string
	Payload any
}

type simulatedExecutor struct{}

func (e *simulatedExecutor) Execute(_ context.Context, _ pendingRun) (executeResult, error) {
	return executeResult{
		Status: contracts.RunStatusSuccess,
		Output: map[string]any{"message": "simulated execution"},
		Events: []executorEvent{
			{Name: "token", Payload: map[string]any{"token": "hello"}},
		},
	}, nil
}

type runtimeRunnerExecutor struct {
	baseURL    string
	httpClient *http.Client
}

func (e *runtimeRunnerExecutor) Execute(ctx context.Context, run pendingRun) (executeResult, error) {
	payload := map[string]any{
		"run_id":        run.ID,
		"thread_id":     run.ThreadID,
		"assistant_id":  run.AssistantID,
		"input":         run.Input,
		"command":       run.Command,
		"configurable":  run.Configurable,
		"metadata":      run.Metadata,
		"checkpoint_id": run.CheckpointID,
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(e.baseURL, "/")+"/execute", bytes.NewReader(body))
	if err != nil {
		return executeResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.httpClient.Do(req)
	if err != nil {
		return executeResult{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return executeResult{}, fmt.Errorf("runtime-runner status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var parsed struct {
		Status string `json:"status"`
		Output any    `json:"output"`
		Error  any    `json:"error"`
		Events []struct {
			Event string `json:"event"`
			Data  any    `json:"data"`
		} `json:"events"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &parsed); err != nil {
			return executeResult{}, fmt.Errorf("decode runtime-runner response: %w", err)
		}
	}
	result := executeResult{
		Status: contracts.RunStatus(strings.TrimSpace(parsed.Status)),
		Output: parsed.Output,
		Error:  parsed.Error,
		Events: make([]executorEvent, 0, len(parsed.Events)),
	}
	if result.Status == "" {
		result.Status = contracts.RunStatusSuccess
	}
	for _, item := range parsed.Events {
		name := strings.TrimSpace(item.Event)
		if name == "" {
			continue
		}
		result.Events = append(result.Events, executorEvent{Name: name, Payload: item.Data})
	}
	return result, nil
}

func New(cfg Config, logger *slog.Logger) (*Worker, error) {
	initWorkerMetrics()
	w := &Worker{
		cfg:    cfg,
		logger: logger,
		runMode: func() string {
			mode := strings.TrimSpace(strings.ToLower(os.Getenv("RUN_MODE")))
			if mode == "mode_b" {
				return "mode_b"
			}
			return "mode_a"
		}(),
		sandboxNamespace: envOrDefault("SANDBOX_NAMESPACE", "default"),
		sandboxTemplate:  envOrDefault("SANDBOX_TEMPLATE_NAME", "langopen-default"),
		httpClient: &http.Client{
			Timeout: time.Duration(envIntOrDefault("WEBHOOK_TIMEOUT_SECONDS", 5)) * time.Second,
		},
		cronTickInterval:    time.Duration(envIntOrDefault("CRON_TICK_SECONDS", 30)) * time.Second,
		webhookPollInterval: time.Duration(envIntOrDefault("WEBHOOK_POLL_SECONDS", 5)) * time.Second,
		webhookMaxAttempts:  envIntOrDefault("WEBHOOK_MAX_ATTEMPTS", 6),
		stuckRunThreshold:   time.Duration(envIntOrDefault("STUCK_RUN_SECONDS", 300)) * time.Second,
		executorMode:        strings.TrimSpace(strings.ToLower(envOrDefault("LANGOPEN_EXECUTOR", "runtime"))),
		modeBSandboxStrict:  !envBoolOrDefault("MODE_B_ALLOW_SIMULATED_SANDBOX", false),
	}
	if cfg.PostgresDSN != "" {
		pg, err := pgxpool.New(context.Background(), cfg.PostgresDSN)
		if err != nil {
			return nil, fmt.Errorf("connect postgres: %w", err)
		}
		w.pg = pg
	}
	if cfg.RedisAddr != "" {
		w.redis = redis.NewClient(&redis.Options{
			Addr:     cfg.RedisAddr,
			Username: strings.TrimSpace(os.Getenv("REDIS_USERNAME")),
			Password: os.Getenv("REDIS_PASSWORD"),
		})
	}
	w.sandboxEnabled = envBoolOrDefault("SANDBOX_ENABLED", w.runMode == "mode_b")
	if w.sandboxEnabled {
		dynClient, err := initDynamicClient()
		if err != nil {
			w.logger.Error("sandbox_client_init_failed", "error", err)
			w.sandboxEnabled = false
		} else {
			w.dynamic = dynClient
		}
	}
	runtimeURL := envOrDefault("RUNTIME_RUNNER_URL", "http://langopen-runtime-runner")
	if w.executorMode == "simulated" {
		w.executor = &simulatedExecutor{}
	} else {
		w.executorMode = "runtime"
		w.executor = &runtimeRunnerExecutor{
			baseURL: runtimeURL,
			httpClient: &http.Client{
				Timeout: time.Duration(envIntOrDefault("RUNTIME_RUNNER_TIMEOUT_SECONDS", 900)) * time.Second,
			},
		}
	}
	w.logger.Info("worker_executor_configured", "executor_mode", w.executorMode, "runtime_runner_url", runtimeURL)
	return w, nil
}

func (w *Worker) Run(ctx context.Context) error {
	if w.redis == nil || w.pg == nil {
		w.logger.Warn("worker_disabled", "reason", "POSTGRES_DSN and REDIS_ADDR are required for production run execution")
		<-ctx.Done()
		return nil
	}

	w.logger.Info("worker_started", "queue", w.cfg.QueueKey)
	errCh := make(chan error, 3)
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		errCh <- w.runWakeupLoop(ctx)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		errCh <- w.runCronLoop(ctx)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		errCh <- w.runWebhookLoop(ctx)
	}()

	var err error
	select {
	case <-ctx.Done():
	case err = <-errCh:
	}
	wg.Wait()
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

func (w *Worker) runWakeupLoop(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		result, err := w.redis.BLPop(ctx, 2*time.Second, w.cfg.QueueKey).Result()
		if err != nil {
			if errors.Is(err, redis.Nil) || errors.Is(err, context.DeadlineExceeded) {
				// Fallback poll keeps execution progressing if wake signals are missed.
				run, fetchErr := w.fetchNextPendingRun(ctx)
				if fetchErr != nil {
					if errors.Is(fetchErr, sql.ErrNoRows) {
						continue
					}
					w.logger.Error("fetch_pending_run_failed", "error", fetchErr)
					continue
				}
				w.executeRun(ctx, run)
				continue
			}
			if errors.Is(err, context.Canceled) {
				return nil
			}
			w.logger.Error("wakeup_pop_failed", "error", err)
			continue
		}
		if len(result) < 2 {
			// Defensive fallback if Redis returns a malformed wakeup payload.
			run, fetchErr := w.fetchNextPendingRun(ctx)
			if fetchErr != nil {
				if errors.Is(fetchErr, sql.ErrNoRows) {
					continue
				}
				w.logger.Error("fetch_pending_run_failed", "error", fetchErr)
				continue
			}
			w.executeRun(ctx, run)
			continue
		}
		w.observeQueueDepth(ctx)

		run, err := w.fetchNextPendingRun(ctx)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			w.logger.Error("fetch_pending_run_failed", "error", err)
			continue
		}
		w.executeRun(ctx, run)
	}
}

func (w *Worker) runCronLoop(ctx context.Context) error {
	ticker := time.NewTicker(w.cronTickInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := w.dispatchDueCrons(ctx, time.Now().UTC()); err != nil && !errors.Is(err, context.Canceled) {
				w.logger.Error("dispatch_due_crons_failed", "error", err)
			}
		}
	}
}

func (w *Worker) runWebhookLoop(ctx context.Context) error {
	ticker := time.NewTicker(w.webhookPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := w.processWebhookOutbox(ctx, time.Now().UTC()); err != nil && !errors.Is(err, context.Canceled) {
				w.logger.Error("process_webhook_outbox_failed", "error", err)
			}
			w.observeOperationalHealth(ctx)
		}
	}
}

func (w *Worker) fetchNextPendingRun(ctx context.Context) (pendingRun, error) {
	tx, err := w.pg.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return pendingRun{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	run := pendingRun{}
	var (
		rawInput    []byte
		rawMetadata []byte
	)
	err = tx.QueryRow(ctx, `
		SELECT
			r.id,
			COALESCE(r.thread_id,''),
			r.assistant_id,
			r.stream_resumable,
			COALESCE(r.input_json, '{}'::jsonb),
			COALESCE(r.metadata_json, '{}'::jsonb),
			COALESCE(r.checkpoint_id, ''),
			COALESCE(a.graph_id, ''),
			COALESCE(d.repo_path, ''),
			COALESCE(d.repo_url, ''),
			COALESCE(d.git_ref, '')
		FROM runs r
		JOIN assistants a ON a.id = r.assistant_id
		JOIN deployments d ON d.id = a.deployment_id
		WHERE r.status='pending'
		ORDER BY r.created_at ASC
		FOR UPDATE SKIP LOCKED
		LIMIT 1
	`).Scan(&run.ID, &run.ThreadID, &run.AssistantID, &run.StreamResumable, &rawInput, &rawMetadata, &run.CheckpointID, &run.GraphID, &run.RepoPath, &run.RepoURL, &run.GitRef)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return pendingRun{}, sql.ErrNoRows
		}
		return pendingRun{}, err
	}
	_ = json.Unmarshal(rawInput, &run.Input)
	metadata := map[string]any{}
	_ = json.Unmarshal(rawMetadata, &metadata)
	run.Metadata = metadata
	if configurable, ok := metadata["configurable"]; ok {
		run.Configurable = configurable
	}
	if command, ok := metadata["command"]; ok {
		run.Command = command
	}
	if run.Configurable == nil {
		run.Configurable = map[string]any{}
	}
	if cfg, ok := run.Configurable.(map[string]any); ok {
		if strings.TrimSpace(run.GraphID) != "" {
			if _, exists := cfg["graph_target"]; !exists {
				cfg["graph_target"] = run.GraphID
			}
		}
		if strings.TrimSpace(run.RepoPath) != "" {
			if _, exists := cfg["repo_path"]; !exists {
				cfg["repo_path"] = run.RepoPath
			}
		}
		if strings.TrimSpace(run.RepoURL) != "" {
			if _, exists := cfg["repo_url"]; !exists {
				cfg["repo_url"] = run.RepoURL
			}
		}
		if strings.TrimSpace(run.GitRef) != "" {
			if _, exists := cfg["git_ref"]; !exists {
				cfg["git_ref"] = run.GitRef
			}
		}
		run.Configurable = cfg
	}

	_, err = tx.Exec(ctx, `UPDATE runs SET status='running', started_at=COALESCE(started_at, NOW()), updated_at=NOW() WHERE id=$1`, run.ID)
	if err != nil {
		return pendingRun{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return pendingRun{}, err
	}
	return run, nil
}

func (w *Worker) executeRun(ctx context.Context, run pendingRun) {
	workerActiveRunsGauge.Inc()
	defer workerActiveRunsGauge.Dec()

	corr := w.fetchRunCorrelation(ctx, run)
	w.logger.Info("run_started",
		"org_id", corr.OrgID,
		"project_id", corr.ProjectID,
		"deployment_id", corr.DeploymentID,
		"assistant_id", corr.AssistantID,
		"thread_id", corr.ThreadID,
		"run_id", corr.RunID,
		"executor_mode", w.executorMode,
	)
	action := ""
	defer func() {
		_ = w.redis.Del(ctx, cancelKeyPrefix()+run.ID).Err()
		w.logger.Info("run_finished",
			"org_id", corr.OrgID,
			"project_id", corr.ProjectID,
			"deployment_id", corr.DeploymentID,
			"assistant_id", corr.AssistantID,
			"thread_id", corr.ThreadID,
			"run_id", corr.RunID,
			"action", action,
		)
	}()
	if w.runMode == "mode_b" {
		if w.sandboxEnabled {
			sandboxID, err := w.allocateSandboxClaim(ctx, run.ID)
			if err != nil {
				sandboxAllocationsTotal.WithLabelValues("failed").Inc()
				w.logger.Error("sandbox_allocation_failed",
					"org_id", corr.OrgID,
					"project_id", corr.ProjectID,
					"deployment_id", corr.DeploymentID,
					"assistant_id", corr.AssistantID,
					"thread_id", corr.ThreadID,
					"run_id", corr.RunID,
					"error", err,
				)
				_ = w.updateRunStatus(ctx, run.ID, contracts.RunStatusError)
				_ = w.updateCronExecutionStatus(ctx, run.ID, "error")
				w.emitStreamEvent(ctx, run, "run_sandbox_allocation_failed", map[string]any{"run_id": run.ID, "error": err.Error()})
				if enqueueErr := w.enqueueWebhookForRun(ctx, run.ID); enqueueErr != nil {
					w.logger.Error("enqueue_webhook_failed",
						"org_id", corr.OrgID,
						"project_id", corr.ProjectID,
						"deployment_id", corr.DeploymentID,
						"assistant_id", corr.AssistantID,
						"thread_id", corr.ThreadID,
						"run_id", corr.RunID,
						"error", enqueueErr,
					)
				}
				return
			}
			sandboxAllocationsTotal.WithLabelValues("succeeded").Inc()
			w.emitStreamEvent(ctx, run, "run_sandbox_allocated", map[string]any{"run_id": run.ID, "sandbox_id": sandboxID, "mode": w.runMode})
		} else {
			if w.modeBSandboxStrict {
				_ = w.updateRunResult(ctx, run.ID, contracts.RunStatusError, nil, map[string]any{
					"message": "mode_b requires sandbox integration but sandbox is disabled/unavailable",
				})
				_ = w.updateCronExecutionStatus(ctx, run.ID, "error")
				w.emitStreamEvent(ctx, run, "run_failed", map[string]any{
					"run_id":  run.ID,
					"status":  "error",
					"message": "mode_b requires sandbox integration but sandbox is disabled/unavailable",
				})
				_ = w.enqueueWebhookForRun(ctx, run.ID)
				return
			}
			sandboxAllocationsTotal.WithLabelValues("simulated").Inc()
			sandboxID := contracts.NewID("sandbox")
			w.emitStreamEvent(ctx, run, "run_sandbox_allocated", map[string]any{"run_id": run.ID, "sandbox_id": sandboxID, "mode": w.runMode, "simulated": true})
		}
	}
	w.emitStreamEvent(ctx, run, "run_started", map[string]any{"run_id": run.ID, "status": "running"})

	action = w.currentCancelAction(ctx, run.ID)
	if action == "rollback" {
		if err := w.rollbackRun(ctx, run.ID); err != nil {
			w.logger.Error("run_rollback_failed",
				"org_id", corr.OrgID,
				"project_id", corr.ProjectID,
				"deployment_id", corr.DeploymentID,
				"assistant_id", corr.AssistantID,
				"thread_id", corr.ThreadID,
				"run_id", corr.RunID,
				"error", err,
			)
			return
		}
		_ = w.updateCronExecutionStatus(ctx, run.ID, "rolled_back")
		w.emitStreamEvent(ctx, run, "run_rolled_back", map[string]any{"run_id": run.ID, "status": "rolled_back"})
		_ = w.redis.Del(ctx, streamBufferPrefix()+run.ID).Err()
		return
	}
	if action == "interrupt" {
		if err := w.updateRunResult(ctx, run.ID, contracts.RunStatusInterrupted, nil, nil); err != nil {
			w.logger.Error("run_interrupt_failed",
				"org_id", corr.OrgID,
				"project_id", corr.ProjectID,
				"deployment_id", corr.DeploymentID,
				"assistant_id", corr.AssistantID,
				"thread_id", corr.ThreadID,
				"run_id", corr.RunID,
				"error", err,
			)
			return
		}
		_ = w.updateCronExecutionStatus(ctx, run.ID, "interrupted")
		w.emitStreamEvent(ctx, run, "run_interrupted", map[string]any{"run_id": run.ID, "status": "interrupted"})
		if err := w.enqueueWebhookForRun(ctx, run.ID); err != nil {
			w.logger.Error("enqueue_webhook_failed",
				"org_id", corr.OrgID,
				"project_id", corr.ProjectID,
				"deployment_id", corr.DeploymentID,
				"assistant_id", corr.AssistantID,
				"thread_id", corr.ThreadID,
				"run_id", corr.RunID,
				"error", err,
			)
		}
		return
	}

	if w.executor == nil {
		result := executeResult{
			Status: contracts.RunStatusError,
			Error:  map[string]any{"message": "executor is not configured"},
		}
		_ = w.updateRunResult(ctx, run.ID, result.Status, result.Output, result.Error)
		_ = w.updateCronExecutionStatus(ctx, run.ID, "error")
		w.emitStreamEvent(ctx, run, "run_failed", map[string]any{"run_id": run.ID, "status": result.Status, "error": result.Error})
		_ = w.enqueueWebhookForRun(ctx, run.ID)
		return
	}
	result, execErr := w.executor.Execute(ctx, run)
	if execErr != nil {
		result.Status = contracts.RunStatusError
		result.Error = map[string]any{"message": execErr.Error()}
	}
	if result.Status == "" {
		result.Status = contracts.RunStatusSuccess
	}

	action = w.currentCancelAction(ctx, run.ID)
	if action == "rollback" {
		if err := w.rollbackRun(ctx, run.ID); err != nil {
			w.logger.Error("run_rollback_failed",
				"org_id", corr.OrgID,
				"project_id", corr.ProjectID,
				"deployment_id", corr.DeploymentID,
				"assistant_id", corr.AssistantID,
				"thread_id", corr.ThreadID,
				"run_id", corr.RunID,
				"error", err,
			)
			return
		}
		_ = w.updateCronExecutionStatus(ctx, run.ID, "rolled_back")
		w.emitStreamEvent(ctx, run, "run_rolled_back", map[string]any{"run_id": run.ID, "status": "rolled_back"})
		_ = w.redis.Del(ctx, streamBufferPrefix()+run.ID).Err()
		return
	}
	if action == "interrupt" {
		result.Status = contracts.RunStatusInterrupted
		result.Error = nil
	}

	for _, ev := range result.Events {
		name := strings.TrimSpace(ev.Name)
		if name == "" {
			continue
		}
		w.emitStreamEvent(ctx, run, name, ev.Payload)
	}

	if err := w.updateRunResult(ctx, run.ID, result.Status, result.Output, result.Error); err != nil {
		w.logger.Error("run_complete_failed",
			"org_id", corr.OrgID,
			"project_id", corr.ProjectID,
			"deployment_id", corr.DeploymentID,
			"assistant_id", corr.AssistantID,
			"thread_id", corr.ThreadID,
			"run_id", corr.RunID,
			"error", err,
		)
		return
	}
	_ = w.updateCronExecutionStatus(ctx, run.ID, string(result.Status))
	switch result.Status {
	case contracts.RunStatusInterrupted:
		w.emitStreamEvent(ctx, run, "run_interrupted", map[string]any{"run_id": run.ID, "status": "interrupted"})
	case contracts.RunStatusSuccess:
		w.emitStreamEvent(ctx, run, "run_completed", map[string]any{"run_id": run.ID, "status": "success"})
	default:
		w.emitStreamEvent(ctx, run, "run_failed", map[string]any{"run_id": run.ID, "status": result.Status, "error": result.Error})
	}
	if err := w.enqueueWebhookForRun(ctx, run.ID); err != nil {
		w.logger.Error("enqueue_webhook_failed",
			"org_id", corr.OrgID,
			"project_id", corr.ProjectID,
			"deployment_id", corr.DeploymentID,
			"assistant_id", corr.AssistantID,
			"thread_id", corr.ThreadID,
			"run_id", corr.RunID,
			"error", err,
		)
	}
}

func (w *Worker) fetchRunCorrelation(ctx context.Context, run pendingRun) runCorrelation {
	out := runCorrelation{
		AssistantID: run.AssistantID,
		ThreadID:    run.ThreadID,
		RunID:       run.ID,
	}
	if w.pg == nil {
		return out
	}
	var threadID sql.NullString
	_ = w.pg.QueryRow(ctx, `
		SELECT
			COALESCE(o.id, ''),
			COALESCE(p.id, ''),
			COALESCE(d.id, ''),
			COALESCE(r.assistant_id, ''),
			r.thread_id
		FROM runs r
		JOIN assistants a ON a.id = r.assistant_id
		JOIN deployments d ON d.id = a.deployment_id
		JOIN projects p ON p.id = d.project_id
		JOIN organizations o ON o.id = p.organization_id
		WHERE r.id=$1
	`, run.ID).Scan(&out.OrgID, &out.ProjectID, &out.DeploymentID, &out.AssistantID, &threadID)
	if threadID.Valid {
		out.ThreadID = threadID.String
	}
	return out
}

func (w *Worker) rollbackRun(ctx context.Context, runID string) error {
	tx, err := w.pg.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `DELETE FROM checkpoints WHERE run_id=$1`, runID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM runs WHERE id=$1`, runID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (w *Worker) updateRunStatus(ctx context.Context, runID string, status contracts.RunStatus) error {
	return w.updateRunResult(ctx, runID, status, nil, nil)
}

func (w *Worker) updateCronExecutionStatus(ctx context.Context, runID, status string) error {
	_, err := w.pg.Exec(ctx, `UPDATE cron_executions SET status=$2 WHERE run_id=$1`, runID, status)
	return err
}

func (w *Worker) updateRunResult(ctx context.Context, runID string, status contracts.RunStatus, output any, errPayload any) error {
	outputRaw, _ := json.Marshal(output)
	errorRaw, _ := json.Marshal(errPayload)
	_, err := w.pg.Exec(ctx, `
		UPDATE runs
		SET status=$2,
		    output_json=COALESCE(NULLIF($3::text,''), '{}')::jsonb,
		    error_json=COALESCE(NULLIF($4::text,''), '{}')::jsonb,
		    started_at=COALESCE(started_at, NOW()),
		    completed_at=CASE
		      WHEN $2 IN ('success','error','timeout','interrupted') THEN NOW()
		      ELSE completed_at
		    END,
		    updated_at=NOW()
		WHERE id=$1
	`, runID, status, string(outputRaw), string(errorRaw))
	return err
}

func (w *Worker) currentCancelAction(ctx context.Context, runID string) string {
	if w.redis == nil {
		return ""
	}
	action, err := w.redis.Get(ctx, cancelKeyPrefix()+runID).Result()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(action)
}

func (w *Worker) emitStreamEvent(ctx context.Context, run pendingRun, name string, payload any) {
	raw, _ := json.Marshal(payload)
	event := streamEvent{ID: w.nextEventID(ctx, run.ID), Event: name, Data: raw}
	eventBytes, _ := json.Marshal(event)
	_ = w.redis.Publish(ctx, streamChannelPrefix()+run.ID, eventBytes).Err()

	if run.StreamResumable {
		key := streamBufferPrefix() + run.ID
		_ = w.redis.RPush(ctx, key, eventBytes).Err()
		_ = w.redis.Expire(ctx, key, streamTTL()).Err()
	}
}

func (w *Worker) nextEventID(ctx context.Context, runID string) int64 {
	key := streamBufferPrefix() + runID
	count, err := w.redis.LLen(ctx, key).Result()
	if err != nil {
		return time.Now().UnixNano()
	}
	return count + 1
}

func (w *Worker) dispatchDueCrons(ctx context.Context, now time.Time) error {
	rows, err := w.pg.Query(ctx, `SELECT id, assistant_id, schedule FROM cron_jobs WHERE enabled=TRUE`)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var cronID, assistantID, schedule string
		if err := rows.Scan(&cronID, &assistantID, &schedule); err != nil {
			return err
		}
		if err := w.dispatchCronJob(ctx, cronID, assistantID, schedule, now); err != nil {
			w.logger.Error("dispatch_cron_job_failed", "cron_id", cronID, "error", err)
		}
	}
	return rows.Err()
}

func (w *Worker) dispatchCronJob(ctx context.Context, cronID, assistantID, schedule string, now time.Time) error {
	spec, err := cron.ParseStandard(schedule)
	if err != nil {
		return fmt.Errorf("invalid schedule %q: %w", schedule, err)
	}

	baseline := now.Add(-1 * time.Hour)
	var lastScheduled sql.NullTime
	err = w.pg.QueryRow(ctx, `SELECT MAX(scheduled_for) FROM cron_executions WHERE cron_job_id=$1`, cronID).Scan(&lastScheduled)
	if err != nil {
		return err
	}
	if lastScheduled.Valid {
		baseline = lastScheduled.Time
	}

	for range 12 {
		next := spec.Next(baseline)
		if next.After(now) {
			break
		}
		if err := w.createRunForCron(ctx, cronID, assistantID, next); err != nil {
			return err
		}
		baseline = next
	}
	return nil
}

func (w *Worker) createRunForCron(ctx context.Context, cronID, assistantID string, scheduledFor time.Time) error {
	tx, err := w.pg.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM cron_executions WHERE cron_job_id=$1 AND scheduled_for=$2)`, cronID, scheduledFor).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}

	runID := contracts.NewID("run")
	now := time.Now().UTC()
	_, err = tx.Exec(ctx, `
		INSERT INTO runs (id, thread_id, assistant_id, status, multitask_strategy, stream_resumable, created_at, updated_at)
		VALUES ($1, NULL, $2, 'pending', 'enqueue', FALSE, $3, $3)
	`, runID, assistantID, now)
	if err != nil {
		return err
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO cron_executions (id, cron_job_id, run_id, scheduled_for, status, created_at)
		VALUES ($1, $2, $3, $4, 'queued', NOW())
	`, contracts.NewID("cronexec"), cronID, runID, scheduledFor)
	if err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return err
	}
	return w.redis.LPush(ctx, w.cfg.QueueKey, "wake").Err()
}

func (w *Worker) enqueueWebhookForRun(ctx context.Context, runID string) error {
	var webhookURL sql.NullString
	var status string
	err := w.pg.QueryRow(ctx, `SELECT webhook_url, status FROM runs WHERE id=$1`, runID).Scan(&webhookURL, &status)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	}
	if !webhookURL.Valid || strings.TrimSpace(webhookURL.String) == "" {
		return nil
	}

	var deliveryExists bool
	err = w.pg.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM webhook_deliveries WHERE run_id=$1)`, runID).Scan(&deliveryExists)
	if err != nil {
		return err
	}
	if deliveryExists {
		return nil
	}

	_, err = w.pg.Exec(ctx, `
		INSERT INTO webhook_deliveries (id, run_id, url, status, attempts, last_error, created_at, updated_at)
		VALUES ($1,$2,$3,'pending',0,NULL,NOW(),NOW())
	`, contracts.NewID("wh"), runID, strings.TrimSpace(webhookURL.String))
	return err
}

func (w *Worker) processWebhookOutbox(ctx context.Context, now time.Time) error {
	rows, err := w.pg.Query(ctx, `
		SELECT id, run_id, url, attempts, status, updated_at
		FROM webhook_deliveries
		WHERE status IN ('pending','retry')
		ORDER BY created_at ASC
		LIMIT 50
	`)
	if err != nil {
		return err
	}
	defer rows.Close()

	items := make([]webhookDelivery, 0)
	for rows.Next() {
		var item webhookDelivery
		if err := rows.Scan(&item.ID, &item.RunID, &item.URL, &item.Attempts, &item.Status, &item.UpdatedAt); err != nil {
			return err
		}
		if item.Status == "retry" && now.Before(item.UpdatedAt.Add(backoffDuration(item.Attempts))) {
			continue
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for _, item := range items {
		if err := w.deliverWebhook(ctx, item, now); err != nil {
			w.logger.Error("deliver_webhook_failed", "delivery_id", item.ID, "error", err)
		}
	}
	return nil
}

func (w *Worker) deliverWebhook(ctx context.Context, item webhookDelivery, now time.Time) error {
	runStatus, _ := w.fetchRunStatus(ctx, item.RunID)
	payload := map[string]any{
		"run_id":    item.RunID,
		"status":    runStatus,
		"delivered": now.Format(time.RFC3339Nano),
	}
	raw, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, item.URL, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := w.httpClient.Do(req)
	if err == nil && resp != nil {
		defer resp.Body.Close()
	}

	nextAttempts := item.Attempts + 1
	if err == nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		webhookDeliveriesTotal.WithLabelValues("success").Inc()
		_, updateErr := w.pg.Exec(ctx, `
			UPDATE webhook_deliveries
			SET status='success', attempts=$2, last_error=NULL, updated_at=NOW()
			WHERE id=$1
		`, item.ID, nextAttempts)
		return updateErr
	}

	status := "retry"
	if nextAttempts >= w.webhookMaxAttempts {
		status = "dead_letter"
	}
	webhookDeliveriesTotal.WithLabelValues(status).Inc()
	lastErr := "webhook delivery failed"
	if err != nil {
		lastErr = err.Error()
	} else if resp != nil {
		lastErr = fmt.Sprintf("status %d", resp.StatusCode)
	}

	_, updateErr := w.pg.Exec(ctx, `
		UPDATE webhook_deliveries
		SET status=$2, attempts=$3, last_error=$4, updated_at=NOW()
		WHERE id=$1
	`, item.ID, status, nextAttempts, lastErr)
	return updateErr
}

func (w *Worker) fetchRunStatus(ctx context.Context, runID string) (string, error) {
	var status string
	err := w.pg.QueryRow(ctx, `SELECT status FROM runs WHERE id=$1`, runID).Scan(&status)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "rolled_back", nil
		}
		return "", err
	}
	return status, nil
}

func backoffDuration(attempts int) time.Duration {
	if attempts <= 0 {
		return 0
	}
	seconds := int(math.Pow(2, float64(attempts)))
	if seconds > 300 {
		seconds = 300
	}
	return time.Duration(seconds) * time.Second
}

func (w *Worker) observeOperationalHealth(ctx context.Context) {
	if w.pg != nil {
		var stuckCount int64
		if err := w.pg.QueryRow(ctx, `
			SELECT COUNT(1)
			FROM runs
			WHERE status='running' AND updated_at < NOW() - ($1::bigint * INTERVAL '1 second')
		`, int64(w.stuckRunThreshold.Seconds())).Scan(&stuckCount); err != nil {
			workerBackendUpGauge.WithLabelValues("postgres").Set(0)
		} else {
			workerBackendUpGauge.WithLabelValues("postgres").Set(1)
			workerStuckRunsGauge.Set(float64(stuckCount))
		}

		var deadLetters int64
		if err := w.pg.QueryRow(ctx, `SELECT COUNT(1) FROM webhook_deliveries WHERE status='dead_letter'`).Scan(&deadLetters); err == nil {
			webhookDeadLettersGauge.Set(float64(deadLetters))
		}
	}

	if w.redis != nil {
		if err := w.redis.Ping(ctx).Err(); err != nil {
			workerBackendUpGauge.WithLabelValues("redis").Set(0)
		} else {
			workerBackendUpGauge.WithLabelValues("redis").Set(1)
		}
	}
}

func cancelKeyPrefix() string {
	return envOrDefault("RUN_CANCEL_KEY_PREFIX", "langopen:run:cancel:")
}

func streamChannelPrefix() string {
	return envOrDefault("RUN_STREAM_CHANNEL_PREFIX", "langopen:run:stream:")
}

func streamBufferPrefix() string {
	return envOrDefault("RUN_STREAM_BUFFER_PREFIX", "langopen:run:events:")
}

func streamTTL() time.Duration {
	seconds := envIntOrDefault("RUN_STREAM_BUFFER_TTL_SECONDS", 3600)
	return time.Duration(seconds) * time.Second
}

func envOrDefault(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
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

func (w *Worker) allocateSandboxClaim(ctx context.Context, runID string) (string, error) {
	if w.dynamic == nil {
		return "", errors.New("dynamic kubernetes client unavailable")
	}
	name := sandboxClaimName(runID)
	resource := schema.GroupVersionResource{
		Group:    "extensions.agents.x-k8s.io",
		Version:  "v1alpha1",
		Resource: "sandboxclaims",
	}
	obj := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "extensions.agents.x-k8s.io/v1alpha1",
			"kind":       "SandboxClaim",
			"metadata": map[string]any{
				"name": name,
			},
			"spec": map[string]any{
				"sandboxTemplateRef": map[string]any{
					"name": w.sandboxTemplate,
				},
			},
		},
	}
	_, err := w.dynamic.Resource(resource).Namespace(w.sandboxNamespace).Create(ctx, obj, metav1.CreateOptions{})
	if err != nil && !k8serrors.IsAlreadyExists(err) {
		return "", err
	}
	return name, nil
}

func sandboxClaimName(runID string) string {
	base := strings.ToLower(runID)
	base = strings.ReplaceAll(base, "_", "-")
	var b strings.Builder
	for _, ch := range base {
		if (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '-' {
			b.WriteRune(ch)
		}
	}
	clean := strings.Trim(b.String(), "-")
	if clean == "" {
		clean = contracts.NewID("sandbox")
		clean = strings.ReplaceAll(strings.ToLower(clean), "_", "-")
	}
	name := "run-" + clean
	if len(name) > 63 {
		name = name[:63]
		name = strings.TrimRight(name, "-")
	}
	return name
}

func (w *Worker) observeQueueDepth(ctx context.Context) {
	if w.redis == nil {
		return
	}
	depth, err := w.redis.LLen(ctx, w.cfg.QueueKey).Result()
	if err != nil {
		return
	}
	workerQueueDepthGauge.Set(float64(depth))
}

func initWorkerMetrics() {
	workerMetricsOnce.Do(func() {
		workerQueueDepthGauge = prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "langopen_worker_queue_depth",
			Help: "Current pending wakeup queue depth observed by worker.",
		})
		workerActiveRunsGauge = prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "langopen_worker_active_runs",
			Help: "Number of currently executing runs in worker process.",
		})
		sandboxAllocationsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "langopen_worker_sandbox_allocations_total",
			Help: "Total sandbox allocation attempts by outcome.",
		}, []string{"result"})
		workerStuckRunsGauge = prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "langopen_worker_stuck_runs",
			Help: "Count of running runs exceeding stuck threshold.",
		})
		webhookDeadLettersGauge = prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "langopen_worker_webhook_dead_letters",
			Help: "Count of webhook deliveries in dead_letter status.",
		})
		workerBackendUpGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "langopen_worker_backend_up",
			Help: "Backend connectivity status by backend (1=up, 0=down).",
		}, []string{"backend"})
		webhookDeliveriesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "langopen_worker_webhook_deliveries_total",
			Help: "Total webhook delivery attempts by resulting state.",
		}, []string{"result"})
		prometheus.MustRegister(
			workerQueueDepthGauge,
			workerActiveRunsGauge,
			sandboxAllocationsTotal,
			workerStuckRunsGauge,
			webhookDeadLettersGauge,
			workerBackendUpGauge,
			webhookDeliveriesTotal,
		)
	})
}
