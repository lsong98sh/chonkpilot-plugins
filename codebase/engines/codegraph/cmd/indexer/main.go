// chonkpilot-codegraph-indexer：常驻 codegraph 增量索引器（P5）。
//
// **单实例多 workdir**（2026-10-19）：每个宿主进程只跑一个 indexer 进程（GUI 子系统 +
// 托盘图标，启动隐藏；单击托盘查看执行输出，关闭窗口 = 收起回托盘）。单实例判定 =
// **注册全局窗口类**（同名类已被占用 → 直接退出，不再依赖文件锁）。项目发现经
// `plugin.index.workdirs`（宿主活实例的去重 work_dir），随后按项目并发（5 个工作线程，
// 每线程独立引擎进程）读取待索引队列：
//
//	plugin.index.workdirs / pending / take / ack
//
// 每个项目的索引库不同，故**按 workdir 加锁**（`<workdir>/.chonkpilot/codegraph/indexer.lock`），
// 不设进程级全局锁：同一项目被其它进程占用 → 本轮跳过，不影响其它项目并行索引。
//
// 取到一批变更 → 合并同路径变更 → 调用 codegraph 引擎（MCP stdio）：
// 首次（state 为空 / not_initialized）走 codegraph_initialize 全量，
// 之后走 codegraph_index_paths 按路径增量；回执队列并更新该项目 status.json。
//
// 环境变量（均由宿主注入）：
//
//	CHONKPILOT_PLUGIN_ADDR      必填，插件桥帧地址（UDS 路径 / tcp://host:port）
//	CHONKPILOT_PLUGIN_ID        缺省 codegraph-indexer（单实例固定 id）
//	CHONKPILOT_CODEGRAPH_ENGINE 缺省 <本 exe 同目录>/chonkpilot-codegraph-mcp-server.exe
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	cg "github.com/lsong98sh/chonkpilot-plugins/codebase/engines/codegraph/server"
	"github.com/lsong98sh/chonkpilot-plugins/shared/indexkind"
	"github.com/lsong98sh/chonkpilot-plugins/shared/lockfile"
	"github.com/lsong98sh/chonkpilot-plugins/shared/pluginclient"
	"github.com/lsong98sh/chonkpilot-plugins/shared/trayconsole"
	"github.com/lsong98sh/chonkpilot-plugins/shared/winproc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	batchLimit         = 200
	idlePoll           = 500 * time.Millisecond
	dispatchPoll       = 100 * time.Millisecond
	readyWriteInterval = 30 * time.Second
	defaultEngineExe   = "chonkpilot-codegraph-mcp-server.exe"
	// maxWorkers 是并发处理 workdir 的工作线程数（每线程独立引擎进程）。
	maxWorkers = 5
	// engineIdleClose 是工作线程空闲多久后释放引擎（避免长期持有索引库 LOCK）。
	engineIdleClose = 5 * time.Second
	// trayClassName 是托盘窗口类名（全局注册 → 跨进程单实例判定）。
	trayClassName = "ChonkPilotTrayConsole-codegraph-indexer"
)

// config 是 indexer 的启动配置（环境变量解析结果；与 work_dir 无关）。
type config struct {
	pluginID   string
	pluginAddr string
	engineExe  string
}

func main() {
	ui, err := startConsole()
	if errors.Is(err, trayconsole.ErrAlreadyRunning) {
		// 单实例判定命中（同名窗口类已被其它进程注册）→ 正常退出（监管器不再拉起）。
		fmt.Fprintln(os.Stderr, "codegraph-indexer: 已有实例在运行，退出")
		os.Exit(0)
	}
	defer func() {
		if ui != nil {
			ui.Close()
		}
	}()
	cfg, err := loadConfig()
	if err != nil {
		slog.Error("codegraph-indexer: 配置错误", "err", err)
		os.Exit(2)
	}
	if err := run(context.Background(), cfg); err != nil {
		slog.Error("codegraph-indexer: 退出", "err", err)
		os.Exit(1)
	}
}

// startConsole 建托盘 + 输出窗口（启动隐藏），并把 slog 双写到 stderr 与窗口。
// 托盘不可用（非 Windows / Shell_NotifyIcon 失败）→ 回落为仅 stderr，不阻断索引。
// 返回 ErrAlreadyRunning 表示已有同名实例（调用方据此静默退出）。
func startConsole() (*trayconsole.Console, error) {
	ui, err := trayconsole.Start(trayconsole.Options{
		Title:          "ChonkPilot · codegraph 索引器（输出）",
		Tooltip:        "ChonkPilot codegraph 索引器 — 点击查看输出",
		ClassName:      trayClassName,
		SingleInstance: true,
		Header: []string{
			fmt.Sprintf("ChonkPilot codegraph 索引器启动（pid=%d，单实例多 workdir）", os.Getpid()),
		},
	})
	if errors.Is(err, trayconsole.ErrAlreadyRunning) {
		return nil, err
	}
	var w io.Writer = os.Stderr
	if err != nil {
		slog.Warn("codegraph-indexer: 托盘窗口不可用（仅 stderr）", "err", err)
		ui = nil
	} else {
		w = io.MultiWriter(os.Stderr, ui)
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo})))
	return ui, nil
}

// loadConfig 解析环境变量（缺省值见文件头）。
func loadConfig() (config, error) {
	addr := strings.TrimSpace(os.Getenv("CHONKPILOT_PLUGIN_ADDR"))
	if addr == "" {
		return config{}, errors.New("CHONKPILOT_PLUGIN_ADDR 不能为空")
	}
	id := strings.TrimSpace(os.Getenv("CHONKPILOT_PLUGIN_ID"))
	if id == "" {
		id = "codegraph-indexer"
	}
	engine := strings.TrimSpace(os.Getenv("CHONKPILOT_CODEGRAPH_ENGINE"))
	if engine == "" {
		exe, err := os.Executable()
		if err != nil {
			return config{}, fmt.Errorf("定位本进程 exe 失败: %w", err)
		}
		engine = filepath.Join(filepath.Dir(exe), defaultEngineExe)
	}
	return config{pluginID: id, pluginAddr: addr, engineExe: engine}, nil
}

// run 连接插件桥、等待 server-ready 后进入主循环。
func run(parent context.Context, cfg config) error {
	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer stop()

	slog.Info("codegraph-indexer: 连接插件桥", "addr", cfg.pluginAddr, "engine", cfg.engineExe)
	cli, err := pluginclient.Dial(ctx, pluginclient.Options{
		Addr:     cfg.pluginAddr,
		PluginID: cfg.pluginID,
		Name:     "codegraph-indexer",
		Version:  "0.1.0",
		Features: map[string]any{"indexer": true},
	})
	if err != nil {
		return fmt.Errorf("连接插件桥: %w", err)
	}
	defer cli.Close()

	ready := make(chan struct{})
	shutdown := make(chan struct{})
	go watchEvents(cli, ready, shutdown)

	select {
	case <-ready:
		slog.Info("codegraph-indexer: 宿主就绪，开始消费待索引队列")
	case <-shutdown:
		return nil // 未就绪即收到关闭 / 连接断开
	case <-ctx.Done():
		return nil
	}

	return serve(ctx, cli, cfg.engineExe, shutdown)
}

// watchEvents 把 server-ready / plugin.shutdown（含连接断开）转成通道信号。
func watchEvents(cli pluginclient.Client, ready, shutdown chan struct{}) {
	var readyOnce, shutdownOnce sync.Once
	for ev := range cli.Events() {
		switch {
		case ev.Name == "server-ready":
			readyOnce.Do(func() { close(ready) })
		case ev.Kind == "ctl" && ev.Name == "plugin.shutdown":
			shutdownOnce.Do(func() { close(shutdown) })
		}
	}
	// 连接断开（宿主退出 / 重启）→ 按关闭处理，避免悬挂。
	shutdownOnce.Do(func() { close(shutdown) })
}

// serve 是主循环（单实例多 workdir）：发现项目 → 按 workdir 加锁 → 派发到最多
// maxWorkers 个工作线程（每线程独立引擎进程）→ pending / status / take / coalesce /
// 引擎索引 / ack / status。线程空闲后释放引擎（bbolt index.db 常驻打开会阻塞插件侧
// clear / 全量重建）。
func serve(ctx context.Context, cli pluginclient.Client, engineExe string, shutdown <-chan struct{}) error {
	known := map[string]struct{}{}
	lastReady := map[string]time.Time{}
	locks := map[string]*lockfile.Lock{} // workdir → 该项目的索引器锁
	inflight := map[string]bool{}        // workdir → 已有批次在处理中

	jobs := make(chan string)
	results := make(chan batchOutcome, maxWorkers)
	var wg sync.WaitGroup
	for i := 0; i < maxWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runWorker(ctx, cli, engineExe, jobs, results)
		}()
	}
	stopWorkers := func() {
		close(jobs)
		wg.Wait()
		for _, lk := range locks {
			lk.Release()
		}
	}

	timer := time.NewTimer(dispatchPoll)
	defer timer.Stop()
	for {
		select {
		case <-shutdown:
			stopWorkers()
			return finalize(cli, known)
		case <-ctx.Done():
			stopWorkers()
			return finalize(cli, known)
		case <-timer.C:
		}
		timer.Reset(dispatchPoll)

		// 1) 收敛已完成批次。
		for draining := true; draining; {
			select {
			case r := <-results:
				inflight[r.workDir] = false
				if r.err != nil {
					// 单项目基础设施错误：不退出，下轮重试（引擎已在工作线程内释放）。
					slog.Warn("codegraph-indexer: 项目处理失败", "work_dir", r.workDir, "err", r.err)
					continue
				}
				if r.didWork {
					lastReady[r.workDir] = time.Now()
				} else {
					idleReady([]string{r.workDir}, lastReady)
				}
			default:
				draining = false
			}
		}

		// 2) 发现当前活跃项目。
		dirs, err := indexWorkDirs(ctx, cli)
		if err != nil {
			slog.Warn("codegraph-indexer: workdirs 查询失败", "err", err)
			timer.Reset(idlePoll)
			continue
		}
		if len(dirs) == 0 {
			idleReady(keysOf(known), lastReady)
			timer.Reset(idlePoll)
			continue
		}
		active := make(map[string]struct{}, len(dirs))
		for _, wd := range dirs {
			active[wd] = struct{}{}
			known[wd] = struct{}{}
		}

		// 3) 项目已不活跃 → 释放其 workdir 锁（处理中的等批次完成后再释放）。
		for wd, lk := range locks {
			if _, ok := active[wd]; ok || inflight[wd] {
				continue
			}
			lk.Release()
			delete(locks, wd)
			delete(lastReady, wd)
		}

		// 4) 派发（池满则下轮再派发；同项目同时只处理一个批次）。
		for _, wd := range dirs {
			if inflight[wd] || !ensureWorkDirLock(wd, locks) {
				continue
			}
			select {
			case jobs <- wd:
				inflight[wd] = true
			default:
			}
		}
	}
}

// batchOutcome 是一个 workdir 的单批处理结果（工作线程 → 主循环）。
type batchOutcome struct {
	workDir   string
	remaining int
	didWork   bool
	err       error
}

// runWorker 是一个工作线程：独占一个引擎进程，串行处理派发给它的 workdir 批次；
// 空闲 engineIdleClose 后释放引擎（下批懒重连）。
func runWorker(ctx context.Context, cli pluginclient.Client, engineExe string, jobs <-chan string, results chan<- batchOutcome) {
	eng := newEngine(engineExe)
	defer eng.close()
	idle := time.NewTicker(engineIdleClose)
	defer idle.Stop()
	var lastJob time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case workDir, ok := <-jobs:
			if !ok {
				return
			}
			lastJob = time.Now()
			remaining, didWork, err := processWorkDir(ctx, cli, eng, workDir)
			if err != nil {
				eng.close()
			}
			select {
			case results <- batchOutcome{workDir: workDir, remaining: remaining, didWork: didWork, err: err}:
			case <-ctx.Done():
				return
			}
		case <-idle.C:
			if !lastJob.IsZero() && time.Since(lastJob) >= engineIdleClose {
				eng.close()
				lastJob = time.Time{}
			}
		}
	}
}

// ensureWorkDirLock 确保本进程持有该项目的索引器锁（每个 workdir 一把，各项目互不影响）。
// 返回 false = 已被其它进程占用（本轮跳过，下轮重试）。
func ensureWorkDirLock(workDir string, locks map[string]*lockfile.Lock) bool {
	if locks[workDir] != nil {
		return true
	}
	lk, err := lockfile.Acquire(filepath.Join(indexDirOf(workDir), "indexer.lock"))
	if err != nil {
		if !errors.Is(err, lockfile.ErrBusy) {
			slog.Warn("codegraph-indexer: 获取项目索引锁失败", "work_dir", workDir, "err", err)
		}
		return false
	}
	locks[workDir] = lk
	return true
}

// indexDirOf 返回项目的 codegraph 索引目录（index.db / status / 索引器锁同根）。
func indexDirOf(workDir string) string {
	return filepath.Join(workDir, ".chonkpilot", "codegraph")
}

// processWorkDir 处理一个项目的至多一批变更。
//
// 顺序：pending 查询（0 → 不做任何事，避免无意义拉起引擎）→ status（未启用 → 排空队列）
// → take → 引擎索引 → ack → status。返回 (剩余 pending, 是否做了工作, error)。
func processWorkDir(ctx context.Context, cli pluginclient.Client, eng *engine, workDir string) (int, bool, error) {
	pending, err := indexPending(ctx, cli, workDir)
	if err != nil {
		return 0, false, fmt.Errorf("pending 查询失败: %w", err)
	}
	if pending <= 0 {
		return 0, false, nil
	}

	st, err := eng.status(ctx, workDir)
	if err != nil {
		_ = writeStatus(workDir, cg.IndexStatus{State: "error", Message: err.Error()})
		return pending, true, fmt.Errorf("status 查询失败: %w", err)
	}

	// 未启用：不索引，但把队列行全部 ack 掉（避免永久堆积），状态写 ready。
	if !st.Enabled {
		if err := drainQueue(ctx, cli, workDir); err != nil {
			return pending, true, fmt.Errorf("队列排空失败: %w", err)
		}
		if err := writeStatus(workDir, cg.IndexStatus{State: "ready", Pending: 0}); err != nil {
			slog.Warn("codegraph-indexer: 写 ready 状态失败", "work_dir", workDir, "err", err)
		}
		return 0, true, nil
	}

	items, err := indexTake(ctx, cli, workDir, batchLimit)
	if err != nil {
		return pending, false, fmt.Errorf("take 失败: %w", err)
	}
	if len(items) == 0 {
		return pending, false, nil
	}

	changes := coalesceChanges(items)
	if err := writeStatus(workDir, cg.IndexStatus{State: "indexing", Pending: pending}); err != nil {
		slog.Warn("codegraph-indexer: 写 indexing 状态失败", "work_dir", workDir, "err", err)
	}

	batchErr := runBatch(ctx, workDir, eng, st, changes)
	remaining := pending - len(items)
	if remaining < 0 {
		remaining = 0
	}
	if batchErr != nil {
		slog.Error("codegraph-indexer: 本批索引失败", "work_dir", workDir, "err", batchErr, "items", len(items))
		// 失败也释放引擎：避免半开状态长期持有索引库。
		eng.close()
		if ackErr := indexAck(ctx, cli, workDir, items, true, batchErr.Error()); ackErr != nil {
			slog.Warn("codegraph-indexer: ack(failed) 失败", "work_dir", workDir, "err", ackErr)
		}
		if err := writeStatus(workDir, cg.IndexStatus{State: "error", Pending: remaining, Message: batchErr.Error()}); err != nil {
			slog.Warn("codegraph-indexer: 写 error 状态失败", "work_dir", workDir, "err", err)
		}
		return remaining, true, nil // 单批失败不退出，外层退避
	}

	if ackErr := indexAck(ctx, cli, workDir, items, false, ""); ackErr != nil {
		// 索引已成功但回执失败：队列行保持 pending，下轮会重复索引（至少一次语义）。
		slog.Warn("codegraph-indexer: ack 失败", "work_dir", workDir, "err", ackErr)
	}
	if p, perr := indexPending(ctx, cli, workDir); perr == nil {
		remaining = p
	}
	if err := writeStatus(workDir, cg.IndexStatus{State: "ready", Pending: remaining}); err != nil {
		slog.Warn("codegraph-indexer: 写 ready 状态失败", "work_dir", workDir, "err", err)
	}
	return remaining, true, nil
}

// idleReady 对空闲项目按 30s 节流写一次 ready（避免频繁写盘）。
func idleReady(dirs []string, lastReady map[string]time.Time) {
	now := time.Now()
	for _, wd := range dirs {
		if now.Sub(lastReady[wd]) < readyWriteInterval {
			continue
		}
		if err := writeStatus(wd, cg.IndexStatus{State: "ready", Pending: 0}); err == nil {
			lastReady[wd] = now
		}
	}
}

// keysOf 取 map 键（无序遍历；调用方只用于节流写盘）。
func keysOf(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// runBatch 对一批（已合并）变更执行索引：
//   - 首次（state 为空 / not_initialized）→ codegraph_initialize 全量（不设短超时）；
//   - 否则 → codegraph_index_paths 按路径增量（绝对路径）。
func runBatch(ctx context.Context, workDir string, eng *engine, st engineStatus, changes []change) error {
	if len(changes) == 0 {
		return nil
	}
	if st.State == "" || st.State == "not_initialized" {
		return eng.initialize(ctx, workDir)
	}
	files, removes := planChanges(workDir, changes)
	if len(files) == 0 && len(removes) == 0 {
		return nil
	}
	return eng.indexPaths(ctx, workDir, files, removes)
}

// drainQueue 取空某项目队列并全部 ack（未启用索引时的排空路径）。
func drainQueue(ctx context.Context, cli pluginclient.Client, workDir string) error {
	for {
		pending, err := indexPending(ctx, cli, workDir)
		if err != nil {
			return err
		}
		if pending <= 0 {
			return nil
		}
		items, err := indexTake(ctx, cli, workDir, batchLimit)
		if err != nil {
			return err
		}
		if len(items) == 0 {
			return nil
		}
		if err := indexAck(ctx, cli, workDir, items, false, ""); err != nil {
			return err
		}
	}
}

// finalize 收尾：对已知项目各写一次 ready 状态（尽力查询剩余 pending）。
func finalize(cli pluginclient.Client, known map[string]struct{}) error {
	for wd := range known {
		pending := 0
		if cli != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			if p, err := indexPending(ctx, cli, wd); err == nil {
				pending = p
			}
			cancel()
		}
		_ = writeStatus(wd, cg.IndexStatus{State: "ready", Pending: pending})
	}
	return nil
}

func writeStatus(workDir string, st cg.IndexStatus) error {
	st.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	return cg.WriteIndexStatus(workDir, st)
}

func sleepOrStop(ctx context.Context, shutdown <-chan struct{}, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return true
	case <-shutdown:
		return true
	case <-t.C:
		return false
	}
}

// ─── 插件桥控制帧 ────────────────────────────────────────

// queueItem 是 plugin.index.take 返回的队列行（字段与 pluginbridge.IndexQueueItem 对齐）。
type queueItem struct {
	ID        string `json:"id"`
	WorkDir   string `json:"work_dir"`
	Path      string `json:"path"`
	Operation string `json:"operation"`
	Attempts  int    `json:"attempts"`
}

// indexWorkDirs 返回宿主当前活实例的去重 work_dir（单实例多 workdir 发现入口）。
func indexWorkDirs(ctx context.Context, cli pluginclient.Client) ([]string, error) {
	raw, err := cli.Call(ctx, "plugin.index.workdirs", map[string]any{"consumer": indexkind.Codegraph})
	if err != nil {
		return nil, err
	}
	var res struct {
		WorkDirs []string `json:"work_dirs"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("workdirs 应答解析失败: %w", err)
	}
	return res.WorkDirs, nil
}

func indexPending(ctx context.Context, cli pluginclient.Client, workDir string) (int, error) {
	raw, err := cli.Call(ctx, "plugin.index.pending", map[string]any{"work_dir": workDir, "consumer": indexkind.Codegraph})
	if err != nil {
		return 0, err
	}
	var res struct {
		Pending int `json:"pending"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, fmt.Errorf("pending 应答解析失败: %w", err)
	}
	return res.Pending, nil
}

func indexTake(ctx context.Context, cli pluginclient.Client, workDir string, limit int) ([]queueItem, error) {
	raw, err := cli.Call(ctx, "plugin.index.take", map[string]any{"work_dir": workDir, "consumer": indexkind.Codegraph, "limit": limit})
	if err != nil {
		return nil, err
	}
	var res struct {
		Items []queueItem `json:"items"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("take 应答解析失败: %w", err)
	}
	return res.Items, nil
}

func indexAck(ctx context.Context, cli pluginclient.Client, workDir string, items []queueItem, failed bool, errMsg string) error {
	ids := make([]string, 0, len(items))
	for _, it := range items {
		if it.ID != "" {
			ids = append(ids, it.ID)
		}
	}
	_, err := cli.Call(ctx, "plugin.index.ack", map[string]any{
		"work_dir": workDir,
		"consumer": indexkind.Codegraph,
		"ids":      ids,
		"failed":   failed,
		"error":    errMsg,
	})
	return err
}

// ─── 变更合并 ────────────────────────────────────────────

const (
	opCreate = "create"
	opWrite  = "write"
	opRemove = "remove"
)

// change 是合并后的单文件变更（Op ∈ create|write|remove）。
type change struct {
	Path string
	Op   string
}

// coalesceChanges 按路径合并队列行（同文件只处理一次，保留最新操作）：
//   - create / write → 保留最新一次（create 后 write → write）；
//   - remove / rename → remove（rename 旧路径按删除处理）；
//   - remove 后再 create → create（以最后一次为准）；
//   - create 后再 remove → remove。
//
// 输出按路径排序（确定性，便于测试与稳定 ack 顺序）。
func coalesceChanges(items []queueItem) []change {
	order := make([]string, 0, len(items))
	byPath := make(map[string]string, len(items))
	for _, it := range items {
		p := normalizePath(it.Path)
		if p == "" {
			continue
		}
		if _, ok := byPath[p]; !ok {
			order = append(order, p)
		}
		byPath[p] = normalizeOp(it.Operation)
	}
	out := make([]change, 0, len(order))
	for _, p := range order {
		out = append(out, change{Path: p, Op: byPath[p]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func normalizeOp(op string) string {
	switch strings.ToLower(strings.TrimSpace(op)) {
	case opCreate:
		return opCreate
	case opRemove, "rename":
		return opRemove
	default: // write / 未知 → 按写入（索引）处理
		return opWrite
	}
}

func normalizePath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	return filepath.ToSlash(filepath.Clean(p))
}

// planChanges 把合并后的变更分成 files / remove 两组（绝对路径，'/' 分隔）。
func planChanges(workDir string, changes []change) (files, removes []string) {
	for _, c := range changes {
		p := absPath(workDir, c.Path)
		if p == "" {
			continue
		}
		if c.Op == opRemove {
			removes = append(removes, p)
			continue
		}
		files = append(files, p)
	}
	return files, removes
}

// absPath 队列路径（通常已是绝对路径）→ '/' 分隔的绝对路径。
func absPath(workDir, p string) string {
	if strings.TrimSpace(p) == "" {
		return ""
	}
	s := filepath.ToSlash(p)
	if filepath.IsAbs(filepath.FromSlash(s)) {
		return s
	}
	return filepath.ToSlash(filepath.Join(workDir, filepath.FromSlash(s)))
}

// ─── codegraph 引擎（MCP stdio 最小客户端）────────────────

// engineStatus 是 codegraph_status 应答（引擎 Status 对象）的所需字段。
type engineStatus struct {
	Workdir string `json:"workdir"`
	State   string `json:"state"`
	Enabled bool   `json:"enabled"`
	Err     string `json:"err"`
}

// engine 是引擎子进程客户端（官方 go-sdk MCP client，stdio；传输失败重连一次）。
type engine struct {
	exe string

	mu sync.Mutex
	ss *mcp.ClientSession
}

func newEngine(exe string) *engine { return &engine{exe: exe} }

// status 查询工作区状态（应答可能直接是 Status 对象）。
func (e *engine) status(ctx context.Context, workDir string) (engineStatus, error) {
	text, isErr, err := e.call(ctx, "codegraph_status", map[string]any{"workdir": workDir})
	if err != nil {
		return engineStatus{}, err
	}
	if isErr {
		return engineStatus{}, errors.New(text)
	}
	var st engineStatus
	if err := json.Unmarshal([]byte(text), &st); err != nil {
		return engineStatus{}, fmt.Errorf("codegraph_status 应答解析失败: %w", err)
	}
	return st, nil
}

// initialize 全量建索引（首次）；首次全量可能耗时较长，不设短超时（由 ctx 决定）。
func (e *engine) initialize(ctx context.Context, workDir string) error {
	return e.toolErr(ctx, "codegraph_initialize", map[string]any{"workdir": workDir})
}

// indexPaths 按路径增量更新索引（绝对路径入参）。
func (e *engine) indexPaths(ctx context.Context, workDir string, files, removes []string) error {
	return e.toolErr(ctx, "codegraph_index_paths", map[string]any{
		"workdir": workDir,
		"files":   files,
		"remove":  removes,
	})
}

// toolErr 调用引擎工具并把协议内错误（IsError）转成 error。
func (e *engine) toolErr(ctx context.Context, name string, args map[string]any) error {
	text, isErr, err := e.call(ctx, name, args)
	if err != nil {
		return err
	}
	if isErr {
		return errors.New(text)
	}
	return nil
}

// call 调用引擎工具；传输层失败自动重连一次（参考 plugin-vfts client）。
func (e *engine) call(ctx context.Context, name string, args map[string]any) (string, bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	text, isErr, err := e.callLocked(ctx, name, args)
	if err == nil || ctx.Err() != nil {
		return text, isErr, err
	}
	e.closeLocked()
	return e.callLocked(ctx, name, args)
}

func (e *engine) callLocked(ctx context.Context, name string, args map[string]any) (string, bool, error) {
	if err := e.connectLocked(ctx); err != nil {
		return "", false, err
	}
	res, err := e.ss.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return "", false, err
	}
	return resultText(res), res.IsError, nil
}

func (e *engine) connectLocked(ctx context.Context) error {
	if e.ss != nil {
		return nil
	}
	cli := mcp.NewClient(&mcp.Implementation{Name: "chonkpilot-codegraph-indexer", Version: "0.1.0"}, nil)
	cmd := exec.Command(e.exe, "--stdio")
	// 引擎 exe 为 console 子系统：隐藏控制台窗口（不影响 stdio 管道）。
	cmd.SysProcAttr = winproc.SysProcAttr()
	ss, err := cli.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		return fmt.Errorf("spawn %s --stdio: %w", e.exe, err)
	}
	e.ss = ss
	return nil
}

func (e *engine) close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.closeLocked()
}

func (e *engine) closeLocked() {
	if e.ss != nil {
		_ = e.ss.Close()
	}
	e.ss = nil
}

// resultText 提取 CallToolResult 的首段文本 content（引擎统一 textResult/errResult）。
func resultText(res *mcp.CallToolResult) string {
	for _, ct := range res.Content {
		if tc, ok := ct.(*mcp.TextContent); ok && tc.Text != "" {
			return tc.Text
		}
	}
	return ""
}
