package desktop

import (
	"testing"
)

// TestParseKeySpec 组合键解析（不注入，纯解析）。
func TestParseKeySpec(t *testing.T) {
	cases := []struct {
		spec    string
		mods    []uint16
		main    uint16
		wantErr bool
	}{
		{"enter", nil, 0x0D, false},
		{"ctrl", nil, 0x11, false},
		{"ctrl+s", []uint16{0x11}, 0x53, false},
		{"alt+f4", []uint16{0x12}, 0x73, false},
		{"win+r", []uint16{0x5B}, 0x52, false},
		{"shift+enter", []uint16{0x10}, 0x0D, false},
		{"ctrl+shift+s", []uint16{0x11, 0x10}, 0x53, false},
		{"lwin+r", []uint16{0x5B}, 0x52, false},
		{"", nil, 0, true},
		{"ctrl+", nil, 0, true},
		{"xyz+enter", nil, 0, true},
		{"nope", nil, 0, true},
		{"ctrl+unknown", nil, 0, true},
	}
	for _, c := range cases {
		mods, main, err := parseKeySpec(c.spec)
		if c.wantErr {
			if err == nil {
				t.Errorf("%q: want err, got mods=%v main=%v", c.spec, mods, main)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: unexpected err %v", c.spec, err)
			continue
		}
		if main != c.main {
			t.Errorf("%q: main=%x want %x", c.spec, main, c.main)
		}
		if len(mods) != len(c.mods) {
			t.Errorf("%q: mods=%v want %v", c.spec, mods, c.mods)
			continue
		}
		for i := range mods {
			if mods[i] != c.mods[i] {
				t.Errorf("%q: mods[%d]=%x want %x", c.spec, i, mods[i], c.mods[i])
			}
		}
	}
}

// TestImeArg IME 参数解析。
func TestImeArg(t *testing.T) {
	for _, s := range []string{"en", "off", "ascii", "EN"} {
		if open, err := imeArg(s); err != nil || open {
			t.Errorf("imeArg(%q) = %v,%v want false,nil", s, open, err)
		}
	}
	for _, s := range []string{"cn", "on", "zh", "chinese", "CN"} {
		if open, err := imeArg(s); err != nil || !open {
			t.Errorf("imeArg(%q) = %v,%v want true,nil", s, open, err)
		}
	}
	if _, err := imeArg("jp"); err == nil {
		t.Errorf("imeArg(jp) want err")
	}
}
