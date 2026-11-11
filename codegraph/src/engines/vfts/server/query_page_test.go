package server

import "testing"

func TestQueryPageMatchedAndTruncated(t *testing.T) {
	w, _ := newIndexedWorkspace(t, tfFiles())

	hits, matched, truncated, err := w.QueryPage("nectarine", "", 2, "")
	if err != nil {
		t.Fatalf("query page: %v", err)
	}
	if len(hits) != 2 || matched != 3 || !truncated {
		t.Fatalf("topK=2: hits=%d matched=%d truncated=%v", len(hits), matched, truncated)
	}
	if hits[0].Path != w.abs("hot.txt") || hits[1].Path != w.abs("warm.txt") {
		t.Fatalf("应按相关度取前 2: %+v", hits)
	}

	hits, matched, truncated, err = w.QueryPage("nectarine", "", 10, "")
	if err != nil {
		t.Fatalf("query page all: %v", err)
	}
	if len(hits) != 3 || matched != 3 || truncated {
		t.Fatalf("topK=10: hits=%d matched=%d truncated=%v", len(hits), matched, truncated)
	}
}

func TestQuerySessionsPageMatchedAndTruncated(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { Drop(dir); CloseAll() }()
	if _, err := w.IndexSessions(seedSessionDocs(), nil); err != nil {
		t.Fatalf("index: %v", err)
	}

	hits, matched, truncated, filterApplied, err := w.QuerySessionsPage("全文索引", "", 1, "s1", "")
	if err != nil {
		t.Fatalf("session page: %v", err)
	}
	if len(hits) != 1 || matched < 1 || !truncated {
		t.Fatalf("topK=1: hits=%d matched=%d truncated=%v", len(hits), matched, truncated)
	}
	_ = filterApplied

	hits, matched, truncated, _, err = w.QuerySessionsPage("全文索引", "", 20, "", "")
	if err != nil {
		t.Fatalf("session page all: %v", err)
	}
	if len(hits) != 3 || matched != 3 || truncated {
		t.Fatalf("topK=20: hits=%d matched=%d truncated=%v", len(hits), matched, truncated)
	}
	for i := 0; i+1 < len(hits); i++ {
		if hits[i].TS > hits[i+1].TS {
			t.Fatalf("返回展示应按 ts 升序: %+v", hits)
		}
	}
}
