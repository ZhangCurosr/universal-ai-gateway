package translate

import "github.com/ZhangCurosr/universal-ai-gateway/internal/protocol"

// 本包用到的协议名。spec/10 §0：translate 不得导入具体适配器包，协议常量在此集中定义，
// 取值必须与各适配器 register.go 中的 Protocol 常量一致。
const (
	protoOpenAIChat      protocol.Protocol = "openai_chat"
	protoAnthropic       protocol.Protocol = "anthropic"
	protoOpenAIResponses protocol.Protocol = "openai_responses"
)
