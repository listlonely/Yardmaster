// M2-W5：版本比较纯函数测试（update check 的网络部分不在此测——
// 测试不碰网络铁律；HTTP 查询逻辑薄层由发布实测覆盖）。
package app

import "testing"

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.2.0", "0.2.0", 0},
		{"0.1.1", "0.2.0", -1},
		{"0.2.0", "0.1.1", 1},
		{"1.0.0", "0.9.9", 1},
		{"0.10.0", "0.9.0", 1}, // 逐段数值比较，非字典序
	}
	for _, c := range cases {
		if got := compareVersions(c.a, c.b); got != c.want {
			t.Errorf("compareVersions(%q,%q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}
