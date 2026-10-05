package wafcaptcha

import (
	"SamWaf/model"
	"testing"
)

// TestModernOptionsMapsBindingFields 校验 modernOptions 对两个新配置字段的映射：
// DynamicEscalation 三态（nil=默认 true；显式 false 只在指针非 nil 且为 false 时映射 false）
// 与 InstrumentationCheckCount 的 FlexInt→int 转换（0=引擎默认解析，不在此改写）。
// 引擎侧语义（0→阻断开 18/关 8；混淆 0→阻断开 6/关 3；观察模式 block=false 仍收集
// 检查并记录判定、只不阻断）由 cap_go_server 契约实现并在其侧测试。
func TestModernOptionsMapsBindingFields(t *testing.T) {
	cfg := model.DefaultCaptchaConfig()
	opts := modernOptions(cfg)
	if !opts.DynamicEscalation {
		t.Fatal("nil dynamicEscalation must default to true")
	}
	if opts.InstrumentationOptions.CheckCount != 0 {
		t.Fatalf("default check count must be 0 (engine resolves), got %d", opts.InstrumentationOptions.CheckCount)
	}
	if opts.InstrumentationOptions.BlockAutomatedBrowsers {
		t.Fatal("default blockAutomatedBrowsers must be false")
	}
	explicitFalse := false
	cfg.CapJsConfig.DynamicEscalation = &explicitFalse
	cfg.CapJsConfig.InstrumentationCheckCount = 18
	opts = modernOptions(cfg)
	if opts.DynamicEscalation {
		t.Fatal("explicit dynamicEscalation=false must map to false")
	}
	if opts.InstrumentationOptions.CheckCount != 18 {
		t.Fatalf("instrumentationCheckCount not mapped: %d", opts.InstrumentationOptions.CheckCount)
	}
	explicitTrue := true
	cfg.CapJsConfig.DynamicEscalation = &explicitTrue
	cfg.CapJsConfig.InstrumentationCheckCount = 8
	opts = modernOptions(cfg)
	if !opts.DynamicEscalation {
		t.Fatal("explicit dynamicEscalation=true must map to true")
	}
	if opts.InstrumentationOptions.CheckCount != 8 {
		t.Fatalf("instrumentationCheckCount not mapped: %d", opts.InstrumentationOptions.CheckCount)
	}
	// 模型层保证 0..18，这里只做直通映射；越界值在保存期已被拒绝。
	cfg.CapJsConfig.InstrumentationCheckCount = 1
	if got := modernOptions(cfg).InstrumentationOptions.CheckCount; got != 1 {
		t.Fatalf("instrumentationCheckCount mapping changed the value: %d", got)
	}
}
