package execx

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

const (
	lockHelperEnv = "CK_CODEGRAPH_LOCK_HELPER"
	lockDBEnv     = "CK_CODEGRAPH_LOCK_DB"
)

// TestMain 兼任「持锁助手进程」：env 命中时以**排他方式**打开索引库并阻塞，直到父进程关闭 stdin。
// 这样测试里的 LOCK 是真正跨进程的（与 resident indexer / 管理动作持锁同语义）。
func TestMain(m *testing.M) {
	if os.Getenv(lockHelperEnv) == "1" {
		db, err := bolt.Open(os.Getenv(lockDBEnv), 0o644, nil)
		if err != nil {
			fmt.Fprintln(os.Stderr, "helper open:", err)
			os.Exit(3)
		}
		fmt.Println("ready")
		_, _ = io.Copy(io.Discard, os.Stdin) // 父进程关闭 stdin 前一直持有排他锁
		_ = db.Close()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// TestRunLockedDBBusyMessage 是索引库 LOCK 的**故障注入**测试：
// 另一进程排他持有 bbolt 索引库 → 查询 executor 有界等待（约 3s）后以统一文本失败
// 「正在索引，不可用…」，而不是裸报系统错误 / 无限阻塞。
func TestRunLockedDBBusyMessage(t *testing.T) {
	work := t.TempDir()
	store := filepath.Join(work, ".chonkpilot", "codegraph")
	if err := os.MkdirAll(store, 0o755); err != nil {
		t.Fatalf("mkdir store: %v", err)
	}
	dbFile := filepath.Join(store, "index.db")

	helper := exec.Command(os.Args[0], "-test.run=XXX-no-such-test")
	helper.Env = append(os.Environ(), lockHelperEnv+"=1", lockDBEnv+"="+dbFile)
	stdin, err := helper.StdinPipe()
	if err != nil {
		t.Fatalf("helper stdin: %v", err)
	}
	stdout, err := helper.StdoutPipe()
	if err != nil {
		t.Fatalf("helper stdout: %v", err)
	}
	var helperErr bytes.Buffer
	helper.Stderr = &helperErr
	if err := helper.Start(); err != nil {
		t.Fatalf("helper start: %v", err)
	}
	stop := func() {
		_ = stdin.Close() // 释放 io.Copy → helper 关库退出
		_ = helper.Wait()
	}
	defer stop()

	readyCh := make(chan error, 1)
	go func() {
		line, rerr := bufio.NewReader(stdout).ReadString('\n')
		if rerr != nil {
			readyCh <- fmt.Errorf("read helper ready: %w (stderr=%s)", rerr, helperErr.String())
			return
		}
		if strings.TrimSpace(line) != "ready" {
			readyCh <- fmt.Errorf("helper ready line = %q", line)
			return
		}
		readyCh <- nil
	}()
	select {
	case rerr := <-readyCh:
		if rerr != nil {
			t.Fatalf("helper 未就绪: %v", rerr)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("helper 持锁超时（15s）")
	}

	in := filepath.Join(work, "in.json")
	if err := os.WriteFile(in, []byte(`{"workdir":`+strconv.Quote(work)+`,"query":"x"}`), 0o644); err != nil {
		t.Fatalf("write input: %v", err)
	}

	var out, errOut bytes.Buffer
	start := time.Now()
	code := run("codegraph_symbol_search", []string{"codegraph_symbol_search", "--input=" + in}, &out, &errOut, 3*time.Second)
	elapsed := time.Since(start)

	if code != 1 {
		t.Fatalf("exit=%d（期望 1 工具失败），stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	if got := out.String(); got != busyMessage {
		t.Fatalf("失败文本 = %q，期望 %q", got, busyMessage)
	}
	// 有界等待：约 3s（2 次 1.5s 打开超时 + 重试间隔）；下限 2s、上限 8s（防环境抖动）。
	if elapsed < 2*time.Second || elapsed > 8*time.Second {
		t.Fatalf("等待时长 = %v，期望 2–8s（约 3s 有界等待）", elapsed)
	}
}

// TestIsLockErr 覆盖 Windows 共享冲突 / bbolt 超时 / 中文错误三类判定。
func TestIsLockErr(t *testing.T) {
	cases := []struct {
		msg  string
		want bool
	}{
		{"index.db 打开失败: timeout", true},
		{"The process cannot access the file because it is being used by another process.", true},
		{"文件被另一个进程使用", true},
		{"lock: another process holds it", true},
		{"unknown codegraph query tool: x", false},
		{"", false},
	}
	for _, c := range cases {
		var err error
		if c.msg != "" {
			err = fmt.Errorf("%s", c.msg)
		}
		if got := isLockErr(err); got != c.want {
			t.Fatalf("isLockErr(%q) = %v，期望 %v", c.msg, got, c.want)
		}
	}
}
