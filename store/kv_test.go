package store

import "testing"

func TestBoolSetting(t *testing.T) {
	for _, v := range []string{"1", "true", "TRUE", " yes ", "on", "enabled"} {
		if !BoolSetting(v, false) {
			t.Errorf("%q 应解析为 true", v)
		}
	}
	for _, v := range []string{"0", "false", "FALSE", " no ", "off", "disabled"} {
		if BoolSetting(v, true) {
			t.Errorf("%q 应解析为 false", v)
		}
	}
	if !BoolSetting("", true) {
		t.Error("空值应使用 fallback=true")
	}
	if BoolSetting("unknown", false) {
		t.Error("未知值应使用 fallback=false")
	}
}
