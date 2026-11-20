package carrier

import "testing"

func TestMatchEEMVNOs(t *testing.T) {
	cases := map[string]string{
		"中国电信 — CTExcel CTExcel": "ctexcel",
		"中国移动 — CTExcel 副卡":      "ctexcel",
		"无服务 — CMLink CMLink2":   "cmlink-uk",
		"EE":                     "ee",
	}
	for name, want := range cases {
		if got := Match("234", "33", name, "894430529").ID; got != want {
			t.Errorf("%q -> %s, want %s", name, got, want)
		}
	}
	if got := Match("234", "20", "3", "").ID; got != "3uk" {
		t.Errorf("3UK -> %s", got)
	}
	if p := Match("999", "99", "", ""); p.ID != "standard" {
		t.Errorf("unknown -> %s", p.ID)
	}
}
