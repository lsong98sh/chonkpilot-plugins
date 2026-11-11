// writeinplace.go — 写入手柄抽象（平台无关说明与辅助）。
//
// Windows 走 writeInPlace（独占句柄就地写，见 write_in_place_windows.go）；
// 非 Windows 走“临时文件 + rename”原子写（见 write_in_place_other.go），
// 以保留 Unix 的 rename 原子性。
package filetool

import (
	"crypto/md5"
	"encoding/hex"
)

// md5Hex 计算字节内容的 MD5 hex（写工具用内存内容计算，避免写后再次打开
// 文件——Windows 独占句柄会挡住自己第二次打开）。
func md5Hex(data []byte) string {
	sum := md5.Sum(data)
	return hex.EncodeToString(sum[:])
}
