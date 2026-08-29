package aigateway

import "strings"

// ProvidersFromEnv 按环境变量装配自托管 / 第三方 AI 提供方：
//   - 配置了真实端点（AI_SELF_HOSTED_URL / AI_THIRD_PARTY_URL）且非占位域名时，
//     返回真实 HTTPProvider（OpenAI 兼容 /v1/chat/completions、/v1/embeddings）；
//   - 否则降级为 LocalDemoProvider（本地回环，[demo] 标注，非伪造真实 LLM 结果），
//     使 /api/ai/chat 开箱返回 200 且 AI 网关全链路（路由/脱敏/审计）可端到端验证。
//
// get 注入一个"按 key 取环境变量"的函数（如 os.Getenv），便于测试时注入假环境。
// 返回 (selfHosted, thirdParty)，可直接传给 NewRouter。
func ProvidersFromEnv(get func(string) string) (self, third AIProvider) {
	selfURL := get("AI_SELF_HOSTED_URL")
	if selfURL != "" && !strings.Contains(selfURL, "vllm.internal") {
		self = NewSelfHostedProvider(selfURL, get("AI_SELF_HOSTED_MODEL"), get("AI_SELF_HOSTED_KEY"))
	} else {
		self = NewLocalDemoProvider()
	}

	thirdURL := get("AI_THIRD_PARTY_URL")
	if thirdURL != "" && !strings.Contains(thirdURL, "thirdparty.com") {
		third = NewThirdPartyProvider(thirdURL, get("AI_THIRD_PARTY_MODEL"), get("AI_THIRD_PARTY_KEY"))
	} else {
		third = NewLocalDemoProvider()
	}
	return self, third
}
