// Package tenant 实现租户的开通和删除流程，CLI 和 API 共用。
package tenant

import (
	"context"
	"fmt"
)

// Step 是开通流程中的一步。
// 进度全靠查集群和 Docker 判断，不存状态文件，所以重跑时 Check 为 true 的步骤会被跳过。
type Step interface {
	Name() string
	// Check 报告这一步是否已经完成。必须只读。
	Check(ctx context.Context) (bool, error)
	// Apply 执行这一步。必须幂等。
	Apply(ctx context.Context) error
	// Delete 撤销这一步，资源不存在时返回 nil。
	Delete(ctx context.Context) error
}

// Reporter 接收每一步的进度，CLI 打印出来，API 记进任务状态。
type Reporter func(step string, event Event)

// Event 是一步的执行结果。
type Event string

const (
	EventApplying Event = "applying"
	EventSkipped  Event = "skipped" // Check 已为 true
	EventApplied  Event = "applied"
	EventDeleted  Event = "deleted"
)

// Create 按顺序执行 steps，跳过已完成的步骤。
// Apply 之后再 Check 一次，防止 Apply 没报错但实际没生效。
func Create(ctx context.Context, steps []Step, report Reporter) error {
	for _, s := range steps {
		done, err := s.Check(ctx)
		if err != nil {
			return fmt.Errorf("%s: check: %w", s.Name(), err)
		}
		if done {
			report(s.Name(), EventSkipped)
			continue
		}
		report(s.Name(), EventApplying)
		if err := s.Apply(ctx); err != nil {
			return fmt.Errorf("%s: apply: %w", s.Name(), err)
		}
		done, err = s.Check(ctx)
		if err != nil {
			return fmt.Errorf("%s: check after apply: %w", s.Name(), err)
		}
		if !done {
			return fmt.Errorf("%s: apply succeeded but check still false", s.Name())
		}
		report(s.Name(), EventApplied)
	}
	return nil
}

// Delete 按相反顺序撤销 steps。某一步失败时立即停止，避免在依赖还在时删掉前面的资源。
func Delete(ctx context.Context, steps []Step, report Reporter) error {
	for i := len(steps) - 1; i >= 0; i-- {
		s := steps[i]
		if err := s.Delete(ctx); err != nil {
			return fmt.Errorf("%s: delete: %w", s.Name(), err)
		}
		report(s.Name(), EventDeleted)
	}
	return nil
}
