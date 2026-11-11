// batchlock.go — 写工具的批处理独占打开调度（file_write / file_replace / file_patch 共用）。
package filetool

import "time"

// batchRetryDelays 是第一遍未拿到独占句柄后的重试间隔。
// 语义：先按入参顺序把所有文件各尝试一次；共享/锁冲突记 pending、继续后面的文件；
// 全部文件处理完后，对仍 pending 的文件按 50ms / 100ms / 200ms 做 3 轮重试，
// 每轮重试所有仍 pending 的文件。3 轮后仍 pending → 该文件单独失败。
var batchRetryDelays = []time.Duration{
	50 * time.Millisecond,
	100 * time.Millisecond,
	200 * time.Millisecond,
}

// batchJob 描述一个文件（file_write 的 winner，或 replace/patch 的同路径分组）的调度单元。
//
//   - indices：该文件在 results 中的下标集合（仅用于记录/自检；填充由 try/fillInUse 负责）。
//   - try：单次尝试（独占打开 + 该文件的全部工作）。成功或 fatal 错误时自行按索引填
//     results 并返回 false；只有 errFileInUse / 共享冲突返回 true（不填结果，进入 pending）。
//   - fillInUse：3 轮重试后仍 pending 时填该文件的失败结果（write 只填 winner；
//     replace/patch 填同组所有项，code=FILE_IN_USE）。
type batchJob struct {
	indices   []int
	try       func() (inUse bool)
	fillInUse func()
}

// runBatchJobs 执行批调度。results 按入参顺序预分配，所有填充都按索引进行，
// 重试不会打乱结果顺序；每个 job 内部处理完立即关闭句柄（同一时刻最多一个文件句柄）。
func runBatchJobs(results []map[string]any, jobs []batchJob) {
	if len(jobs) == 0 {
		return
	}
	_ = results // results 只用于调用方按索引填充；保留参数以统一调用形态。

	pending := make([]int, 0, len(jobs))
	for i := range jobs {
		if jobs[i].try() {
			pending = append(pending, i)
		}
	}
	for _, delay := range batchRetryDelays {
		if len(pending) == 0 {
			return
		}
		time.Sleep(delay)
		still := pending[:0]
		for _, idx := range pending {
			if jobs[idx].try() {
				still = append(still, idx)
			}
		}
		pending = still
	}
	for _, idx := range pending {
		jobs[idx].fillInUse()
	}
}
