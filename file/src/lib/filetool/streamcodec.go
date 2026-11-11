// streamcodec.go — file_iconv 的流式编解码辅助。
//
// file_iconv 不再整文件 io.ReadAll：源文件经 io.SectionReader 分段读取，
// decoder → encoder 用 x/text 的 transform.Reader 串联，最终 io.Copy 到
// 同目录临时文件。内存占用与文件大小无关（固定 64 KiB 级缓冲）。
package filetool

import (
	"bytes"
	"errors"
	"io"
	"os"

	"github.com/lsong98sh/chonkpilot-util/encodingx"
	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/encoding/korean"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/traditionalchinese"
	xunicode "golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"
)

// streamChunkBytes 是流式转换 / 分段比较使用的缓冲大小。
const streamChunkBytes = 64 << 10

// streamStageError 标注转换链中出错的阶段（decode / encode / write），
// 供调用方映射回既有的 decode_error / encode_error / write 文案。
type streamStageError struct {
	stage string
	err   error
}

func (e *streamStageError) Error() string { return e.err.Error() }
func (e *streamStageError) Unwrap() error { return e.err }

// isStreamStage 判断 err（含被包裹的转换链错误）是否来自指定阶段。
func isStreamStage(err error, stage string) bool {
	var se *streamStageError
	return errors.As(err, &se) && se.stage == stage
}

// newStreamDecoder 返回按编码名解码的流式 transformer；utf-8 返回 nil（原样透传）。
// 未知编码 → ok=false（正常路径已在参数校验阶段拒绝）。
func newStreamDecoder(name string) (transform.Transformer, bool) {
	switch n := normalizeEncoding(name); n {
	case encodingx.UTF8:
		return nil, true
	case encodingx.UTF16LE:
		return xunicode.UTF16(xunicode.LittleEndian, xunicode.IgnoreBOM).NewDecoder(), true
	case encodingx.UTF16BE:
		return xunicode.UTF16(xunicode.BigEndian, xunicode.IgnoreBOM).NewDecoder(), true
	default:
		// 复用 readwrite.go 的 legacy 映射（gbk/gb18030/shift_jis/big5/euc-kr）。
		return legacyDecoder(n)
	}
}

// newStreamEncoder 返回按编码名编码的流式 transformer；utf-8 返回 nil
// （BOM 由调用方在写入前自行输出）。withBOM 只对 UTF-16 生效，legacy 恒忽略。
func newStreamEncoder(name string, withBOM bool) (transform.Transformer, bool) {
	switch n := normalizeEncoding(name); n {
	case encodingx.UTF8:
		return nil, true
	case encodingx.UTF16LE:
		policy := xunicode.IgnoreBOM
		if withBOM {
			policy = xunicode.UseBOM
		}
		return xunicode.UTF16(xunicode.LittleEndian, policy).NewEncoder(), true
	case encodingx.UTF16BE:
		policy := xunicode.IgnoreBOM
		if withBOM {
			policy = xunicode.UseBOM
		}
		return xunicode.UTF16(xunicode.BigEndian, policy).NewEncoder(), true
	default:
		return legacyEncoder(n)
	}
}

// legacyEncoder 返回遗留编码的流式 encoder（legacy 目标无法携带 BOM）。
func legacyEncoder(name string) (transform.Transformer, bool) {
	switch name {
	case encodingx.GBK:
		return simplifiedchinese.GBK.NewEncoder(), true
	case encodingx.GB18030:
		return simplifiedchinese.GB18030.NewEncoder(), true
	case encodingx.ShiftJIS:
		return japanese.ShiftJIS.NewEncoder(), true
	case encodingx.Big5:
		return traditionalchinese.Big5.NewEncoder(), true
	case encodingx.EUCKR:
		return korean.EUCKR.NewEncoder(), true
	}
	return nil, false
}

// skipBOMFor 返回源文件 BOM 中应跳过的字节数：仅当 BOM 与解码方向一致时跳过
// （utf-8 3 字节；utf-16le/be 2 字节）。显式 from 与源 BOM 不同时不跳，
// round-trip 的 BOM 策略会输出与原字节不同的结果，从而暴露不匹配。
func skipBOMFor(name, sourceBOM string) int64 {
	b := normalizeEncoding(sourceBOM)
	switch normalizeEncoding(name) {
	case encodingx.UTF8:
		if b == encodingx.UTF8 {
			return 3
		}
	case encodingx.UTF16LE:
		if b == encodingx.UTF16LE {
			return 2
		}
	case encodingx.UTF16BE:
		if b == encodingx.UTF16BE {
			return 2
		}
	}
	return 0
}

// stageReader 把 decoder / encoder transformer 的错误标注为对应阶段。
// 已标注的错误原样透传（保留最内层 decode 标记，不被外层 encode 覆盖）。
type stageReader struct {
	r     io.Reader
	stage string
}

func (r *stageReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if err == nil || err == io.EOF {
		return n, err
	}
	var se *streamStageError
	if errors.As(err, &se) {
		return n, err
	}
	return n, &streamStageError{stage: r.stage, err: err}
}

// stageWriter 把目标写入错误标注为 write 阶段（区别于源读取错误）。
type stageWriter struct {
	w io.Writer
}

func (w *stageWriter) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	if err != nil {
		return n, &streamStageError{stage: "write", err: err}
	}
	return n, nil
}

// streamConvert 从 src 的 [skip, size) 区间流式读取，依次经过 dec、enc，
// 写入 dst。目标为 UTF-8 且 writeUTF8BOM 时先写 3 字节 BOM（调用方按 bom 模式决定）。
// 返回实际写入字节数（含 BOM），不整文件入内存。
func streamConvert(dst io.Writer, src *os.File, size, skip int64, dec, enc transform.Transformer, writeUTF8BOM bool) (int64, error) {
	if skip < 0 {
		skip = 0
	}
	if skip > size {
		skip = size
	}
	var r io.Reader = io.NewSectionReader(src, skip, size-skip)
	if dec != nil {
		r = &stageReader{r: transform.NewReader(r, dec), stage: "decode"}
	}
	if enc != nil {
		r = &stageReader{r: transform.NewReader(r, enc), stage: "encode"}
	}

	w := &stageWriter{w: dst}
	var written int64
	if writeUTF8BOM {
		n, err := w.Write([]byte{0xEF, 0xBB, 0xBF})
		written += int64(n)
		if err != nil {
			return written, err
		}
		if n != 3 {
			return written, io.ErrShortWrite
		}
	}
	n, err := io.CopyBuffer(w, r, make([]byte, streamChunkBytes))
	written += n
	if err != nil {
		return written, err
	}
	return written, nil
}

// filesEqual 以 64 KiB 分段比较两个文件（不整文件入内存）。
// 返回 false 时 err 为 nil 表示内容不同；IO 错误才返回非 nil err。
func filesEqual(a, b *os.File) (bool, error) {
	if _, err := a.Seek(0, io.SeekStart); err != nil {
		return false, err
	}
	if _, err := b.Seek(0, io.SeekStart); err != nil {
		return false, err
	}
	bufA := make([]byte, streamChunkBytes)
	bufB := make([]byte, streamChunkBytes)
	for {
		na, ea := io.ReadFull(a, bufA)
		nb, eb := io.ReadFull(b, bufB)
		if na != nb || !bytes.Equal(bufA[:na], bufB[:nb]) {
			return false, nil
		}
		aEOF := ea == io.EOF || ea == io.ErrUnexpectedEOF
		bEOF := eb == io.EOF || eb == io.ErrUnexpectedEOF
		if aEOF || bEOF {
			return aEOF && bEOF, nil
		}
		if ea != nil {
			return false, ea
		}
		if eb != nil {
			return false, eb
		}
	}
}

// iconvTemp 是 file_iconv 使用的同目录临时文件（backup / roundTrip / out）。
// discard 关闭并删除；输出文件被 rename 后路径已消失，删除为 no-op。
type iconvTemp struct {
	f    *os.File
	path string
}

func createIconvTemp(dir string) (*iconvTemp, error) {
	f, err := os.CreateTemp(dir, ".ck-iconv-*")
	if err != nil {
		return nil, err
	}
	return &iconvTemp{f: f, path: f.Name()}, nil
}

func (t *iconvTemp) discard() {
	if t == nil || t.f == nil {
		return
	}
	_ = t.f.Close()
	_ = os.Remove(t.path)
}

// streamCommitOutcome 描述提交失败时的目标状态。
type streamCommitOutcome struct {
	// rolledBack 表示目标可能已被就地写、但已用 backup 恢复原内容。
	rolledBack bool
	// partial 表示目标可能不完整（就地提交失败且回滚也失败）。
	partial bool
}

// commitStreamHook 仅供测试注入提交 / 回滚失败；stage ∈ {"commit", "rollback"}。
// 生产路径恒为 nil。
var commitStreamHook func(stage string) error

func runCommitStreamHook(stage string) error {
	if commitStreamHook == nil {
		return nil
	}
	return commitStreamHook(stage)
}
