package engine

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/redis/go-redis/v9"
	"github.com/robfig/cron/v3"
	"langopen.dev/pkg/contracts"
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
	cronTickInterval    time.Duration
	webhookPollInterval time.Duration
	webhookMaxAttempts  int
}

type pendingRun struct {
	ID              string
	ThreadID        string
	AssistantID     string
	StreamResumable bool
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

func New(cfg Config, logger *slog.Logger) (*Worker, error) {
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
		httpClient: &http.Client{
			Timeout: time.Duration(envIntOrDefault("WEBHOOK_TIMEOUT_SECONDS", 5)) * time.Second,
		},
		cronTickInterval:    time.Duration(envIntOrDefault("CRON_TICK_SECONDS", 30)) * time.Second,
		webhookPollInterval: time.Duration(envIntOrDefault("WEBHOOK_POLL_SECONDS", 5)) * time.Second,
		webhookMaxAttempts:  envIntOrDefault("WEBHOOK_MAX_ATTEMPTS", 6),
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
				continue
			}
			if errors.Is(err, context.Canceled) {
				return nil
			}
			w.logger.Error("wakeup_pop_failed", "error", err)
			continue
		}
		if len(result) < 2 {
			continue
		}

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
	err = tx.QueryRow(ctx, `
		SELECT id, COALESCE(thread_id,''), assistant_id, stream_resumable
		FROM runs
		WHERE status='pending'
		ORDER BY created_at ASC
		FOR UPDATE SKIP LOCKED
		LIMIT 1
	`).Scan(&run.ID, &run.ThreadID, &run.AssistantID, &run.StreamResumable)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return pendingRun{}, sql.ErrNoRows
		}
		return pendingRun{}, err
	}

	_, err = tx.Exec(ctx, `UPDATE runs SET status='running', updated_at=NOW() WHERE id=$1`, run.ID)
	if err != nil {
		return pendingRun{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return pendingRun{}, err
	}
	return run, nil
}

func (w *Worker) executeRun(ctx context.Context, run pendingRun) {
	w.logger.Info("run_started", "run_id", run.ID, "thread_id", run.ThreadID)
	if w.runMode == "mode_b" {
		sandboxID := contracts.NewID("sandbox")
		w.emitStreamEvent(ctx, run, "run_sandbox_allocated", map[string]any{"run_id": run.ID, "sandbox_id": sandboxID, "mode": w.runMode})
	}
	w.emitStreamEvent(ctx, run, "run_started", map[string]any{"run_id": run.ID, "status": "running"})

	time.Sleep(200 * time.Millisecond)
	action, _ := w.redis.Get(ctx, cancelKeyPrefix()+run.ID).Result()
	action = strings.TrimSpace(action)

	switch action {
	case "rollback":
		if err := w.rollbackRun(ctx, run.ID); err != nil {
			w.logger.Error("run_rollback_failed", "run_id", run.ID, "error", err)
			return
		}
		w.emitStreamEvent(ctx, run, "run_rolled_back", map[string]any{"run_id": run.ID, "status": "rolled_back"})
		_ = w.redis.Del(ctx, streamBufferPrefix()+run.ID).Err()
	case "interrupt":
		if err := w.updateRunStatus(ctx, run.ID, contracts.RunStatusInterrupted); err != nil {
			w.logger.Error("run_interrupt_failed", "run_id", run.ID, "error", err)
			return
		}
		w.emitStreamEvent(ctx, run, "run_interrupted", map[string]any{"run_id": run.ID, "status": "interrupted"})
		if err := w.enqueueWebhookForRun(ctx, run.ID); err != nil {
			w.logger.Error("enqueue_webhook_failed", "run_id", run.ID, "error", err)
		}
	default:
		if err := w.updateRunStatus(ctx, run.ID, contracts.RunStatusSuccess); err != nil {
			w.logger.Error("run_complete_failed", "run_id", run.ID, "error", err)
			return
		}
		w.emitStreamEvent(ctx, run, "token", map[string]any{"token": "hello"})
		w.emitStreamEvent(ctx, run, "run_completed", map[string]any{"run_id": run.ID, "status": "success"})
		if err := w.enqueueWebhookForRun(ctx, run.ID); err != nil {
			w.logger.Error("enqueue_webhook_failed", "run_id", run.ID, "error", err)
		}
	}

	_ = w.redis.Del(ctx, cancelKeyPrefix()+run.ID).Err()
	w.logger.Info("run_finished", "run_id", run.ID, "action", action)
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
	_, err := w.pg.Exec(ctx, `UPDATE runs SET status=$2, updated_at=NOW() WHERE id=$1`, runID, status)
	return err
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
		_, updateErr := w.pg.Exec(ctx, `
			UPDATE webhook_deliveries
			SET status='success', attempts=$2, last_error=NULL, updated_at=NOW()
			WHERE id=$1
		`, item.ID, nextAttempts)
		return updateErr
	}

	status := "retry"
	if nextAttempts >= w.webhookMaxAttempts {
		status = "failed"
	}
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
