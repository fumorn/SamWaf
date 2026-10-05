package model

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// CaptchaCapJSConfig 使用原有 camelCase JSON 字段与 FlexInt 数字/字符串兼容。
// Format=0 等价 1；旧 capJs 的 c/s/d 不变，但由新版 SHA-256 format 1 执行。
// 只有显式 format=2 才使用现代多协议挑战；不会根据参数偷偷切换格式。
type CaptchaCapJSConfig struct {
	Format               FlexInt  `json:"format,omitempty"`
	Protocols            []string `json:"protocols,omitempty"`
	ChallengeCount       FlexInt  `json:"challengeCount,omitempty"`
	ChallengeSize        FlexInt  `json:"challengeSize,omitempty"`
	ChallengeDifficulty  FlexInt  `json:"challengeDifficulty,omitempty"`
	ExpiresMs            FlexInt  `json:"expiresMs,omitempty"`
	HashWXDifficulty     FlexInt  `json:"hashWXDifficulty,omitempty"`
	HashWXNoncesPerHash  FlexInt  `json:"hashWXNoncesPerHash,omitempty"`
	HashWXChallengeCount FlexInt  `json:"hashWXChallengeCount,omitempty"`
	RSWT                 FlexInt  `json:"rswT,omitempty"`
	// Instrumentation 不能带 omitempty：bool+omitempty 序列化时会把显式 false
	// 抹掉，重新保存又回到默认值，"关闭开关"永远存不下来（本次修复的存量 bug 类）。
	Instrumentation bool `json:"instrumentation"`
	// InstrumentationCheckCount instrumentation 检查项数量 0..18；0=按引擎默认
	// 解析（阻断开=18/关=8；混淆 0→阻断开=6/关=3），见 captcha_modern.md。
	InstrumentationCheckCount FlexInt `json:"instrumentationCheckCount,omitempty"`
	// DynamicEscalation 用指针承载三态：nil=默认 true；显式 false 必须原样落库
	// 存活。bool+omitempty 恰好会丢显式 false，指针是这一 bug 类的正解。
	DynamicEscalation      *bool                         `json:"dynamicEscalation,omitempty"`
	InstrumentationOptions CaptchaInstrumentationOptions `json:"instrumentationOptions,omitempty"`
	InfoTitle              CaptchaLocalizedText          `json:"infoTitle,omitempty"`
	InfoText               CaptchaLocalizedText          `json:"infoText,omitempty"`
}

type CaptchaLocalizedText struct {
	En string `json:"en,omitempty"`
	Zh string `json:"zh,omitempty"`
}

// CaptchaInstrumentationOptions 支持 Go 原生逐级混淆等级 1..10；0 使用默认3。
// 各等级不保证与上游 javascript-obfuscator 的输出形态或混淆强度完全相同。
type CaptchaInstrumentationOptions struct {
	// BlockAutomatedBrowsers 不能带 omitempty：与 Instrumentation 同理，显式
	// false 必须在结构存在时持久化。新语义见 captcha_modern.md：检查始终收集
	// 并记录判定，开关只控制是否阻断（观察模式 block=false 不阻断）。
	BlockAutomatedBrowsers bool    `json:"blockAutomatedBrowsers"`
	ObfuscationLevel       FlexInt `json:"obfuscationLevel,omitempty"`
}

// DefaultCaptchaConfig 返回独立配置（含独立的协议切片）。
func DefaultCaptchaConfig() CaptchaConfig {
	return CaptchaConfig{
		ExpireTime: 24, IPMode: "nic", EngineType: CaptchaEngineTraditional,
		CapJsConfig: CaptchaCapJSConfig{
			Format: 1, Protocols: []string{"sha256-pow"},
			ChallengeCount: 50, ChallengeSize: 32, ChallengeDifficulty: 4, ExpiresMs: 600000,
			HashWXDifficulty: 1000000, HashWXNoncesPerHash: 65536, HashWXChallengeCount: 4,
			RSWT:                   75000,
			InstrumentationOptions: CaptchaInstrumentationOptions{ObfuscationLevel: 3},
			InfoTitle:              CaptchaLocalizedText{Zh: "安全验证", En: "Security Verification"},
			InfoText:               CaptchaLocalizedText{Zh: "为了确保您的访问安全，请完成以下验证", En: "To ensure the security of your access, please complete the following verification"},
		},
	}
}

func fillCaptchaDefaults(c *CaptchaConfig) {
	p := &c.CapJsConfig
	if p.Format == 0 {
		p.Format = 1
	}
	if p.Protocols == nil {
		p.Protocols = []string{"sha256-pow"}
	}
	if p.HashWXDifficulty == 0 {
		p.HashWXDifficulty = 1000000
	}
	if p.HashWXNoncesPerHash == 0 {
		p.HashWXNoncesPerHash = 65536
	}
	if p.HashWXChallengeCount == 0 {
		p.HashWXChallengeCount = 4
	}
	if p.RSWT == 0 {
		p.RSWT = 75000
	}
	if p.InstrumentationOptions.ObfuscationLevel == 0 {
		p.InstrumentationOptions.ObfuscationLevel = 3
	}
	if c.EngineType == "" || c.EngineType == "default" {
		c.EngineType = CaptchaEngineTraditional
	}
	if c.IPMode == "" {
		c.IPMode = "nic"
	}
	c.ContactInfo = strings.TrimSpace(c.ContactInfo)
	// format 1 的 instrumentation 是顶层附加结果，不属于其 PoW 协议列表。
	if p.Format == 2 {
		hasInstrumentation := false
		for _, protocol := range p.Protocols {
			if protocol == "instrumentation" {
				hasInstrumentation = true
			}
		}
		if hasInstrumentation {
			p.Instrumentation = true
		}
		// 空列表由校验拒绝，而不是悄悄变成 instrumentation-only。
		if p.Instrumentation && !hasInstrumentation && len(p.Protocols) > 0 {
			p.Protocols = append(append([]string(nil), p.Protocols...), "instrumentation")
		}
	}
}

// DecodeCaptchaConfig 是管理端严格入口：先在完整默认值上解码，再校验。
// 未知字段（包括嵌套字段）与重复键明确拒绝，绝不静默丢弃后保存。
// 空字符串表示默认配置；非空输入必须是单个 JSON 对象。FlexInt 保留旧
// 数字字符串、空字符串和 null 的兼容语义。engine_type="default" 兼容为 traditional。
func DecodeCaptchaConfig(raw string) (CaptchaConfig, error) {
	c := DefaultCaptchaConfig()
	raw = strings.TrimSpace(raw)
	if raw != "" {
		if raw[0] != '{' {
			return CaptchaConfig{}, fmt.Errorf("captcha_json must be an object")
		}
		if err := rejectCaptchaDuplicateKeys(json.NewDecoder(strings.NewReader(raw))); err != nil {
			return CaptchaConfig{}, fmt.Errorf("captcha_json: %w", err)
		}
		d := json.NewDecoder(strings.NewReader(raw))
		d.DisallowUnknownFields()
		if err := d.Decode(&c); err != nil {
			return CaptchaConfig{}, fmt.Errorf("captcha_json: %w", err)
		}
		var extra interface{}
		if err := d.Decode(&extra); err != io.EOF {
			return CaptchaConfig{}, fmt.Errorf("captcha_json must contain one object")
		}
	}
	fillCaptchaDefaults(&c)
	if err := ValidateCaptchaConfig(c); err != nil {
		return CaptchaConfig{}, err
	}
	return c, nil
}

// NormalizeCaptchaJSON 校验并生成完整、稳定的 JSON。可选 defaultPathPrefix 仅在
// 输入未设置路径时填充（管理服务传入随机前缀）；显式根路径 "/" 永远保留。
// 未知字段策略是拒绝，而非保留不生效的配置或在重新 Marshal 时静默丢失。
func NormalizeCaptchaJSON(raw string, defaultPathPrefix ...string) (string, error) {
	if len(defaultPathPrefix) > 1 {
		return "", fmt.Errorf("captcha accepts at most one default path prefix")
	}
	c, err := DecodeCaptchaConfig(raw)
	if err != nil {
		return "", err
	}
	if c.PathPrefix == "" && len(defaultPathPrefix) == 1 {
		c.PathPrefix = defaultPathPrefix[0]
	}
	if err := ValidateCaptchaConfig(c); err != nil {
		return "", err
	}
	b, err := json.Marshal(c)
	return string(b), err
}

// ValidateCaptchaConfig 不修改调用者；零值现代选项使用文档默认值，但 c/s/d、
// expiresMs、expire_time 显式为0不合法。请通过 DefaultCaptchaConfig 或 Decode 构造。
func ValidateCaptchaConfig(c CaptchaConfig) error {
	fillCaptchaDefaults(&c)
	if c.IsEnableCaptcha != 0 && c.IsEnableCaptcha != 1 {
		return fmt.Errorf("is_enable_captcha must be 0 or 1")
	}
	if c.EngineType != CaptchaEngineTraditional && c.EngineType != CaptchaEngineCapJs {
		return fmt.Errorf("unsupported captcha engine_type %q", c.EngineType)
	}
	if c.IPMode != "nic" && c.IPMode != "proxy" {
		return fmt.Errorf("captcha ip_mode must be nic or proxy")
	}
	switch c.PassBinding {
	case "", CaptchaPassBindingIP, CaptchaPassBindingIPTLS:
	default:
		// ""/"ip" 语义等价（默认仅绑定客户端 IP）。保存期对枚举外取值明确报错，
		// 不静默改写；运行期 Parse 的安全回退见 ParseCaptchaConfig。
		return fmt.Errorf("captcha passBinding must be \"\", %q or %q", CaptchaPassBindingIP, CaptchaPassBindingIPTLS)
	}
	if len([]rune(c.ContactInfo)) > CaptchaContactMaxRunes {
		return fmt.Errorf("captcha contact_info exceeds %d characters", CaptchaContactMaxRunes)
	}
	if err := ValidateCaptchaPathPrefix(c.PathPrefix); err != nil {
		return err
	}
	p := c.CapJsConfig
	for _, bound := range []struct {
		name            string
		value, min, max FlexInt
	}{
		{"expire_time", c.ExpireTime, 1, 720},
		{"challengeCount", p.ChallengeCount, 1, 1000},
		{"challengeSize", p.ChallengeSize, 1, 256},
		{"challengeDifficulty", p.ChallengeDifficulty, 1, 16},
		{"expiresMs", p.ExpiresMs, 1000, 3600000},
		{"hashWXDifficulty", p.HashWXDifficulty, 1, 1000000000},
		{"hashWXNoncesPerHash", p.HashWXNoncesPerHash, 1, 1048576},
		{"hashWXChallengeCount", p.HashWXChallengeCount, 1, 64},
		{"rswT", p.RSWT, 1, 300000},
		{"instrumentationCheckCount", p.InstrumentationCheckCount, 0, 18},
		{"instrumentationOptions.obfuscationLevel", p.InstrumentationOptions.ObfuscationLevel, 1, 10},
	} {
		if bound.value < bound.min || bound.value > bound.max {
			return fmt.Errorf("captcha %s must be in %d..%d", bound.name, bound.min, bound.max)
		}
	}
	if p.Format != 1 && p.Format != 2 {
		return fmt.Errorf("captcha format must be 1 or 2")
	}
	if len(p.Protocols) == 0 {
		return fmt.Errorf("captcha protocols must not be empty")
	}
	seen := make(map[string]bool)
	for _, protocol := range p.Protocols {
		switch protocol {
		case "sha256-pow", "hashwx", "rsw", "instrumentation":
		default:
			return fmt.Errorf("unsupported captcha protocol %q", protocol)
		}
		if seen[protocol] {
			return fmt.Errorf("duplicate captcha protocol %q", protocol)
		}
		seen[protocol] = true
		if p.Format == 1 && protocol != "sha256-pow" {
			return fmt.Errorf("captcha protocol %q requires explicit format 2", protocol)
		}
	}
	return nil
}

// ValidateCaptchaPathPrefix 允许空值（自动生成）、根路径及安全的绝对 URL 路径。
// 使用 ASCII 路径白名单，拒绝所有百分号转义、反斜杠、引号、query/fragment、
// 控制字符和路径穿越；不做 Clean/Trim，避免把危险输入改写成另一个路由。
func ValidateCaptchaPathPrefix(prefix string) error {
	if prefix == "" {
		return nil
	}
	if len(prefix) > 256 || !strings.HasPrefix(prefix, "/") || strings.Contains(prefix, "//") || strings.Contains(prefix, "..") {
		return fmt.Errorf("captcha path_prefix must be a safe absolute path (max 256 bytes)")
	}
	for _, ch := range prefix {
		if ch == '/' || ch == '_' || ch == '-' || ch == '.' || ch == '~' || ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' {
			continue
		}
		return fmt.Errorf("captcha path_prefix contains unsafe characters")
	}
	for _, segment := range strings.Split(prefix, "/") {
		if segment == "." {
			return fmt.Errorf("captcha path_prefix contains a dot segment")
		}
	}
	return nil
}

func rejectCaptchaDuplicateKeys(d *json.Decoder) error {
	t, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := t.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok {
				return fmt.Errorf("object key must be a string")
			}
			// encoding/json 字段匹配大小写不敏感；同名大小写变体也不能覆盖。
			name = strings.ToLower(name)
			if seen[name] {
				return fmt.Errorf("duplicate field %q", name)
			}
			seen[name] = true
			if err := rejectCaptchaDuplicateKeys(d); err != nil {
				return err
			}
		}
	case '[':
		for d.More() {
			if err := rejectCaptchaDuplicateKeys(d); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter")
	}
	_, err = d.Token()
	return err
}
