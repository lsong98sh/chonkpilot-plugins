//go:build windows

package filetool

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// openExclusiveExistingOnce 以 GENERIC_READ|GENERIC_WRITE、dwShareMode=0 独占打开
// 已存在文件；**只尝试一次**，不内部重试（批调度器负责 pending 重试）。
//
// 共享/锁冲突 → errFileInUse（调度器记 pending）；不存在 → errTargetNotExist；
// 目录 → errIsDir；其他错误（ACCESS_DENIED、路径错误等）原样返回（fatal）。
func openExclusiveExistingOnce(path string) (*os.File, error) {
	if isDirPath(path) {
		return nil, errIsDir
	}
	f, err := openExclusive(path, windows.OPEN_EXISTING)
	if err != nil {
		if isNotExistErr(err) {
			return nil, errTargetNotExist
		}
		if isRetryableOpenErr(err) {
			return nil, errFileInUse
		}
		return nil, err
	}
	return f, nil
}

// openExclusiveWriteOnce 独占打开（已存在）或独占创建（不存在）目标文件；
// **只尝试一次**，不内部重试。overwrite=false 且已存在 → 关闭句柄并返回 errTargetExists。
// 共享/锁冲突 → errFileInUse；其他错误原样返回。
func openExclusiveWriteOnce(path string, overwrite bool) (f *os.File, existed bool, err error) {
	if isDirPath(path) {
		return nil, false, errIsDir
	}
	f, err = openExclusive(path, windows.OPEN_EXISTING)
	if err == nil {
		if !overwrite {
			_ = f.Close()
			return nil, true, errTargetExists
		}
		return f, true, nil
	}
	if !isNotExistErr(err) {
		if isRetryableOpenErr(err) {
			return nil, false, errFileInUse
		}
		return nil, false, err
	}

	f, err = openExclusive(path, windows.CREATE_NEW)
	if err == nil {
		return f, false, nil
	}
	if errors.Is(err, windows.ERROR_FILE_EXISTS) || errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		// 竞态：探测与创建之间被他人创建 → 按已存在重新打开（不覆盖创建语义）。
		if !overwrite {
			return nil, true, errTargetExists
		}
		f, oerr := openExclusive(path, windows.OPEN_EXISTING)
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

// openExclusive 用 Windows CreateFile 打开 path；shareMode=0 即独占。
// 成功后的句柄交给 os.NewFile，由 os.File.Close 负责 CloseHandle。
func openExclusive(path string, disposition uint32) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(
		p,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		0, // dwShareMode=0：其他进程无法同时打开
		nil,
		disposition,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(h), path), nil
}

// isRetryableOpenErr 只有共享冲突/锁冲突才允许进入 pending；ACCESS_DENIED、路径错误立即返回。
func isRetryableOpenErr(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION) ||
		errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}

// isNotExistErr 判断 Windows 打开错误是否为“目标不存在”。
func isNotExistErr(err error) bool {
	return errors.Is(err, windows.ERROR_FILE_NOT_FOUND) ||
		errors.Is(err, windows.ERROR_PATH_NOT_FOUND)
}
