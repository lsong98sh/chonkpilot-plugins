//go:build !windows

package filetool

// holdDeleteSemantics 在非 Windows 平台为 false：保留「预检 no-op + RemoveAll」删除路径，
// 不承诺独占/全有全无。
const holdDeleteSemantics = false

// heldFile 在非 Windows 平台只是路径占位：没有可持有的独占句柄。
type heldFile struct {
	path string
}

func holdExclusive(path string, _ bool) (*heldFile, error) {
	return &heldFile{path: path}, nil
}

func (f *heldFile) markDeleteOnClose() error  { return nil }
func (f *heldFile) clearDeleteOnClose() error { return nil }
func (f *heldFile) md5() (string, error)      { return md5File(f.path) }
func (f *heldFile) close() error              { return nil }
