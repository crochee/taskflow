package workflow

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"math"
	"reflect"
	"runtime"
	"sync"
	"time"

	"github.com/cenkalti/backoff/v4"
	uuid "github.com/satori/go.uuid"
	"go.uber.org/multierr"
)

func NewFuncTask(f func(context.Context) error) Task {
	return FuncTask(f)
}

type FuncTask func(context.Context) error

func (f FuncTask) ID() string {
	h := md5.New()
	_, _ = fmt.Fprint(h, f.Name())
	return hex.EncodeToString(h.Sum(nil))
}

func (f FuncTask) Name() string {
	return runtime.FuncForPC(reflect.ValueOf(f).Pointer()).Name()
}

func (f FuncTask) Commit(ctx context.Context) error {
	return f(ctx)
}

func (f FuncTask) Rollback(context.Context) error {
	return nil
}

type recoverTask struct {
	Task
}

func SafeTask(t Task) Task {
	return &recoverTask{
		Task: t,
	}
}

func (rt *recoverTask) Commit(ctx context.Context) (err error) {
	defer func() {
		if r := recover(); r != nil {
			const size = 64 << 10
			buf := make([]byte, size)
			buf = buf[:runtime.Stack(buf, false)]
			err = multierr.Append(err, fmt.Errorf("[Recover] found:%v,trace:\n%s", r, buf))
		}
	}()
	err = rt.Task.Commit(ctx)
	return
}

func (rt *recoverTask) Rollback(ctx context.Context) (err error) {
	defer func() {
		if r := recover(); r != nil {
			const size = 64 << 10
			buf := make([]byte, size)
			buf = buf[:runtime.Stack(buf, false)]
			err = multierr.Append(err, fmt.Errorf("[Recover] found:%v,trace:\n%s", r, buf))
		}
	}()
	err = rt.Task.Rollback(ctx)
	return
}

const (
	PolicyRetry Policy = 1 + iota
	PolicyRevert
)

type retryTask struct {
	Task
	attempts int
	interval time.Duration
	policy   Policy
}

func RetryTask(t Task, opts ...Option) Task {
	o := &option{
		policy: PolicyRetry,
	}
	for _, opt := range opts {
		opt(o)
	}
	return &retryTask{
		Task:     t,
		attempts: o.attempt,
		interval: o.interval,
		policy:   o.policy,
	}
}

func (rt *retryTask) Commit(ctx context.Context) error {
	err := rt.Task.Commit(ctx)
	if err == nil {
		return nil
	}
	if rt.policy == PolicyRetry {
		var tempAttempts int
		backOff := rt.newBackOff() // 退避算法 保证时间间隔为指数级增长
		currentInterval := 0 * time.Millisecond
		timer := time.NewTimer(currentInterval)
		for {
			select {
			case <-timer.C:
				shouldRetry := tempAttempts < rt.attempts
				if !shouldRetry {
					timer.Stop()
					return err
				}
				if retryErr := rt.Task.Commit(ctx); retryErr == nil {
					shouldRetry = false
				} else {
					err = multierr.Append(err, fmt.Errorf("%d try,%w", tempAttempts+1, retryErr))
				}
				if !shouldRetry {
					timer.Stop()
					return err
				}
				// 计算下一次
				currentInterval = backOff.NextBackOff()
				tempAttempts++
				// 定时器重置
				timer.Reset(currentInterval)
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			}
		}
	}
	return err
}

func (rt *retryTask) newBackOff() backoff.BackOff {
	if rt.attempts < 2 || rt.interval <= 0 {
		return &backoff.ZeroBackOff{}
	}

	b := backoff.NewExponentialBackOff()
	b.InitialInterval = rt.interval

	// calculate the multiplier for the given number of attempts
	// so that applying the multiplier for the given number of attempts will not exceed 2 times the initial interval
	// it allows to control the progression along the attempts
	b.Multiplier = math.Pow(2, 1/float64(rt.attempts-1))

	// according to docs, b.Reset() must be called before using
	b.Reset()
	return b
}

type parallelTask struct {
	id   string
	name string

	executedTasks []Task
	mutex         sync.Mutex

	tasks []Task

	errOnce sync.Once
	err     error
}

func ParallelTask(opts ...Option) Task {
	uid := uuid.NewV1()
	uidStr := hex.EncodeToString(uid[:])
	o := &option{
		name:  "parallel-task-" + uidStr,
		tasks: make([]Task, 0),
	}
	for _, opt := range opts {
		opt(o)
	}
	return &parallelTask{
		id:    uidStr,
		name:  o.name,
		tasks: o.tasks,
	}
}

func (s *parallelTask) ID() string {
	return s.id
}

func (s *parallelTask) Name() string {
	return s.name
}

func (s *parallelTask) Commit(ctx context.Context) error {
	newCtx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	for _, task := range s.tasks {
		wg.Add(1)
		go func(ctx context.Context, wg *sync.WaitGroup, cancel context.CancelFunc, t Task) {
			select {
			case <-ctx.Done():
			default:
				if err := t.Commit(ctx); err != nil {
					s.errOnce.Do(func() {
						s.err = err
						cancel()
					})
				}
				s.mutex.Lock()
				s.executedTasks = append(s.executedTasks, t)
				s.mutex.Unlock()
			}
			wg.Done()
		}(newCtx, &wg, cancel, task)
	}
	wg.Wait()
	cancel()
	return s.err
}

func (s *parallelTask) Rollback(ctx context.Context) error {
	s.err = nil
	var wg sync.WaitGroup
	for _, task := range s.executedTasks {
		wg.Add(1)
		go func(ctx context.Context, wg *sync.WaitGroup, t Task) {
			var err error
			select {
			case <-ctx.Done():
				err = ctx.Err()
			default:
				err = t.Rollback(ctx)
			}
			s.mutex.Lock()
			s.err = multierr.Append(s.err, err)
			s.mutex.Unlock()
			wg.Done()
		}(ctx, &wg, task)
	}
	wg.Wait()
	return s.err
}

type pipelineTask struct {
	id   string
	name string

	tasks []Task
	cur   int
}

func PipelineTask(opts ...Option) Task {
	uid := uuid.NewV1()
	uidStr := hex.EncodeToString(uid[:])
	o := &option{
		name:  "pipeline-task-" + uidStr,
		tasks: make([]Task, 0),
	}
	for _, opt := range opts {
		opt(o)
	}
	return &pipelineTask{
		id:    uidStr,
		name:  o.name,
		tasks: o.tasks,
	}
}

func (s *pipelineTask) ID() string {
	return s.id
}

func (s *pipelineTask) Name() string {
	return s.name
}

func (s *pipelineTask) Commit(ctx context.Context) error {
	for index, task := range s.tasks {
		if err := task.Commit(ctx); err != nil {
			s.cur = index
			return err
		}
	}
	return nil
}

func (s *pipelineTask) Rollback(ctx context.Context) error {
	var err error
	for i := s.cur; i >= 0; i-- {
		err = multierr.Append(err, s.tasks[i].Rollback(ctx))
	}
	return err
}
