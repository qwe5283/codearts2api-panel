package server

import "strings"

// errClass 是上游错误的处置类别。判定只看错误文本（ApiError 与流内
// UpstreamError 的字符串都带业务码），这样开流前、流式、聚合三条路径共用一套
// 规则——曾因各写各的出现「流式不冷却、非流式冷却」的割裂。
type errClass int

const (
	// classAccountFault 账号侧故障（5xx、网络、鉴权异常）：按状态码处罚账号并轮转。
	classAccountFault errClass = iota
	// classModelUnusable 该模型对此账号不可用（未注册）：不冷却、不轮转，回 404。
	classModelUnusable
	// classRequestContent 请求内容问题：换账号/重试结果相同，不冷却、不轮转，回 400。
	// 上游常把它们包成 HTTP 502，若落进「账号故障」会把整池冷却 10 分钟。
	classRequestContent
)

// classifyUpstreamError 归类上游错误。nil 视为账号故障便于调用方少写分支，
// 调用方应先用 err==nil 短路。
func classifyUpstreamError(err error) errClass {
	if err == nil {
		return classAccountFault
	}
	low := strings.ToLower(err.Error())
	switch {
	case strings.Contains(low, "002002009") || strings.Contains(low, "not registered"):
		return classModelUnusable
	// 逐条：
	//   InferHub.001001005 / request param is invalid —— 参数形状错；
	//   InferHub.001001020 / request model is not multimodal —— 带图打到纯文本模型；
	//   InferHub.001001046 —— 多模态内容格式不正确；
	//   ModelArts.81029 / media conversion failed —— 图片超出媒体转换上限
	//     （downstream returned 413）；
	//   ModelArts.81001 / prompt length exceeds —— 请求超出该模型的上下文：
	//     同一个超长请求打 1M 上下文的 deepseek 成功、打 131k 的 qwen3-vl 报 81001，
	//     是「这个模型装不下」，不是账号坏了。
	case strings.Contains(low, "001001005") || strings.Contains(low, "request param is invalid"),
		strings.Contains(low, "001001020") || strings.Contains(low, "not multimodal"),
		strings.Contains(low, "001001046"),
		strings.Contains(low, "81029") || strings.Contains(low, "media conversion failed"),
		strings.Contains(low, "81001") || strings.Contains(low, "prompt length exceeds"):
		return classRequestContent
	}
	return classAccountFault
}

// isRequestContentError 报告错误是否由请求内容决定（见 classRequestContent）。
func isRequestContentError(err error) bool {
	return err != nil && classifyUpstreamError(err) == classRequestContent
}

// upstreamUnavailableReason 在错误属于「该模型对这个账号不可用」时返回说明，否则空串。
func upstreamUnavailableReason(err error) string {
	if classifyUpstreamError(err) == classModelUnusable && err != nil {
		return "模型未在当前账号注册"
	}
	return ""
}
