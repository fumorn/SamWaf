# Captcha JSON 配置与迁移约定

`CaptchaConfig.CapJsConfig` 为 `CaptchaCapJSConfig`。管理保存必须调用
`NormalizeCaptchaJSON(raw, optionalDefaultPrefix)`；需要结构体与错误时调用
`DecodeCaptchaConfig(raw)`。`ValidateCaptchaConfig(config)` 只校验、不修改传入结构。
`DefaultCaptchaConfig()` 创建完整默认配置；不要从零值结构体重新 Marshal 旧 JSON。

## 兼容与未知字段策略

- 原有数值字段仍为 `FlexInt`，接受整数与数字字符串；空字符串/null 保留默认值。
  新增整数字段同样支持该规则。`exclude_urls` 仍接受换行字符串或字符串数组。
- 空 JSON 字符串与 `{}` 生成完整默认值；非空输入必须是单个 JSON 对象。
- 管理保存**拒绝所有层级的未知字段**、重复键、坏类型、越界参数与多余 JSON。
  不会静默丢弃字段或保存部分解析成功的结果。未来版本新增字段须先升级后端，
  旧后端不会假装已保存其不认识的设置。
- 保存期将空/default engine 兼容为 traditional，其它未知引擎拒绝。
  旧运行期 `ParseCaptchaConfig` 仍把未知引擎归一 traditional、忽略未知字段、
  截断联系方式，但坏类型/非法有效值整份回落默认配置，不返回部分解析结果。
  安全敏感调用应使用带 error 的 Decode，遇到错误自行 fail-closed。
- 新增/编辑均走同一个严格入口；未设置路径时新增生成随机路径，编辑优先沿用旧路径。
  显式根前缀 `/` 保持原样。编辑中的其它缺省字段按完整配置默认值处理，不是 PATCH。

## 格式与协议

旧 `engine_type: "capJs"` 不再选择旧 FNV；默认或 `format: 0` 均为 format 1
SHA-256 PoW，保留同一配置的 challengeCount/Size/Difficulty。只有显式
`format: 2` 才启用现代协议组合。

`cap_js_config.protocols` 支持 `sha256-pow`、`hashwx`、`rsw`、`instrumentation`。
缺省/null 为 `["sha256-pow"]`；显式空列表、空成员、未知协议、重复成员均拒绝。
format 1 只允许 sha256-pow，但允许 `instrumentation: true` 作为顶层附加结果。
format 2 下 instrumentation=true 自动追加对应协议（已存在则不重复）；显式
instrumentation 协议也会设置该开关为 true。

## 默认值与有效范围

| JSON 字段 | 默认值 | 有效范围 |
| --- | --- | --- |
| expire_time（通过后有效期，小时） | 24 | 1..720 |
| passBinding（通行凭证绑定维度） | ""/"ip"（两者等价） | ""、"ip"、"ip+tls" |
| challengeCount | 50 | 1..1000 |
| challengeSize | 32 | 1..256 |
| challengeDifficulty | 4 | 1..16 |
| expiresMs（挑战有效期，毫秒） | 600000 | 1000..3600000 |
| hashWXDifficulty | 1000000 | 1..1000000000；0 使用默认 |
| hashWXNoncesPerHash | 65536 | 1..1048576；0 使用默认 |
| hashWXChallengeCount | 4 | 1..64；0 使用默认 |
| rswT | 75000 | 1..300000；0 使用默认 |
| instrumentationCheckCount | 0（引擎默认） | 0..18 |
| dynamicEscalation | nil（=true） | true/false；显式值必须落库存活 |
| instrumentationOptions.obfuscationLevel | 3 | 1..10；0 使用默认 |

`instrumentation` 与 `instrumentationOptions.blockAutomatedBrowsers` 默认 false，
不会给存量用户偷偷开启浏览器拦截。混淆 1..10 均由 Go 原生逐级实现，
配置接受并保留选择的等级，不将 4..10 静默降级为 3。该实现不保证与上游
javascript-obfuscator 的输出形态或混淆强度完全相同。

## 开关持久化：显式 false 不丢

bool 字段挂 `omitempty` 是存量 bug 类：序列化时显式 `false` 会被抹掉，
重新保存时又回到默认 true——"关闭开关"永远存不下来。修复原则：

- `instrumentation` 与 `instrumentationOptions.blockAutomatedBrowsers` 已去掉
  字段级 omitempty：结构存在时 `false` 与 `true` 一样原样落库。
- `dynamicEscalation` 是三态语义（未设置/显式关/显式开），压不成两态 bool，
  因此用 `*bool`：`nil`=默认 true；显式 `false` 必须落库存活。这正是该 bug
  类要用指针承载的原因，错误类型（字符串/数字）在保存期明确报错。
- `instrumentationCheckCount` 为 FlexInt 0..18：0=按引擎默认解析，不落具体
  数值；越界（负数、>18）与非整数明确报错。

## instrumentation 检查的新语义

`blockAutomatedBrowsers` 现在只控制"是否阻断"：无论开关如何，检查始终注入、
判定始终记录（观察模式：`block=false` 也收集并记录判定、不阻断）。检查项数量
由 `instrumentationCheckCount` 控制，0 时引擎按默认解析——阻断开=18、关=8；
混淆等级为 0 时阻断开=6、关=3。`dynamicEscalation`（默认开启）表示允许引擎在
信号异常时动态升级检查强度。

### dynamicEscalation 与 NAT 的连带说明

升级/挑战判定大量按出口 IP 维度触发：NAT/办公网后面一个自动化客户端持续
失败，可能让整个出口 IP 被升级到更高难度，同 NAT 的正常用户一起被误伤。
关闭 `dynamicEscalation` 可以避免这种连带升级，代价是对自动化工具更宽容
（只保留静态配置的检查强度）。

## 通行凭证绑定（passBinding）

- `""`/`"ip"`（默认，两者等价）：通行凭证绑定 站点 + 完整策略哈希 + 规范化
  客户端 IP，行为与历史版本一致。
- `"ip+tls"`：在上述之上再把凭证绑定到请求的 TLS 指纹。签发时把
  sha256(指纹) 存进凭证元数据（只存哈希、绝不存原文）；校验时当前请求指纹
  为空或与存值不一致一律拒绝。cookie 与 header token 两条呈现路径同等绑定。
- fail closed：绑定模式下请求没有 TLS 指纹（明文 HTTP、采集失败等）时，
  签发直接失败（不发 cookie/token），校验直接判无凭证。
- 保存期跨字段校验：`passBinding="ip+tls"` 要求站点 `ssl==1`，否则拒绝保存
  并提示"TLS 指纹绑定需要站点启用 SSL"——明文 HTTP 站点禁配此模式。
- 传统点选引擎（traditional）与 capJs 共用同一套 IssuePass/HasPass 凭证，
  绑定对两种引擎同样生效，不受 engine_type 影响。
- 运行期兼容：`ParseCaptchaConfig` 遇到枚举外的存量 `passBinding`（直接改库
  等来源）安全回退为 `"ip"`，不整份作废配置；保存期入口仍明确报错。
- 策略哈希：`passBinding` 参与 Scope 的策略哈希。`""` 与 `"ip"` 在哈希前归一
  为同一值，等价写法互不作废；从默认切到 `"ip+tls"` 会立即作废旧凭证（这是
  预期行为：绑定收紧后旧凭证不应存活）。

### passBinding="ip+tls" 的诚实取舍

- 必须启用 SSL：没有 TLS 就没有指纹，站点会被自己的验证码配置锁死，所以
  保存期直接拒绝而不是运行期反复失败。
- 企业 TLS 规整中间件可能误伤：SSL 检查网关/杀毒代理会替换证书并重协商
  TLS，指纹随之轮换或全员趋同——正常用户可能被反复挑战（指纹不停变），
  或者绑定退化为"全公司同一个指纹"（失去区分度）。有这类中间件的环境
  应保持默认 `""`/`"ip"`。
- NAT 场景：默认 `ip` 模式下同一 NAT 出口的用户共享出口 IP，一个客户端
  拿到的凭证对同 NAT 的其它客户端同样有效（既有弱点）。`ip+tls` 把凭证再
  绑到 TLS 指纹，缓解跨客户端串用；但同型号浏览器/同模板容器的批量实例
  TLS 栈一致，仍会互相共用凭证。
- 指纹稳定性依赖客户端 TLS 栈：浏览器升级/降级 TLS 库、换浏览器都会换
  指纹，用户会被要求重新完成挑战（凭证在 TTL 内提前失效）。

## 路径与公开文本

`path_prefix` 可为空（自动生成），非空必须是最多 256 字节的绝对 URL 路径。
只允许 ASCII 字母、数字、`/`、`_`、`-`、`.`、`~`；拒绝 `..`、单点段、双斜杠、
百分号转义（含双重转义）、反斜杠、引号、query/fragment、空白和控制字符。
不使用路径 Clean 或 Trim 把危险输入偷偷改成别的路由。

`contact_info` 两侧空白被去除，管理保存最多 200 Unicode 字符；运行期为兼容
存量数据截断到 200 字符。挑战页仍必须进行 HTML 转义，校验不是输出转义的替代。

## 示例：现代组合

```json
{
  "engine_type": "capJs",
  "is_enable_captcha": 1,
  "expire_time": "24",
  "passBinding": "ip+tls",
  "cap_js_config": {
    "format": 2,
    "protocols": ["sha256-pow", "hashwx", "rsw"],
    "instrumentation": true,
    "dynamicEscalation": false,
    "instrumentationCheckCount": 18,
    "instrumentationOptions": {
      "blockAutomatedBrowsers": true,
      "obfuscationLevel": 3
    }
  }
}
```

上例保存时补全所有默认值并追加 instrumentation 协议；`dynamicEscalation:false`
与 `blockAutomatedBrowsers`/`instrumentation` 的显式 false 一样原样落库存活。
接入核心 Options 时，整数字段从 FlexInt 显式转换为 int，ExpiresMs 显式转换为
int64；`dynamicEscalation` 按"nil=默认 true，显式 false 只在指针非 nil 时映射
false"的三态规则展开；`instrumentationCheckCount` 直传，0 交由引擎按默认解析。
