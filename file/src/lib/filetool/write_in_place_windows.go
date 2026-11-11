//go:build windows

package filetool

import (
	"io"
	"os"
)

// writeInPlace 在已独占打开的句柄上就地覆盖：Seek(0) → 循环写全 → Truncate → Sync。
//
// 返回值 written 是**已成功写入的字节数**（含部分写入）；任一步失败时调用方
// 据此判断是否需要标注 partial。写期间其他应用因 dwShareMode=0 无法打开该文件。
//
// 代价：就地写不具备 rename 的崩溃原子性——进程在写一半崩溃可能留下部分内容。
func writeInPlace(f *os.File, data []byte) (int, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	written := 0
	for written < len(data) {
		n, err := f.Write(data[written:])
		written += n
		if err != nil {
			return written, err
		}
		if n == 0 {
			return written, io.ErrShortWrite
		}
	}
	if err := f.Truncate(int64(len(data))); err != nil {
		return written, err
	}
	if err := f.Sync(); err != nil {
		return written, err
	}
	return written, nil
}
