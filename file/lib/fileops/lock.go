package fileops

import (
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// lockSuffix 是跨进程锁文件后缀。
const lockSuffix = ".chonk.lock"

// staleLockAge 超过此时长的锁文件视为陈旧，可突破。
const staleLockAge = 30 * time.Second

// aliveHardStaleAge 持锁进程仍存活时的突破硬上限（C-26）：持锁超此时长仍突破，
// 防 pid 复用等导致锁文件永存（存活判定只作加速恢复，不作永久信任）。
const aliveHardStaleAge = 10 * time.Minute

// lockRetryCount 修改类操作默认加锁重试次数（30s：100ms × 300，见 acquireLock）。
const lockRetryCount = 300

// acquireLock 对 path 对应的文件加跨进程锁（`<path>.chonk.lock`）。
// 采用 O_CREATE|O_EXCL 原子创建，失败则重试（最多 retry 次，间隔 100ms）。
// 陈旧锁突破策略（C-26）：解析锁文件首行 token（host:pid:random）——pid 已死立即突破
// （不受 30s 限制，加快崩溃恢复）；pid 存活仅超 aliveHardStaleAge 硬上限才突破；
// host 非本机 / token 解析失败（旧格式/损坏）→ 回落时间规则（>staleLockAge）。
// 返回释放函数，调用方须在操作完成后 defer release。
func acquireLock(path string, retry int) (release func(), err error) {
	lockPath := lockFilePath(path)
	// 持有者令牌 = host:pid + 随机串：random 段保证同进程内不同持有者也可区分
	// （跨进程 ABA 与同进程 goroutine ABA 均能被 release 的归属比对识别）。
	payload := lockToken()

	for i := 0; i <= retry; i++ {
		// 尝试创建锁文件
		if f, oerr := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644); oerr == nil {
			f.WriteString(payload)
			f.WriteString("\n")
			f.WriteString(time.Now().Format(time.RFC3339))
			f.Close()
			release = func() { unlockFile(lockPath, payload) }
			return release, nil
		}

		// 检查是否可突破（死进程锁 / 陈旧锁，见 canBreakLock）
		if fi, sterr := os.Stat(lockPath); sterr == nil {
			if canBreakLock(lockPath, fi.ModTime()) {
				os.Remove(lockPath)
				continue
			}
		}

		if i < retry {
			time.Sleep(100 * time.Millisecond)
		}
	}
	return nil, fmt.Errorf("file %s is locked (lockfile: %s)", path, lockPath)
}

// canBreakLock 判定已存在的锁文件是否可突破（C-26）：
//   - 首行 token（host:pid:random）解析成功、host 为本机且平台支持探活：
//     pid 已死 → 立即突破（不受 30s 限制，加快崩溃恢复）；pid 存活 → 仅超
//     aliveHardStaleAge 硬上限才突破（日志说明，防 pid 复用导致锁永存）；
//   - host 非本机（无法探活远端进程）、token 解析失败（旧格式/损坏）或平台不支持探活
//     → 回落既有时间规则（>staleLockAge 视为陈旧）。
func canBreakLock(lockPath string, modTime time.Time) bool {
	b, err := os.ReadFile(lockPath)
	if err != nil {
		return time.Since(modTime) > staleLockAge
	}
	host, pid, ok := parseLockToken(firstLine(string(b)))
	if !ok || !strings.EqualFold(host, localHostname()) || !lockProbeSupported {
		return time.Since(modTime) > staleLockAge
	}
	if !processAlive(pid) {
		return true
	}
	if time.Since(modTime) > aliveHardStaleAge {
		fmt.Fprintf(os.Stderr, "[fileops] lock held by live pid %d over %v, force break: %s\n",
			pid, aliveHardStaleAge, lockPath)
		return true
	}
	return false
}

// parseLockToken 解析锁文件首行 token（host:pid:random）；返回 host、pid 与解析是否成功。
func parseLockToken(line string) (host string, pid int, ok bool) {
	parts := strings.SplitN(line, ":", 3)
	if len(parts) != 3 || parts[0] == "" || parts[2] == "" {
		return "", 0, false
	}
	p, err := strconv.Atoi(parts[1])
	if err != nil || p <= 0 {
		return "", 0, false
	}
	return parts[0], p, true
}

// firstLine 取文本首行（锁文件第 1 行 = 持有者令牌）。
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

var (
	localHostOnce sync.Once
	localHostName string
)

// localHostname 本机主机名（缓存；探活前与本机比对，远端锁无法探活）。
func localHostname() string {
	localHostOnce.Do(func() { localHostName, _ = os.Hostname() })
	return localHostName
}

// unlockFile 删除锁文件；**仅在归属比对通过时删除**：读回锁文件首行与本次持有者令牌一致
// 才 Remove，否则说明锁已被突破（陈旧被抢）或已由他人持有（ABA）→ 记日志放弃，不误删他人锁。
func unlockFile(lockPath, payload string) {
	b, err := os.ReadFile(lockPath)
	if err != nil {
		return
	}
	if !strings.HasPrefix(string(b), payload+"\n") {
		fmt.Fprintf(os.Stderr, "[fileops] lock owner mismatch, skip unlock: %s\n", lockPath)
		return
	}
	os.Remove(lockPath)
}

// lockToken 生成锁持有者令牌（host:pid:random）。
func lockToken() string {
	host, _ := os.Hostname()
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%s:%d:%s", host, os.Getpid(), hex.EncodeToString(b))
}

// lockFilePath 返回锁文件路径。
func lockFilePath(path string) string {
	return filepath.Clean(path) + lockSuffix
}

// fileMD5 计算文件内容 MD5 hex（流式，O(1) 内存；不整文件读入，避免大文件内存无界，C-54 同族）。
func fileMD5(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := md5.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
