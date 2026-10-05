package model

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestCaptchaModernLegacyMigration(t *testing.T) {
	for _, engine := range []string{"traditional", "capJs", "default"} {
		raw := `{"engine_type":"` + engine + `","expire_time":"12","exclude_urls":["/api"," /pay "],"cap_js_config":{"challengeCount":"60","challengeSize":"24","challengeDifficulty":"3","expiresMs":"120000"}}`
		out, err := NormalizeCaptchaJSON(raw, "/_captcha")
		if err != nil {
			t.Fatal(err)
		}
		c, err := DecodeCaptchaConfig(out)
		if err != nil {
			t.Fatal(err)
		}
		p := c.CapJsConfig
		if p.Format != 1 || !reflect.DeepEqual(p.Protocols, []string{"sha256-pow"}) || p.ChallengeCount != 60 || p.ChallengeSize != 24 || p.ChallengeDifficulty != 3 || p.ExpiresMs != 120000 {
			t.Fatalf("migration changed legacy cost: %+v", p)
		}
		if c.ExpireTime != 12 || c.ExcludeURLs != "/api\n/pay" || c.PathPrefix != "/_captcha" {
			t.Fatalf("lost legacy fields: %+v", c)
		}
		if engine == "default" {
			engine = "traditional"
		}
		if c.EngineType != engine {
			t.Fatalf("wrong engine: %s", c.EngineType)
		}
		out2, err := NormalizeCaptchaJSON(out, "/different")
		if err != nil || out2 != out {
			t.Fatalf("not idempotent: %v\n%s\n%s", err, out, out2)
		}
	}
}

func TestCaptchaModernDefaultsAndInstrumentation(t *testing.T) {
	for _, raw := range []string{"", "{}", `{"cap_js_config":{"format":"0","hashWXDifficulty":0,"hashWXNoncesPerHash":"0","hashWXChallengeCount":0,"rswT":0,"instrumentationOptions":{"obfuscationLevel":0}}}`} {
		c, err := DecodeCaptchaConfig(raw)
		if err != nil {
			t.Fatal(err)
		}
		p := c.CapJsConfig
		if c.ExpireTime != 24 || p.Format != 1 || p.ChallengeCount != 50 || p.ChallengeSize != 32 || p.ChallengeDifficulty != 4 || p.ExpiresMs != 600000 || p.HashWXDifficulty != 1000000 || p.HashWXNoncesPerHash != 65536 || p.HashWXChallengeCount != 4 || p.RSWT != 75000 || p.InstrumentationOptions.ObfuscationLevel != 3 || p.InstrumentationOptions.BlockAutomatedBrowsers {
			t.Fatalf("bad defaults: %+v", c)
		}
	}
	for _, raw := range []string{
		`{"cap_js_config":{"format":2,"protocols":["hashwx","rsw"],"instrumentation":true}}`,
		`{"cap_js_config":{"format":2,"protocols":["hashwx","rsw","instrumentation"]}}`,
		`{"cap_js_config":{"format":2,"protocols":["hashwx","rsw","instrumentation"],"instrumentation":true}}`,
	} {
		c, err := DecodeCaptchaConfig(raw)
		if err != nil {
			t.Fatal(err)
		}
		if !c.CapJsConfig.Instrumentation || !reflect.DeepEqual(c.CapJsConfig.Protocols, []string{"hashwx", "rsw", "instrumentation"}) {
			t.Fatalf("wrong instrumentation: %+v", c.CapJsConfig)
		}
	}
	c, err := DecodeCaptchaConfig(`{"cap_js_config":{"format":1,"instrumentation":true,"instrumentationOptions":{"blockAutomatedBrowsers":true,"obfuscationLevel":"2"}}}`)
	if err != nil {
		t.Fatal(err)
	}
	if !c.CapJsConfig.Instrumentation || !c.CapJsConfig.InstrumentationOptions.BlockAutomatedBrowsers || c.CapJsConfig.InstrumentationOptions.ObfuscationLevel != 2 || !reflect.DeepEqual(c.CapJsConfig.Protocols, []string{"sha256-pow"}) {
		t.Fatalf("format1 instrumentation broken: %+v", c.CapJsConfig)
	}
}

func TestCaptchaModernRejectsInvalidJSONAndProtocols(t *testing.T) {
	cases := []string{
		`null`, `[]`, `"bad"`, `{"expire_time":`, `{} {}`,
		`{"new_future_field":true}`, `{"cap_js_config":{"typo":1}}`,
		`{"cap_js_config":{"instrumentationOptions":{"future":1}}}`,
		`{"cap_js_config":{"infoTitle":{"fr":"hello"}}}`,
		`{"expire_time":2,"expire_time":3}`, `{"expire_time":2,"EXPIRE_TIME":3}`,
		`{"cap_js_config":{"format":1,"format":2}}`,
		`{"expire_time":"oops","is_enable_captcha":1}`,
		`{"cap_js_config":{"challengeCount":1.1}}`,
		`{"cap_js_config":{"instrumentation":"true"}}`,
		`{"cap_js_config":{"format":3}}`, `{"cap_js_config":{"format":-1}}`,
		`{"cap_js_config":{"protocols":[]}}`,
		`{"cap_js_config":{"format":2,"protocols":[],"instrumentation":true}}`,
		`{"cap_js_config":{"format":2,"protocols":["unknown"]}}`,
		`{"cap_js_config":{"format":2,"protocols":[""]}}`,
		`{"cap_js_config":{"format":2,"protocols":["sha256-pow",""]}}`,
		`{"cap_js_config":{"format":2,"protocols":["SHA256-POW"]}}`,
		`{"cap_js_config":{"format":2,"protocols":["hashwx","hashwx"]}}`,
		`{"cap_js_config":{"protocols":["hashwx"]}}`,
		`{"cap_js_config":{"protocols":["instrumentation"]}}`,
		`{"engine_type":"unknown"}`, `{"ip_mode":"unknown"}`, `{"is_enable_captcha":2}`,
	}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			if out, err := NormalizeCaptchaJSON(raw, "/valid"); err == nil || out != "" {
				t.Fatalf("invalid input accepted: %s => %s (%v)", raw, out, err)
			}
		})
	}
}

func TestCaptchaModernNumericBounds(t *testing.T) {
	for _, tc := range []struct {
		name        string
		min, max    int
		top         bool
		zeroDefault bool
	}{
		{"expire_time", 1, 720, true, false},
		{"challengeCount", 1, 1000, false, false}, {"challengeSize", 1, 256, false, false}, {"challengeDifficulty", 1, 16, false, false},
		{"expiresMs", 1000, 3600000, false, false},
		{"hashWXDifficulty", 1, 1000000000, false, true}, {"hashWXNoncesPerHash", 1, 1048576, false, true}, {"hashWXChallengeCount", 1, 64, false, true},
		{"rswT", 1, 300000, false, true},
	} {
		for _, value := range []int{-1, 0, tc.min, tc.max, tc.max + 1} {
			fields := map[string]interface{}{tc.name: value}
			if !tc.top {
				fields = map[string]interface{}{"cap_js_config": fields}
			}
			b, err := json.Marshal(fields)
			if err != nil {
				t.Fatal(err)
			}
			_, err = DecodeCaptchaConfig(string(b))
			valid := value >= tc.min && value <= tc.max || value == 0 && tc.zeroDefault
			if (err == nil) != valid {
				t.Fatalf("%s=%d valid=%v err=%v", tc.name, value, valid, err)
			}
		}
	}
	for _, level := range []string{"-1", "11", "100"} {
		if _, err := DecodeCaptchaConfig(`{"cap_js_config":{"instrumentationOptions":{"obfuscationLevel":` + level + `}}}`); err == nil {
			t.Fatalf("unsupported obfuscation accepted: %s", level)
		}
	}
}

func TestCaptchaModernInstrumentationAllLevels(t *testing.T) {
	for level := 0; level <= 10; level++ {
		for _, format := range []FlexInt{1, 2} {
			c := DefaultCaptchaConfig()
			c.CapJsConfig.Format = format
			c.CapJsConfig.Instrumentation = true
			c.CapJsConfig.InstrumentationOptions.ObfuscationLevel = FlexInt(level)
			b, err := json.Marshal(c)
			if err != nil {
				t.Fatal(err)
			}
			out, err := NormalizeCaptchaJSON(string(b))
			if err != nil {
				t.Fatalf("format=%d level=%d: %v", format, level, err)
			}
			got, err := DecodeCaptchaConfig(out)
			want := FlexInt(level)
			if level == 0 {
				want = 3
			}
			if err != nil || got.CapJsConfig.InstrumentationOptions.ObfuscationLevel != want {
				t.Fatalf("format=%d level=%d changed: got=%d err=%v", format, level, got.CapJsConfig.InstrumentationOptions.ObfuscationLevel, err)
			}
		}
	}
}

func TestCaptchaModernPathsAndContact(t *testing.T) {
	for _, prefix := range []string{"/", "/_waf_ABC-012", "/nested/path/", "/a.b~c"} {
		b, _ := json.Marshal(map[string]string{"path_prefix": prefix})
		out, err := NormalizeCaptchaJSON(string(b), "/fallback")
		if err != nil {
			t.Fatal(err)
		}
		c, err := DecodeCaptchaConfig(out)
		if err != nil || c.PathPrefix != prefix {
			t.Fatalf("prefix changed: %q => %q (%v)", prefix, c.PathPrefix, err)
		}
	}
	for _, prefix := range []string{"relative", "//evil", "/a/../b", "/a..b", "/a/./b", "/a%2fb", "/%252e%252e", "/a\\b", "/a\"b", "/a'b", "/a?b", "/a#b", "/a\nb", " /a", "/a ", "/" + strings.Repeat("x", 256)} {
		if err := ValidateCaptchaPathPrefix(prefix); err == nil {
			t.Fatalf("unsafe path accepted: %q", prefix)
		}
	}
	if _, err := NormalizeCaptchaJSON("{}", "//bad"); err == nil {
		t.Fatal("unsafe fallback accepted")
	}
	b, _ := json.Marshal(map[string]string{"contact_info": "  " + strings.Repeat("界", 200) + "  "})
	if _, err := NormalizeCaptchaJSON(string(b)); err != nil {
		t.Fatal(err)
	}
	b, _ = json.Marshal(map[string]string{"contact_info": strings.Repeat("界", 201)})
	if _, err := NormalizeCaptchaJSON(string(b)); err == nil {
		t.Fatal("overlong contact accepted")
	}
	if len([]rune(ParseCaptchaConfig(string(b)).ContactInfo)) != 200 {
		t.Fatal("legacy runtime contact truncation changed")
	}
}

func TestCaptchaModernNoPartialDecode(t *testing.T) {
	for _, raw := range []string{
		`{"is_enable_captcha":1,"expire_time":"bad","cap_js_config":{"challengeCount":2}}`,
		`{"cap_js_config":{"challengeCount":2,"challengeSize":"bad"}}`,
	} {
		if got := ParseCaptchaConfig(raw); !reflect.DeepEqual(got, DefaultCaptchaConfig()) {
			t.Fatalf("partial decode escaped: %+v", got)
		}
		if got, err := DecodeCaptchaConfig(raw); err == nil || !reflect.DeepEqual(got, CaptchaConfig{}) {
			t.Fatalf("strict partial decode escaped: %+v %v", got, err)
		}
	}
}

func TestCaptchaModernValidateDoesNotMutate(t *testing.T) {
	c := DefaultCaptchaConfig()
	c.CapJsConfig.Format = 2
	c.CapJsConfig.Instrumentation = true
	c.CapJsConfig.Protocols = make([]string, 1, 4)
	c.CapJsConfig.Protocols[0] = "hashwx"
	if err := ValidateCaptchaConfig(c); err != nil {
		t.Fatal(err)
	}
	if len(c.CapJsConfig.Protocols) != 1 || c.CapJsConfig.Protocols[:2][1] != "" {
		t.Fatal("validation mutated caller protocol backing array")
	}
}

func TestCaptchaModernPassBindingEnumAndRuntimeFallback(t *testing.T) {
	// 保存期：三个合法值全部通过并原样回写。
	for _, binding := range []string{"", CaptchaPassBindingIP, CaptchaPassBindingIPTLS} {
		b, err := json.Marshal(map[string]string{"passBinding": binding})
		if err != nil {
			t.Fatal(err)
		}
		out, err := NormalizeCaptchaJSON(string(b), "/_captcha")
		if err != nil {
			t.Fatalf("passBinding=%q rejected at save: %v", binding, err)
		}
		c, err := DecodeCaptchaConfig(out)
		if err != nil || c.PassBinding != binding {
			t.Fatalf("passBinding=%q round-trip got %q (%v)", binding, c.PassBinding, err)
		}
	}
	// 保存期：枚举外取值（大小写变体、空白、未知模式）一律明确报错。
	for _, binding := range []string{"IP", " ip", "ip ", "tls", "ip+TLS", "ip+tls ", "mac", "ip+http"} {
		b, err := json.Marshal(map[string]string{"passBinding": binding})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := NormalizeCaptchaJSON(string(b), "/_captcha"); err == nil {
			t.Fatalf("invalid passBinding accepted: %q", binding)
		}
	}
	// 运行期 Parse：非法存量值归一为默认 "ip"（安全非破坏回退），
	// 不整份回落默认配置、不丢其它字段。
	raw := `{"engine_type":"capJs","expire_time":"12","cap_js_config":{"challengeCount":"60"},"passBinding":"bogus"}`
	got := ParseCaptchaConfig(raw)
	if got.PassBinding != CaptchaPassBindingIP {
		t.Fatalf("runtime fallback passBinding=%q", got.PassBinding)
	}
	if got.EngineType != CaptchaEngineCapJs || got.ExpireTime != 12 || got.CapJsConfig.ChallengeCount != 60 {
		t.Fatalf("runtime fallback dropped unrelated fields: %+v", got)
	}
}

func TestCaptchaModernSwitchesPersistExplicitFalse(t *testing.T) {
	// blockAutomatedBrowsers 显式 false 必须出现在存储 JSON 里（无字段级 omitempty）。
	out, err := NormalizeCaptchaJSON(`{"cap_js_config":{"instrumentationOptions":{"blockAutomatedBrowsers":false,"obfuscationLevel":3}}}`, "/_captcha")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"blockAutomatedBrowsers":false`) {
		t.Fatalf("explicit blockAutomatedBrowsers=false lost: %s", out)
	}
	if out2, err := NormalizeCaptchaJSON(out, "/other"); err != nil || out2 != out {
		t.Fatalf("explicit false not stable on re-save: %s (%v)", out2, err)
	}
	c, err := DecodeCaptchaConfig(out)
	if err != nil {
		t.Fatal(err)
	}
	if c.CapJsConfig.InstrumentationOptions.BlockAutomatedBrowsers {
		t.Fatal("explicit blockAutomatedBrowsers=false flipped on read")
	}
	// instrumentation 显式 false 同理必须存活。
	out, err = NormalizeCaptchaJSON(`{"cap_js_config":{"instrumentation":false}}`, "/_captcha")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"instrumentation":false`) {
		t.Fatalf("explicit instrumentation=false lost: %s", out)
	}
	if c, err := DecodeCaptchaConfig(out); err != nil || c.CapJsConfig.Instrumentation {
		t.Fatalf("explicit instrumentation=false flipped: %+v (%v)", c.CapJsConfig.Instrumentation, err)
	}
	// dynamicEscalation 三态：nil=默认（存储 JSON 不出现）；true/false 都原样存活。
	if c, err := DecodeCaptchaConfig(out); err != nil || c.CapJsConfig.DynamicEscalation != nil {
		t.Fatalf("dynamicEscalation default must be nil: %v (%v)", c.CapJsConfig.DynamicEscalation, err)
	}
	for _, raw := range []string{
		`{"cap_js_config":{"dynamicEscalation":true}}`,
		`{"cap_js_config":{"dynamicEscalation":false}}`,
	} {
		out, err := NormalizeCaptchaJSON(raw, "/_captcha")
		if err != nil {
			t.Fatal(err)
		}
		c, err := DecodeCaptchaConfig(out)
		if err != nil {
			t.Fatal(err)
		}
		if c.CapJsConfig.DynamicEscalation == nil {
			t.Fatalf("dynamicEscalation round-trip lost: %s", raw)
		}
		want := strings.Contains(raw, "true")
		if *c.CapJsConfig.DynamicEscalation != want || !strings.Contains(out, `"dynamicEscalation":`+boolJSON(want)) {
			t.Fatalf("dynamicEscalation round-trip %s => %v / %s", raw, *c.CapJsConfig.DynamicEscalation, out)
		}
	}
	// 坏类型明确报错，不接受字符串/数字形态。
	for _, raw := range []string{
		`{"cap_js_config":{"dynamicEscalation":"false"}}`,
		`{"cap_js_config":{"dynamicEscalation":0}}`,
	} {
		if _, err := NormalizeCaptchaJSON(raw, "/_captcha"); err == nil {
			t.Fatalf("bad dynamicEscalation accepted: %s", raw)
		}
	}
}

func boolJSON(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func TestCaptchaModernInstrumentationCheckCountBounds(t *testing.T) {
	for value, valid := range map[int]bool{-1: false, 0: true, 1: true, 8: true, 17: true, 18: true, 19: false, 100: false} {
		b, err := json.Marshal(map[string]interface{}{"cap_js_config": map[string]interface{}{"instrumentationCheckCount": value}})
		if err != nil {
			t.Fatal(err)
		}
		_, err = DecodeCaptchaConfig(string(b))
		if (err == nil) != valid {
			t.Fatalf("instrumentationCheckCount=%d valid=%v err=%v", value, valid, err)
		}
	}
	// FlexInt 兼容数字字符串；0=引擎默认解析，不落具体数值。
	c, err := DecodeCaptchaConfig(`{"cap_js_config":{"instrumentationCheckCount":"18"}}`)
	if err != nil || c.CapJsConfig.InstrumentationCheckCount != 18 {
		t.Fatalf("string form rejected: %d (%v)", c.CapJsConfig.InstrumentationCheckCount, err)
	}
	c, err = DecodeCaptchaConfig(`{}`)
	if err != nil || c.CapJsConfig.InstrumentationCheckCount != 0 {
		t.Fatal("default instrumentationCheckCount must be 0 (engine resolves)")
	}
	if out, err := NormalizeCaptchaJSON(`{}`, "/_captcha"); err != nil || strings.Contains(out, "instrumentationCheckCount") {
		t.Fatalf("zero default must stay omitted: %s (%v)", out, err)
	}
}
