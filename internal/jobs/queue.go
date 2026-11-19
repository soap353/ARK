package jobs

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// QueueConfig configures concurrency semaphores for job types.
type QueueConfig struct {
	MaxDownloads int // default: 2
	MaxEncodes   int // default: 1
	MaxIndexes   int // default: 1
}

// DefaultQueueConfig returns default ponytail semaphore limits.
func DefaultQueueConfig() QueueConfig {
	return QueueConfig{
		MaxDownloads: 3,
		MaxEncodes:   1,
		MaxIndexes:   1,
	}
}

// QueueManager manages the job queue lifecycle and semaphore dispatching.
type QueueManager struct {
	store *JobStore

	downloadSem chan struct{}
	encodeSem   chan struct{}
	indexSem    chan struct{}

	runners       map[string]Runner
	activeCancels map[string]context.CancelFunc

	notifyCh chan struct{}
	stopCh   chan struct{}
	wg       sync.WaitGroup

	mu      sync.RWMutex
	running bool
}

// NewQueueManager creates a new QueueManager with configured worker semaphores.
func NewQueueManager(store *JobStore, cfg QueueConfig) *QueueManager {
	if cfg.MaxDownloads <= 0 {
		cfg.MaxDownloads = 2
	}
	if cfg.MaxEncodes <= 0 {
		cfg.MaxEncodes = 1
	}
	if cfg.MaxIndexes <= 0 {
		cfg.MaxIndexes = 1
	}

	aria2 := NewAria2cRunner("")
	zimit := NewZimitRunner("")
	ytdlp := NewYtDlpRunner("")

	qm := &QueueManager{
		store:         store,
		downloadSem:   make(chan struct{}, cfg.MaxDownloads),
		encodeSem:     make(chan struct{}, cfg.MaxEncodes),
		indexSem:      make(chan struct{}, cfg.MaxIndexes),
		runners:       make(map[string]Runner),
		activeCancels: make(map[string]context.CancelFunc),
		notifyCh:      make(chan struct{}, 10),
		stopCh:        make(chan struct{}),
	}

	// Register default runners
	qm.RegisterRunner(TypeDownload, aria2)
	qm.RegisterRunner(TypePreset, aria2)
	qm.RegisterRunner(TypeZim, zimit)
	qm.RegisterRunner(TypeEncode, ytdlp)
	qm.RegisterRunner(TypeDrugLabels, NewDrugLabelsRunner())
	qm.RegisterRunner(TypeMapExtract, NewMapExtractRunner())

	return qm
}

// RegisterRunner attaches a custom runner for a specific job type.
func (qm *QueueManager) RegisterRunner(jobType string, runner Runner) {
	qm.mu.Lock()
	defer qm.mu.Unlock()
	qm.runners[jobType] = runner
}

// Start begins processing pending jobs in background goroutines.
func (qm *QueueManager) Start(ctx context.Context) {
	qm.mu.Lock()
	if qm.running {
		qm.mu.Unlock()
		return
	}
	qm.running = true
	qm.mu.Unlock()

	qm.wg.Add(1)
	go qm.dispatchLoop(ctx)
}

// Stop stops the dispatch loop and waits for running jobs to finish.
func (qm *QueueManager) Stop() {
	qm.mu.Lock()
	if !qm.running {
		qm.mu.Unlock()
		return
	}
	qm.running = false
	close(qm.stopCh)
	qm.mu.Unlock()

	qm.wg.Wait()
}

// Submit schedules a new job into the queue.
func (qm *QueueManager) Submit(ctx context.Context, job *Job) error {
	if err := qm.store.Create(ctx, job); err != nil {
		return err
	}
	qm.notify()
	return nil
}

// Notify wakes the dispatch loop to check for pending jobs.
func (qm *QueueManager) Notify() {
	qm.notify()
}

// Cancel cancels a pending or running job.
func (qm *QueueManager) Cancel(ctx context.Context, jobID string) error {
	qm.mu.Lock()
	cancel, isRunning := qm.activeCancels[jobID]
	qm.mu.Unlock()

	if isRunning && cancel != nil {
		cancel()
	}

	if err := qm.store.SetStatus(ctx, jobID, StatusCancelled, "Cancelled by user"); err != nil {
		return err
	}
	return nil
}

// Retry resets a failed or cancelled job back to pending and wakes the dispatcher.
func (qm *QueueManager) Retry(ctx context.Context, jobID string) error {
	_ = qm.store.UpdateProgress(ctx, jobID, 0.0, "Queued for retry", 0, 0, 0)
	if err := qm.store.SetStatus(ctx, jobID, StatusPending, ""); err != nil {
		return err
	}
	qm.notify()
	return nil
}

// Remove cancels the job if running and deletes it from the database.
func (qm *QueueManager) Remove(ctx context.Context, jobID string) error {
	qm.mu.Lock()
	cancel, isRunning := qm.activeCancels[jobID]
	qm.mu.Unlock()

	if isRunning && cancel != nil {
		cancel()
	}

	return qm.store.Delete(ctx, jobID)
}

func (qm *QueueManager) notify() {
	select {
	case qm.notifyCh <- struct{}{}:
	default:
	}
}

func (qm *QueueManager) dispatchLoop(ctx context.Context) {
	defer qm.wg.Done()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-qm.stopCh:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			qm.tryDispatch(ctx)
		case <-qm.notifyCh:
			qm.tryDispatch(ctx)
		}
	}
}

func (qm *QueueManager) tryDispatch(ctx context.Context) {
	// 1. Fill available download slots
downloadLoop:
	for {
		select {
		case qm.downloadSem <- struct{}{}:
			job, err := qm.store.AcquireNextPending(ctx, []string{TypeDownload, TypePreset, TypeDrugLabels, TypeMapExtract, TypeOllamaPull})
			if err != nil || job == nil {
				<-qm.downloadSem
				break downloadLoop
			}
			qm.wg.Add(1)
			go qm.runWorker(ctx, job, qm.downloadSem)
		default:
			break downloadLoop
		}
	}

	// 2. Fill available encode slots
encodeLoop:
	for {
		select {
		case qm.encodeSem <- struct{}{}:
			job, err := qm.store.AcquireNextPending(ctx, []string{TypeEncode})
			if err != nil || job == nil {
				<-qm.encodeSem
				break encodeLoop
			}
			qm.wg.Add(1)
			go qm.runWorker(ctx, job, qm.encodeSem)
		default:
			break encodeLoop
		}
	}

	// 3. Fill available index / ZIM / USB export slots
indexLoop:
	for {
		select {
		case qm.indexSem <- struct{}{}:
			job, err := qm.store.AcquireNextPending(ctx, []string{TypeIndex, TypeZim, TypeUSB})
			if err != nil || job == nil {
				<-qm.indexSem
				break indexLoop
			}
			qm.wg.Add(1)
			go qm.runWorker(ctx, job, qm.indexSem)
		default:
			break indexLoop
		}
	}
}

func (qm *QueueManager) runWorker(ctx context.Context, job *Job, sem chan struct{}) {
	jobCtx, cancel := context.WithCancel(ctx)
	qm.mu.Lock()
	qm.activeCancels[job.ID] = cancel
	qm.mu.Unlock()

	defer func() {
		cancel()
		qm.mu.Lock()
		delete(qm.activeCancels, job.ID)
		qm.mu.Unlock()
		<-sem
		qm.wg.Done()
		qm.notify() // Check if another job can immediately take the freed slot
	}()

	qm.mu.RLock()
	runner, exists := qm.runners[job.Type]
	qm.mu.RUnlock()

	if !exists || runner == nil {
		_ = qm.store.SetStatus(ctx, job.ID, StatusFailed, fmt.Sprintf("no runner registered for job type %s", job.Type))
		return
	}

	progressFunc := func(progress float64, msg string, bytesDown, totalBytes int64, eta int) {
		_ = qm.store.UpdateProgress(ctx, job.ID, progress, msg, bytesDown, totalBytes, eta)
	}

	err := runner.Run(jobCtx, job, progressFunc)
	if err != nil {
		if errors.Is(err, context.Canceled) || jobCtx.Err() != nil {
			_ = qm.store.SetStatus(ctx, job.ID, StatusCancelled, "Cancelled by user")
			return
		}
		if cur, getErr := qm.store.Get(ctx, job.ID); getErr == nil && cur.Status == StatusCancelled {
			return
		}
		_ = qm.store.SetStatus(ctx, job.ID, StatusFailed, err.Error())
	} else {
		_ = qm.store.UpdateProgress(ctx, job.ID, 100.0, "Completed", job.TotalBytes, job.TotalBytes, 0)
		_ = qm.store.SetStatus(ctx, job.ID, StatusDone, "")
	}
}
