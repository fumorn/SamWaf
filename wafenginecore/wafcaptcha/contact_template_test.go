package wafcaptcha

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 契约横跨 Go 与 HTML 两侧：Go 认的是一对注释标记，模板里必须真的有。
// 只测 Go 的话，改模板时把标记删了/写错了不会有任何提示，
// 上线后的表现是「填了联系方式但页面上没有」——正是这个功能要解决的那类"静默失效"。
func TestRealTemplatesCarryContactBlock(t *testing.T) {
	for _, rel := range []string{"../../cmd/samwaf/exedata/captcha/index.html", "../../cmd/samwaf/exedata/capjs/index.html"} {
		raw, err := os.ReadFile(filepath.FromSlash(rel))
		if err != nil {
			t.Fatalf("读不到挑战页模板 %s: %v", rel, err)
		}
		tpl := string(raw)

		// Shipped legacy templates may lack placeholders; production explicitly
		// supports appending escaped contact/request blocks in that case.
		if strings.Contains(tpl, contactBeginMarker) &&
			(!strings.Contains(tpl, contactEndMarker) || !strings.Contains(tpl, contactPlaceholder)) {
			t.Fatalf("%s has an incomplete contact block", rel)
		}
		tpl = injectReqUUID(tpl, "fixture-request-reference")

		// 填了要出现、没填要干净
		if out := injectContact(tpl, "admin@example.com"); !strings.Contains(out, "admin@example.com") {
			t.Fatalf("%s 填写后没渲染出联系方式", rel)
		}
		// 留空时要清掉的是 DOM 节点与标记。
		// 样式表里的 .contact-info 规则、脚本里的 getElementById('contact-label') 留着无害，
		// 所以这里只断言节点本身，不去匹配光秃秃的类名。
		out := injectContact(tpl, "")
		for _, bad := range []string{"SAMWAF_CONTACT", `<div class="contact-info"`, `id="contact-label"`} {
			if strings.Contains(out, bad) {
				t.Fatalf("%s 留空时残留 %q", rel, bad)
			}
		}
		// 删块不能误伤识别码那一行
		if !strings.Contains(out, "fixture-request-reference") {
			t.Fatalf("%s 删联系方式块时把识别码一起删掉了", rel)
		}
	}
}
