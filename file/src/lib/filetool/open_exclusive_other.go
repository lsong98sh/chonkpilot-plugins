//go:build !windows

package filetool

import (
	"errors"
	"os"
	"syscall"
)

// openExclusiveExistingOnce 在 Unix 上用 O_RDWR 打开并以 flock(LOCK_EX|LOCK_NB)
// 取得协作锁；**只尝试一次**，不内部重试（批调度器负责 pending 重试）。
//
// 不存在 → errTargetNotExist；锁冲突（EWOULDBLOCK/EAGAIN）→ errFileInUse；
// 目录 → errIsDir；其他错误原样返回。
//
// 说明：Unix 没有 Windows 语义的“独占打开”，flock 只是**协作锁**——不遵守
// flock 的其他应用仍可同时读写。写工具在 Unix 仍走“临时文件 + rename”原子
// 写路径（见 writeInPlace），因此不承诺写期间对其他应用可见的独占。
func openExclusiveExistingOnce(path string) (*os.File, error) {
	if isDirPath(path) {
		return nil, errIsDir
	}
	f, err := openAndFlock(path, os.O_RDWR)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errTargetNotExist
		}
		if isRetryableOpenErr(err) {
			return nil, errFileInUse
		}
		return nil, err
	}
	return f, nil
}

// openExclusiveWriteOnce 打开（已存在）或创建（不存在）目标文件并取得 flock；
// **只尝试一次**，不内部重试。overwrite=false 且已存在 → 关闭句柄并返回 errTargetExists。
func openExclusiveWriteOnce(path string, overwrite bool) (f *os.File, existed bool, err error) {
	if isDirPath(path) {
		return nil, false, errIsDir
	}
	f, err = openAndFlock(path, os.O_RDWR)
	if err == nil {
		if !overwrite {
			_ = f.Close()
			return nil, true, errTargetExists
		}
		return f, true, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		if isRetryableOpenErr(err) {
			return nil, false, errFileInUse
		}
		return nil, false, err
	}

	f, err = openAndFlock(path, os.O_RDWR|os.O_CREATE|os.O_EXCL)
	if err == nil {
		return f, false, nil
	}
	if errors.Is(err, os.ErrExist) {
		// 竞态：探测与创建之间被他人创建 → 按已存在重新打开。
		if !overwrite {
			return nil, true, errTargetExists
		}
		f, oerr := openAndFlock(path, os.O_RDWR)
		if oerr != nil {
			if isRetryableOpenErr(oerr) {
				return nil, true, errFileInUse
			}
			return nil, true, oerr
		}
		return f, true, nil
	}
	if isRetryableOpenErr(err) {
		return nil, false, errFileInUse
	}
	return nil, false, err
}

// openAndFlock 打开文件并尝试非阻塞排他 flock；flock 失败时关闭句柄并返回错误，
// 由调度器识别 EWOULDBLOCK 后进入 pending。flock 随 Close 自动释放。
func openAndFlock(path string, flag int) (*os.File, error) {
	f, err := os.OpenFile(path, flag, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// isRetryableOpenErr 仅锁冲突（EWOULDBLOCK/EAGAIN）可进入 pending；权限、路径错误立即返回。
func isRetryableOpenErr(err error) bool {
	return errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN)
}
