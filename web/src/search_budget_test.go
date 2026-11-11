package main

import "testing"

func TestApplySearchContentBudget(t *testing.T) {
	items := []result{
		{Title: "a", Content: "12345"},
		{Title: "b", Content: "abcdef"},
		{Title: "c", Content: "xyz"},
	}
	used, truncated := applySearchContentBudget(items, 8)
	if used != 8 || !truncated {
		t.Fatalf("used=%d truncated=%v", used, truncated)
	}
	if items[0].Content != "12345" || items[0].ContentTruncated {
		t.Fatalf("a=%+v", items[0])
	}
	if items[1].Content != "abc" || !items[1].ContentTruncated {
		t.Fatalf("b=%+v", items[1])
	}
	if items[2].Content != "" || !items[2].ContentTruncated {
		t.Fatalf("c=%+v", items[2])
	}

	used, truncated = applySearchContentBudget([]result{{Title: "a"}, {Title: "b"}}, 100)
	if used != 0 || truncated {
		t.Fatalf("无正文不应计: used=%d truncated=%v", used, truncated)
	}
	if len(items) != 3 {
		t.Fatalf("items=%v", items)
	}
}

func TestAPIAttemptTimeoutUnderGatewayLimit(t *testing.T) {
	if apiAttemptTimeoutMax.Seconds()*3+3 >= 180 {
		t.Fatalf("三次尝试 + 退避必须 < 180s: %v", apiAttemptTimeoutMax)
	}
}
