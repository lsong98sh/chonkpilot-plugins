// session.go：会话内容全文索引集合（SESSION-FTS，2B-vfts §3.5，2026-11-01）。
//
// 与文件索引（<workDir>/.chonkpilot/vfts/）**完全独立**：本集合落
// <workDir>/.chonkpilot/vfts-session/（collection/ + meta.json + state.json），
// 文件索引的全量重建（Initialize）只清 vfts/ 目录，不会碰到会话索引。
//
// 一个 doc = 一条消息：主键 s:<session_id>:<message_id>；path = sessions/<session_id>/<message_id>
// （复用既有 path 子串过滤口径）；content = 消息正文（同一 jieba 分词器 + lowercase）。
// 另存 session_id / dialog_id / turn_id / message_id / role / ts（消息创建时间，UnixMilli）。
//
// 过滤方案（最稳一条）：会话过滤**不依赖** zvec 的 filter 表达式是否可用 —— 查询时先尝试
// 服务端 SetFilter("session_id == '…'")（可用则缩小候选、保证召回），失败则去掉过滤重试；
// 无论哪条路径，Go 侧都按 session_id / role 做最终过滤（正确性不依赖 filter 能力）。
package server

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	zvec "github.com/zvec-ai/zvec-go"
)

// 会话集合落盘布局（与文件索引目录并列、互不影响）。
const (
	sessionStoreName = "vfts-session"
	sessionCollDir   = "collection"
	sessionMetaName  = "meta.json"
	sessionStateName = "state.json"
)

// 会话集合字段名（除 path/content 外均用于命中回带与过滤）。
const (
	sfieldPath    = "path"
	sfieldContent = "content"
	sfieldSession = "session_id"
	sfieldDialog  = "dialog_id"
	sfieldTurn    = "turn_id"
	sfieldMessage = "message_id"
	sfieldRole    = "role"
	sfieldTS      = "ts"
)

// sessionSnippetChars 会话命中片段长度（命中词前后约 240 字符）。
const sessionSnippetChars = 240

// ErrSessionNotInitialized 会话索引未初始化（查询门控用；不算协议错误）。
var ErrSessionNotInitialized = fmt.Errorf("会话内容索引未初始化（请先重建会话索引）")

// SessionDoc 一条待索引的会话消息（一条消息 = 一个 doc）。
type SessionDoc struct {
	SessionID string `json:"session_id"`
	DialogID  string `json:"dialog_id,omitempty"`
	TurnID    string `json:"turn_id,omitempty"`
	MessageID string `json:"message_id"`
	Role      string `json:"role"`
	TS        int64  `json:"ts,omitempty"` // 消息创建时间（UnixMilli）
	Text      string `json:"text"`
}

// SessionHit 一条会话命中（按 ts 升序返回，供会话内上下跳转）。
type SessionHit struct {
	SessionID string  `json:"session_id"`
	DialogID  string  `json:"dialog_id,omitempty"`
	TurnID    string  `json:"turn_id,omitempty"`
	MessageID string  `json:"message_id"`
	Role      string  `json:"role"`
	TS        int64   `json:"ts"`
	Snippet   string  `json:"snippet"`
	Score     float32 `json:"score"`
}

// SessionIndexResult 一次会话索引写入的计数。
type SessionIndexResult struct {
	Added   int `json:"added"`   // 本次写入的 doc 数
	Removed int `json:"removed"` // 本次删除的 doc 数（先删后插）
	Total   int `json:"total"`   // 集合当前 doc 总数
}

// SessionStatus 会话索引状态视图（工具 vfts_session_status 的返回体）。
type SessionStatus struct {
	State         string `json:"state"` // "" 未初始化 | indexing | ready | error
	Sessions      int    `json:"sessions"`
	Docs          int    `json:"docs"`
	LastIndexedAt int64  `json:"lastIndexedAt"`
}

// SessionCursor 单会话的索引游标（state.json；插件全量重建断点续建 + 后台对账用）。
type SessionCursor struct {
	IndexedUntilTurn string `json:"indexed_until_turn,omitempty"`
	UpdatedAt        int64  `json:"updatedAt,omitempty"`
	// Fingerprint 该会话已索引内容的稳定指纹（插件按「消息 id + role + 正文」算的
	// sha256 hex；D-74 后台对账用，空 = 尚未记录指纹）。
	Fingerprint string `json:"fingerprint,omitempty"`
	// IndexedAt 最近一次记录指纹的时刻（UnixMilli，UTC 语义；仅用于日志 / 对账新鲜度）。
	IndexedAt int64 `json:"indexedAt,omitempty"`
}

// sessionStateFile 是 state.json 的内容（state + 总 doc 数 + 每会话游标）。
type sessionStateFile struct {
	State         string                   `json:"state"`
	Err           string                   `json:"err,omitempty"`
	LastIndexedAt int64                    `json:"lastIndexedAt"`
	Docs          int                      `json:"docs"`
	Sessions      map[string]SessionCursor `json:"sessions,omitempty"`
}

// sessionMetaFile 是 meta.json 的内容（跨进程可读的状态/计数镜像）。
type sessionMetaFile struct {
	State         string `json:"state"`
	Err           string `json:"err,omitempty"`
	Tokenizer     string `json:"tokenizer"`
	Docs          int    `json:"docs"`
	Sessions      int    `json:"sessions"`
	LastIndexedAt int64  `json:"lastIndexedAt"`
}

// SessionStoreDir 返回会话索引落盘目录（<workdir>/.chonkpilot/vfts-session）。
func SessionStoreDir(dir string) string {
	return filepath.ToSlash(filepath.Join(dir, ".chonkpilot", sessionStoreName))
}

// sessionCollPath zvec 集合目录（OS 原生分隔符，供 C-API 使用）。
func (w *Workspace) sessionCollPath() string {
	return filepath.Join(filepath.FromSlash(w.SessionStore), sessionCollDir)
}

func (w *Workspace) sessionMetaPath() string {
	return filepath.Join(filepath.FromSlash(w.SessionStore), sessionMetaName)
}

func (w *Workspace) sessionStatePath() string {
	return filepath.Join(filepath.FromSlash(w.SessionStore), sessionStateName)
}

// SessionRemove 一条待删会话消息（先删后插的反向；按 message_id 定位）。
type SessionRemove struct {
	SessionID string `json:"session_id"`
	MessageID string `json:"message_id"`
}

// sessionPK 会话消息主键。
//
// 契约形态是 `s:<session_id>:<message_id>`，但 zvec 主键不接受 ':'（INVALID_ARGUMENT:
// contains invalid characters），故实际主键用 base64url（字母数字 + `-`/`_`）编码：
// `s_<b64(session_id|message_id)>`。映射是单射；会话/消息 id 仍原样存字段，
// path 仍是 `sessions/<session_id>/<message_id>`（对外一律用原 id）。
func sessionPK(sessionID, messageID string) string {
	return "s_" + base64.RawURLEncoding.EncodeToString([]byte(sessionID+"|"+messageID))
}

// sessionDocPath 会话 doc 的 path 字段（复用「path 子串过滤」口径）。
func sessionDocPath(sessionID, messageID string) string {
	return "sessions/" + sessionID + "/" + messageID
}

// buildSessionSchema 构建会话集合 schema：path + content(FTS) + 会话元数据字段。
func buildSessionSchema() (*zvec.CollectionSchema, func()) {
	schema := zvec.NewCollectionSchema("vfts_session")

	pathField := zvec.NewFieldSchema(sfieldPath, zvec.DataTypeString, false, 0)
	_ = schema.AddField(pathField)

	contentField := zvec.NewFieldSchema(sfieldContent, zvec.DataTypeString, false, 0)
	contentParams, err := zvec.NewFTSIndexParams(tokenizer, ftsFilters, jiebaExtraParams())
	if err == nil {
		_ = contentField.SetIndexParams(contentParams)
	}
	_ = schema.AddField(contentField)

	sessionField := zvec.NewFieldSchema(sfieldSession, zvec.DataTypeString, false, 0)
	dialogField := zvec.NewFieldSchema(sfieldDialog, zvec.DataTypeString, true, 0)
	turnField := zvec.NewFieldSchema(sfieldTurn, zvec.DataTypeString, true, 0)
	messageField := zvec.NewFieldSchema(sfieldMessage, zvec.DataTypeString, false, 0)
	roleField := zvec.NewFieldSchema(sfieldRole, zvec.DataTypeString, false, 0)
	tsField := zvec.NewFieldSchema(sfieldTS, zvec.DataTypeInt64, false, 0)
	for _, f := range []*zvec.FieldSchema{sessionField, dialogField, turnField, messageField, roleField, tsField} {
		_ = schema.AddField(f)
	}

	cleanup := func() {
		if contentParams != nil {
			contentParams.Destroy()
		}
		pathField.Destroy()
		contentField.Destroy()
		sessionField.Destroy()
		dialogField.Destroy()
		turnField.Destroy()
		messageField.Destroy()
		roleField.Destroy()
		tsField.Destroy()
		schema.Destroy()
	}
	return schema, cleanup
}

// createSessionCollection 新建并打开会话集合（调用方须保证目标路径不存在）。
func createSessionCollection(path string) (*zvec.Collection, error) {
	if err := ensureZvec(); err != nil {
		return nil, err
	}
	schema, cleanup := buildSessionSchema()
	defer cleanup()
	return zvec.CreateAndOpen(path, schema, nil)
}

// ensureSessionCollLocked 确保会话集合已打开（须持 smu）：内存有则复用；盘上有则打开；否则新建。
func (w *Workspace) ensureSessionCollLocked() (*zvec.Collection, error) {
	if c := w.sessColl.Load(); c != nil {
		return c, nil
	}
	path := w.sessionCollPath()
	if _, err := os.Stat(path); err == nil {
		c, err := openCollection(path)
		if err != nil {
			return nil, fmt.Errorf("打开会话索引集合失败: %w", err)
		}
		w.sessColl.Store(c)
		return c, nil
	}
	if err := os.MkdirAll(filepath.FromSlash(w.SessionStore), 0o755); err != nil {
		return nil, err
	}
	c, err := createSessionCollection(path)
	if err != nil {
		return nil, err
	}
	w.sessColl.Store(c)
	return c, nil
}

// openSessionCollLocked 只打开既有会话集合（不新建；未初始化 → ErrSessionNotInitialized）。
func (w *Workspace) openSessionCollLocked() (*zvec.Collection, error) {
	if c := w.sessColl.Load(); c != nil {
		return c, nil
	}
	path := w.sessionCollPath()
	if _, err := os.Stat(path); err != nil {
		return nil, ErrSessionNotInitialized
	}
	c, err := openCollection(path)
	if err != nil {
		return nil, fmt.Errorf("打开会话索引集合失败: %w", err)
	}
	w.sessColl.Store(c)
	return c, nil
}

// loadSessionStateLocked 从盘刷新 state.json（缺失 = 零值；解析失败保留内存值）。
func (w *Workspace) loadSessionStateLocked() {
	b, err := os.ReadFile(w.sessionStatePath())
	if err != nil {
		return
	}
	var st sessionStateFile
	if json.Unmarshal(b, &st) != nil {
		return
	}
	if st.Sessions == nil {
		st.Sessions = map[string]SessionCursor{}
	}
	w.sess = st
}

// saveSessionStateLocked 落盘 state.json + meta.json（状态/计数镜像，跨进程可读）。
func (w *Workspace) saveSessionStateLocked() error {
	if w.sess.Sessions == nil {
		w.sess.Sessions = map[string]SessionCursor{}
	}
	if err := os.MkdirAll(filepath.FromSlash(w.SessionStore), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(w.sess, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(w.sessionStatePath(), b); err != nil {
		return err
	}
	mb, err := json.MarshalIndent(sessionMetaFile{
		State: w.sess.State, Err: w.sess.Err, Tokenizer: tokenizer,
		Docs: w.sess.Docs, Sessions: len(w.sess.Sessions), LastIndexedAt: w.sess.LastIndexedAt,
	}, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(w.sessionMetaPath(), mb)
}

// collDocCount 集合内 doc 总数（zvec stats；失败 → 0）。
func collDocCount(coll *zvec.Collection) int {
	st, err := coll.GetStats()
	if err != nil || st == nil {
		return 0
	}
	return int(st.DocCount)
}

// insertSessionDocs 分批写入会话 doc。
func insertSessionDocs(coll *zvec.Collection, docs []SessionDoc) error {
	const batch = 64
	for i := 0; i < len(docs); i += batch {
		end := i + batch
		if end > len(docs) {
			end = len(docs)
		}
		items := make([]*zvec.Doc, 0, end-i)
		for _, d := range docs[i:end] {
			doc := zvec.NewDoc()
			doc.SetPK(sessionPK(d.SessionID, d.MessageID))
			_ = doc.AddStringField(sfieldPath, sessionDocPath(d.SessionID, d.MessageID))
			_ = doc.AddStringField(sfieldContent, d.Text)
			_ = doc.AddStringField(sfieldSession, d.SessionID)
			if d.DialogID != "" {
				_ = doc.AddStringField(sfieldDialog, d.DialogID)
			}
			if d.TurnID != "" {
				_ = doc.AddStringField(sfieldTurn, d.TurnID)
			}
			_ = doc.AddStringField(sfieldMessage, d.MessageID)
			_ = doc.AddStringField(sfieldRole, d.Role)
			_ = doc.AddInt64Field(sfieldTS, d.TS)
			items = append(items, doc)
		}
		_, err := coll.Insert(items)
		for _, d := range items {
			d.Destroy()
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// IndexSessions 写/删会话索引（按 message_id 先删后插，幂等）。
// docs = 待写入（同主键旧 doc 先删）；removes = 待删消息（会话 id + 消息 id）。
func (w *Workspace) IndexSessions(docs []SessionDoc, removes []SessionRemove) (*SessionIndexResult, error) {
	w.smu.Lock()
	defer w.smu.Unlock()

	valid := make([]SessionDoc, 0, len(docs))
	seen := map[string]bool{}
	for _, d := range docs {
		d.SessionID = strings.TrimSpace(d.SessionID)
		d.MessageID = strings.TrimSpace(d.MessageID)
		d.Role = strings.ToLower(strings.TrimSpace(d.Role))
		d.Text = strings.TrimSpace(d.Text)
		if d.SessionID == "" || d.MessageID == "" || d.Text == "" {
			continue
		}
		pk := sessionPK(d.SessionID, d.MessageID)
		if seen[pk] {
			continue
		}
		seen[pk] = true
		valid = append(valid, d)
	}

	// 待删主键 = removes ∪ docs 的旧主键（先删后插 → 幂等）
	delIDs := make([]string, 0, len(removes)+len(valid))
	delSeen := map[string]bool{}
	addDel := func(sid, mid string) {
		if sid == "" || mid == "" {
			return
		}
		pk := sessionPK(sid, mid)
		if delSeen[pk] {
			return
		}
		delSeen[pk] = true
		delIDs = append(delIDs, pk)
	}
	for _, r := range removes {
		addDel(strings.TrimSpace(r.SessionID), strings.TrimSpace(r.MessageID))
	}
	for _, d := range valid {
		addDel(d.SessionID, d.MessageID)
	}
	if len(valid) == 0 && len(delIDs) == 0 {
		w.loadSessionStateLocked()
		return &SessionIndexResult{Total: w.sess.Docs}, nil
	}

	coll, err := w.ensureSessionCollLocked()
	if err != nil {
		return nil, err
	}
	// 实际删除数 = 确实存在的旧 doc 数（Fetch 未命中的主键不计数）
	removedActual := len(delIDs)
	if existing, ferr := coll.Fetch(delIDs, nil); ferr == nil {
		removedActual = len(existing)
		zvec.FreeDocs(existing)
	}
	if err := deleteChunks(coll, delIDs); err != nil {
		return nil, fmt.Errorf("删除旧会话 doc: %w", err)
	}
	if len(valid) > 0 {
		if err := insertSessionDocs(coll, valid); err != nil {
			return nil, fmt.Errorf("写入会话 doc: %w", err)
		}
	}
	if err := coll.Flush(); err != nil {
		return nil, fmt.Errorf("flush 会话集合: %w", err)
	}
	docTotal := collDocCount(coll)

	// 更新 state：每会话游标（本批最大 ts 所在轮）、总 doc 数、状态/时间。
	w.loadSessionStateLocked()
	if w.sess.Sessions == nil {
		w.sess.Sessions = map[string]SessionCursor{}
	}
	addMaxTS := map[string]int64{}
	addCursor := map[string]string{}
	for _, d := range valid {
		if d.TS >= addMaxTS[d.SessionID] {
			addMaxTS[d.SessionID] = d.TS
			if d.TurnID != "" {
				addCursor[d.SessionID] = d.TurnID
			}
		}
	}
	now := time.Now().UnixMilli()
	for _, d := range valid {
		c := w.sess.Sessions[d.SessionID]
		c.UpdatedAt = now
		w.sess.Sessions[d.SessionID] = c
	}
	for sid, cur := range addCursor {
		c := w.sess.Sessions[sid]
		c.IndexedUntilTurn = cur
		c.UpdatedAt = now
		w.sess.Sessions[sid] = c
	}
	w.sess.Docs = docTotal
	w.sess.State = "ready"
	w.sess.Err = ""
	w.sess.LastIndexedAt = now
	if err := w.saveSessionStateLocked(); err != nil {
		return nil, err
	}
	return &SessionIndexResult{Added: len(valid), Removed: removedActual, Total: w.sess.Docs}, nil
}

// ClearSessions 清空会话索引：session_id 为空 = 清整个集合（连集合目录）；否则只删该会话。
func (w *Workspace) ClearSessions(sessionID string) (int, error) {
	w.smu.Lock()
	defer w.smu.Unlock()
	sessionID = strings.TrimSpace(sessionID)

	if sessionID == "" {
		w.loadSessionStateLocked()
		removed := w.sess.Docs
		if c := w.sessColl.Load(); c != nil {
			if n := collDocCount(c); n > 0 {
				removed = n
			}
			_ = c.Close()
			w.sessColl.Store(nil)
		}
		if err := os.RemoveAll(w.sessionCollPath()); err != nil {
			return 0, fmt.Errorf("清除会话集合目录: %w", err)
		}
		w.sess = sessionStateFile{Sessions: map[string]SessionCursor{}}
		if err := w.saveSessionStateLocked(); err != nil {
			return removed, err
		}
		return removed, nil
	}

	coll, err := w.openSessionCollLocked()
	if err != nil {
		if errors.Is(err, ErrSessionNotInitialized) {
			return 0, nil
		}
		return 0, err
	}
	pks, err := sessionPKsOf(coll, sessionID)
	if err != nil {
		return 0, err
	}
	if len(pks) > 0 {
		if err := deleteChunks(coll, pks); err != nil {
			return 0, fmt.Errorf("删除会话 doc: %w", err)
		}
		if err := coll.Flush(); err != nil {
			return 0, fmt.Errorf("flush 会话集合: %w", err)
		}
	}
	docTotal := collDocCount(coll)
	w.loadSessionStateLocked()
	if w.sess.Sessions == nil {
		w.sess.Sessions = map[string]SessionCursor{}
	}
	delete(w.sess.Sessions, sessionID)
	w.sess.Docs = docTotal
	if len(w.sess.Sessions) == 0 {
		w.sess.State = ""
	}
	if err := w.saveSessionStateLocked(); err != nil {
		return len(pks), err
	}
	return len(pks), nil
}

// sessionPKsOf 扫描集合，返回属于该会话的全部 doc 主键（会话级清理用）。
func sessionPKsOf(coll *zvec.Collection, sessionID string) ([]string, error) {
	opts := zvec.NewIteratorOptions()
	if opts == nil {
		return nil, errors.New("创建迭代器参数失败")
	}
	defer opts.Destroy()
	_ = opts.SetOutputFields([]string{sfieldSession, sfieldMessage})
	_ = opts.SetIncludeVector(false)
	it, err := coll.CreateIterator(opts)
	if err != nil {
		return nil, fmt.Errorf("遍历会话集合: %w", err)
	}
	defer it.Close()
	var out []string
	for {
		d, err := it.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("遍历会话集合: %w", err)
		}
		sid, _ := d.GetStringField(sfieldSession)
		pk := d.GetPK()
		d.Destroy()
		if sid != sessionID {
			continue
		}
		if pk == "" {
			continue
		}
		out = append(out, pk)
	}
	return out, nil
}

// SessionStatus 会话索引状态（读 state.json；不触达 zvec，配置页/状态栏轮询安全）。
func (w *Workspace) SessionStatus() SessionStatus {
	w.smu.Lock()
	defer w.smu.Unlock()
	w.loadSessionStateLocked()
	return SessionStatus{
		State:         w.sess.State,
		Sessions:      len(w.sess.Sessions),
		Docs:          w.sess.Docs,
		LastIndexedAt: w.sess.LastIndexedAt,
	}
}

// SessionCursors 返回该 workdir 各会话的索引游标（插件全量重建断点续建用）。
func SessionCursors(workdir string) (map[string]SessionCursor, error) {
	abs, err := filepath.Abs(workdir)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(filepath.Join(filepath.FromSlash(SessionStoreDir(filepath.ToSlash(abs))), sessionStateName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]SessionCursor{}, nil
		}
		return nil, err
	}
	var st sessionStateFile
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("state.json 解析失败: %w", err)
	}
	if st.Sessions == nil {
		st.Sessions = map[string]SessionCursor{}
	}
	return st.Sessions, nil
}

// RecordSessionFingerprints 记录若干会话「已索引内容」的稳定指纹（内部方法，非 MQ / 非工具面）。
//
// 插件后台对账（D-74）用：每轮把各会话按「消息 id + role + 正文」算出的 sha256 与
// state.json 里的值比对，不一致即重建该会话索引后回写；一次调用只落一次盘。
func RecordSessionFingerprints(workdir string, fps map[string]string) error {
	if len(fps) == 0 {
		return nil
	}
	w, err := Open(workdir)
	if err != nil {
		return err
	}
	return w.RecordSessionFingerprints(fps)
}

// RecordSessionFingerprints 同包级函数（持 smu，从盘刷新后合并写回）。
func (w *Workspace) RecordSessionFingerprints(fps map[string]string) error {
	w.smu.Lock()
	defer w.smu.Unlock()
	w.loadSessionStateLocked()
	if w.sess.Sessions == nil {
		w.sess.Sessions = map[string]SessionCursor{}
	}
	now := time.Now().UTC().UnixMilli()
	for sid, fp := range fps {
		sid = strings.TrimSpace(sid)
		if sid == "" {
			continue
		}
		c := w.sess.Sessions[sid]
		c.Fingerprint = fp
		c.IndexedAt = now
		w.sess.Sessions[sid] = c
	}
	return w.saveSessionStateLocked()
}

// QuerySessions 检索会话集合：session_id 空 = 全部会话；role 空 = user+assistant 均返回。
// 命中按相关度取 topK，再按 ts 升序展示（会话内上下跳转口径）。
func (w *Workspace) QuerySessions(match, expr string, topK int, sessionID, role string) ([]SessionHit, error) {
	hits, _, _, _, err := w.QuerySessionsPage(match, expr, topK, sessionID, role)
	return hits, err
}

// QuerySessionsPage 是 QuerySessions 的统计版本：
// matched = 本地 session_id/role 过滤后的命中数（截断前），truncated = matched > 返回数
// 或候选已达上限（保守值）；filterApplied 表示 session_id 的服务端 filter 是否生效
// （false 时已回退为“全量候选 + 本地过滤”，目标会话可能少召回）。
// 语义修正：先按相关度取 topK，再按 ts 升序展示（此前先 ts 排序再截断会丢掉高分晚消息）。
func (w *Workspace) QuerySessionsPage(match, expr string, topK int, sessionID, role string) ([]SessionHit, int, bool, bool, error) {
	if topK <= 0 {
		topK = 20
	}
	role = strings.ToLower(strings.TrimSpace(role))
	w.smu.Lock()
	defer w.smu.Unlock()
	coll, err := w.openSessionCollLocked()
	if err != nil {
		return nil, 0, false, false, err
	}
	rawTopK := topK * 20
	if rawTopK > 1000 {
		rawTopK = 1000
	}
	hits, filterApplied, err := searchSessionChunks(coll, match, expr, rawTopK, sessionID)
	if err != nil {
		return nil, 0, false, false, err
	}
	filtered := make([]SessionHit, 0, topK)
	for _, h := range hits {
		if sessionID != "" && h.SessionID != sessionID {
			continue
		}
		if role != "" && h.Role != role {
			continue
		}
		filtered = append(filtered, SessionHit{
			SessionID: h.SessionID, DialogID: h.DialogID, TurnID: h.TurnID,
			MessageID: h.MessageID, Role: h.Role, TS: h.TS,
			Snippet: h.Snippet, Score: h.Score,
		})
	}
	matched := len(filtered)
	if len(filtered) > topK {
		filtered = filtered[:topK]
	}
	sort.SliceStable(filtered, func(i, j int) bool { return filtered[i].TS < filtered[j].TS })
	truncated := matched > len(filtered) || len(hits) >= rawTopK
	return filtered, matched, truncated, filterApplied, nil
}

// rawSessionHit 会话检索的原始命中（未排序/未按 role 过滤）。
type rawSessionHit struct {
	SessionID string
	DialogID  string
	TurnID    string
	MessageID string
	Role      string
	TS        int64
	Snippet   string
	Score     float32
}

// searchSessionChunks 执行一次会话 FTS 查询。
//
// 过滤：先带服务端 filter（session_id == '…'；可用则缩小候选、保证召回）；若该能力不可用
// （SetFilter / Query 报错）→ 去掉 filter 重试。两条路径的命中在 QuerySessions 里都会再按
// session_id / role 过滤，**正确性不依赖 filter 是否被支持**；返回的 bool 表示服务端 filter
// 是否真正生效（false = 回退路径，目标会话可能被其它会话挤出候选）。
func searchSessionChunks(coll *zvec.Collection, match, expr string, topK int, sessionID string) ([]rawSessionHit, bool, error) {
	withFilter := sessionID != ""
	hits, err := searchSessionOnce(coll, match, expr, topK, sessionID, withFilter)
	if err != nil && withFilter {
		h2, err2 := searchSessionOnce(coll, match, expr, topK, sessionID, false)
		return h2, false, err2
	}
	return hits, withFilter, err
}

// searchSessionOnce 单次会话查询（filterOn 决定是否下发服务端过滤表达式）。
func searchSessionOnce(coll *zvec.Collection, match, expr string, topK int, sessionID string, filterOn bool) ([]rawSessionHit, error) {
	q := zvec.NewSearchQuery()
	defer q.Destroy()
	if err := q.SetFieldName(sfieldContent); err != nil {
		return nil, err
	}
	if err := q.SetTopK(topK); err != nil {
		return nil, err
	}
	if err := q.SetOutputFields([]string{sfieldPath, sfieldContent, sfieldSession, sfieldDialog, sfieldTurn, sfieldMessage, sfieldRole, sfieldTS}); err != nil {
		return nil, err
	}
	if filterOn && sessionID != "" {
		if err := q.SetFilter(sfieldSession + " = " + quoteFilterValue(sessionID)); err != nil {
			return nil, err
		}
	}

	terms := queryTerms(match, expr)
	fts := zvec.NewFTS()
	defer fts.Destroy()
	if match != "" {
		if err := fts.SetMatchString(match); err != nil {
			return nil, err
		}
	}
	if expr != "" {
		if err := fts.SetQueryString(expr); err != nil {
			return nil, err
		}
	}
	if err := q.SetFTS(fts); err != nil {
		return nil, err
	}
	docs, err := coll.Query(q)
	if err != nil {
		return nil, err
	}
	defer zvec.FreeDocs(docs)

	out := make([]rawSessionHit, 0, len(docs))
	for _, d := range docs {
		sid, _ := d.GetStringField(sfieldSession)
		dialog, _ := d.GetStringField(sfieldDialog)
		turn, _ := d.GetStringField(sfieldTurn)
		mid, _ := d.GetStringField(sfieldMessage)
		role, _ := d.GetStringField(sfieldRole)
		ts, _ := d.GetInt64Field(sfieldTS)
		txt, _ := d.GetStringField(sfieldContent)
		out = append(out, rawSessionHit{
			SessionID: sid, DialogID: dialog, TurnID: turn, MessageID: mid, Role: role, TS: ts,
			Snippet: sessionSnippet(txt, terms, sessionSnippetChars), Score: d.GetScore(),
		})
	}
	return out, nil
}

// quoteFilterValue 把字符串字面量包成 filter 表达式的单引号形态（转义单引号与反斜杠）。
func quoteFilterValue(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, `'`, `\'`)
	return "'" + v + "'"
}

// sessionSnippet 取命中词前后约 maxChars 字符的正文片段（换行压成空格；无命中 → 开头片段）。
//
// jieba 按词切分，匹配串（如「全文索引」）在正文里未必连续出现，故先找整串，
// 找不到再按「整串的连续子串（长 → 短）」定位（如命中「索引」），保证片段落在命中处。
func sessionSnippet(text string, terms []string, maxChars int) string {
	flat := strings.Join(strings.Fields(strings.ReplaceAll(text, "\r", " ")), " ")
	if flat == "" {
		return ""
	}
	runes := []rune(flat)
	low := strings.ToLower(flat)
	half := maxChars / 2
	for _, t := range terms {
		lt := strings.ToLower(t)
		idx := locateLongestSubstring(low, lt)
		if idx < 0 {
			continue
		}
		// 字节偏移 → rune 偏移
		pos := len([]rune(low[:idx]))
		start := pos - half
		if start < 0 {
			start = 0
		}
		end := start + maxChars
		if end > len(runes) {
			end = len(runes)
			start = end - maxChars
			if start < 0 {
				start = 0
			}
		}
		snippet := strings.TrimSpace(string(runes[start:end]))
		if start > 0 {
			snippet = "…" + snippet
		}
		if end < len(runes) {
			snippet += "…"
		}
		return snippet
	}
	return truncate(flat, maxChars)
}

// locateLongestSubstring 在 haystack 中定位 needle 或其**最长**连续子串（≥2 字符）的位置；
// 均未命中 → -1。用于 jieba 分词命中原词但正文非连续出现的场景。
func locateLongestSubstring(haystack, needle string) int {
	if needle == "" {
		return -1
	}
	if i := strings.Index(haystack, needle); i >= 0 {
		return i
	}
	n := []rune(needle)
	for size := len(n) - 1; size >= 2; size-- {
		for off := 0; off+size <= len(n); off++ {
			if i := strings.Index(haystack, string(n[off:off+size])); i >= 0 {
				return i
			}
		}
	}
	return -1
}
