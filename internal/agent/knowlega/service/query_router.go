package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/loon-hejw/knowlega/internal/agent/knowlega/core"
	"github.com/loon-hejw/knowlega/internal/agent/knowlega/llmretry"
)

const (
	QueryIntentDirectChat       = "direct_chat"
	QueryIntentSystemFAQ        = "system_faq"
	QueryIntentGeneralAssistant = "general_assistant"
	QueryIntentWikiQuery        = "wiki_query"
	QueryIntentMissingEvidence  = "missing_evidence"
	QueryIntentUnsupported      = "unsupported"
)

type QueryRoutingInput struct {
	Question            string
	ConversationContext string
	Purpose             string
	Schema              string
	Index               string
	Overview            string
}

type QueryRouteDecision struct {
	Intent     string  `json:"intent"`
	Confidence float64 `json:"confidence"`
	Reason     string  `json:"reason"`
}

type QueryGeneralAnswerInput struct {
	Question            string
	ConversationContext string
}

type QueryRouterAgent interface {
	RouteQuery(QueryRoutingInput) (QueryRouteDecision, error)
}

type ContextQueryRouterAgent interface {
	RouteQueryContext(context.Context, QueryRoutingInput) (QueryRouteDecision, error)
}

type QueryGeneralAnswerAgent interface {
	AnswerGeneralQuery(QueryGeneralAnswerInput) (string, error)
}

type ContextQueryGeneralAnswerAgent interface {
	AnswerGeneralQueryContext(context.Context, QueryGeneralAnswerInput) (string, error)
}

func routeQueryWithContext(ctx context.Context, agent QueryAgent, input QueryRoutingInput) (QueryRouteDecision, error) {
	if decision, ok := deterministicQueryRoute(input.Question); ok {
		return decision, nil
	}
	if contextAgent, ok := agent.(ContextQueryRouterAgent); ok {
		return normalizeRouteDecision(contextAgent.RouteQueryContext(ctx, input))
	}
	if routerAgent, ok := agent.(QueryRouterAgent); ok {
		return normalizeRouteDecision(routerAgent.RouteQuery(input))
	}
	return QueryRouteDecision{
		Intent:     QueryIntentWikiQuery,
		Confidence: 0.5,
		Reason:     "no query router configured; defaulting to persistent wiki query",
	}, nil
}

func normalizeRouteDecision(decision QueryRouteDecision, err error) (QueryRouteDecision, error) {
	if err != nil {
		return QueryRouteDecision{}, err
	}
	decision.Intent = normalizeRouteIntent(decision.Intent)
	if decision.Confidence <= 0 {
		decision.Confidence = 0.5
	}
	if decision.Confidence < 0.55 && decision.Intent != QueryIntentDirectChat && decision.Intent != QueryIntentSystemFAQ {
		decision.Intent = QueryIntentWikiQuery
		decision.Reason = strings.TrimSpace(decision.Reason + " low confidence; defaulted to persistent wiki query")
	}
	if strings.TrimSpace(decision.Reason) == "" {
		decision.Reason = "query routed by intent classifier"
	}
	return decision, nil
}

func normalizeRouteIntent(intent string) string {
	intent = strings.ToLower(strings.TrimSpace(intent))
	intent = strings.ReplaceAll(intent, "-", "_")
	intent = strings.ReplaceAll(intent, " ", "_")
	switch intent {
	case QueryIntentDirectChat, "chitchat", "smalltalk", "small_talk", "chat", "greeting":
		return QueryIntentDirectChat
	case QueryIntentSystemFAQ, "faq", "system_help", "help", "capability", "capabilities":
		return QueryIntentSystemFAQ
	case QueryIntentGeneralAssistant, "general", "general_chat", "general_question", "ordinary_qa":
		return QueryIntentGeneralAssistant
	case QueryIntentMissingEvidence, "no_evidence", "insufficient_evidence", "unknown":
		return QueryIntentMissingEvidence
	case QueryIntentUnsupported, "unsupported_request", "out_of_scope", "unsafe_or_unsupported":
		return QueryIntentUnsupported
	case QueryIntentWikiQuery, "answer_from_persistent_wiki", "code_query", "query", "rag", "":
		return QueryIntentWikiQuery
	default:
		return QueryIntentWikiQuery
	}
}

func deterministicQueryRoute(q string) (QueryRouteDecision, bool) {
	if isDirectChatQuestion(q) {
		return QueryRouteDecision{
			Intent:     QueryIntentDirectChat,
			Confidence: 1,
			Reason:     "matched short greeting/thanks rule",
		}, true
	}
	if isSystemFAQQuestion(q) {
		return QueryRouteDecision{
			Intent:     QueryIntentSystemFAQ,
			Confidence: 0.95,
			Reason:     "matched system capability/help rule",
		}, true
	}
	if isExplicitWikiQuestion(q) {
		return QueryRouteDecision{
			Intent:     QueryIntentWikiQuery,
			Confidence: 0.9,
			Reason:     "question explicitly asks for local wiki/source/project evidence",
		}, true
	}
	if isUnsupportedGeneralQuestion(q) {
		return QueryRouteDecision{
			Intent:     QueryIntentUnsupported,
			Confidence: 0.9,
			Reason:     "request requires external real-time or high-stakes advice outside the local wiki",
		}, true
	}
	return QueryRouteDecision{}, false
}

func routeQueryPlan(q string, intent, answerMode string) core.QueryPlan {
	return core.QueryPlan{
		Question:       q,
		Intent:         intent,
		CandidateLimit: 0,
		AnswerMode:     answerMode,
		CanWriteBack:   false,
	}
}

func systemFAQAnswer() string {
	return "我是 Knowledge Core 的知识库助手。可以帮你查询已经导入的 raw/source 内容，阅读 wiki 页面，结合图谱证据回答问题，也可以把有价值的综合结论写回 wiki/syntheses。普通寒暄和系统用法我会直接回答；需要业务或项目证据的问题会进入知识库查询流程。"
}

func unsupportedQuestionAnswer(q string) string {
	question := strings.TrimSpace(q)
	if question == "" {
		question = "这个问题"
	}
	return fmt.Sprintf("我不能直接回答 `%s`：它需要实时外部信息或高风险专业判断，而当前系统的可靠边界是本地知识库证据。请导入相关资料后让我基于知识库回答，或改成一个不依赖实时/专业结论的问题。", question)
}

func generalAssistantUnavailableAnswer(q string) string {
	question := strings.TrimSpace(q)
	if question == "" {
		question = "这个问题"
	}
	return fmt.Sprintf("`%s` 看起来不需要知识库证据，但当前没有可用的通用 LLM 回答能力。你可以改成需要查询本地知识库的问题，或配置 OpenAI-compatible/Anthropic LLM 后再试。", question)
}

func isSystemFAQQuestion(q string) bool {
	text := normalizeDirectChatText(q)
	if text == "" {
		return false
	}
	phrases := []string{
		"你能做什么", "你可以做什么", "怎么用", "如何使用", "知识库怎么用", "knowledge core 怎么用",
		"这个系统能做什么", "有什么功能", "支持什么", "能帮我做什么",
	}
	for _, phrase := range phrases {
		if strings.Contains(text, phrase) {
			return true
		}
	}
	return false
}

func isExplicitWikiQuestion(q string) bool {
	text := strings.ToLower(q)
	keywords := []string{
		"知识库", "wiki", "根据文档", "根据资料", "根据源码", "根据项目", "引用", "证据",
		"raw/", "raw/sources", "wiki/", "页面", "导入", "来源", "图谱", "代码图谱",
	}
	for _, keyword := range keywords {
		if strings.Contains(text, strings.ToLower(keyword)) {
			return true
		}
	}
	return false
}

func isUnsupportedGeneralQuestion(q string) bool {
	text := strings.ToLower(strings.TrimSpace(q))
	if text == "" {
		return false
	}
	realTimeTerms := []string{"今天", "现在", "最新", "实时", "股价", "汇率", "新闻", "天气", "today", "latest", "current price", "weather"}
	highStakesTerms := []string{"诊断", "处方", "法律意见", "投资建议", "买股票", "卖股票", "diagnose", "prescription", "legal advice", "investment advice"}
	for _, term := range realTimeTerms {
		if strings.Contains(text, term) {
			return true
		}
	}
	for _, term := range highStakesTerms {
		if strings.Contains(text, term) {
			return true
		}
	}
	return false
}

func (a OpenAICompatibleQueryAgent) RouteQuery(input QueryRoutingInput) (QueryRouteDecision, error) {
	return a.RouteQueryContext(context.Background(), input)
}

func (a OpenAICompatibleQueryAgent) RouteQueryContext(ctx context.Context, input QueryRoutingInput) (QueryRouteDecision, error) {
	system := `You are the first-stage router for Knowledge Core, a persistent LLM Wiki product.
Classify the user request before any wiki tool loop runs.

Intents:
- direct_chat: short greetings, thanks, or simple social chat.
- system_faq: asks what this system can do or how to use the knowledge base.
- general_assistant: low-risk general explanation or writing question that does not need local wiki evidence and does not need real-time facts.
- wiki_query: needs local wiki/source/project/code evidence, asks about imported materials, asks for citations, or is ambiguous.
- missing_evidence: explicitly asks for local evidence that the provided navigation context shows is absent.
- unsupported: needs real-time external data or high-stakes medical/legal/financial advice.

Be conservative: when uncertain, choose wiki_query.
Return only JSON: {"intent":"wiki_query","confidence":0.8,"reason":"short reason"}`
	user := fmt.Sprintf(`Question:
%s

Conversation context:
%s

Purpose:
%s

Index excerpt:
%s

Overview excerpt:
%s`, input.Question, input.ConversationContext, input.Purpose, input.Index, input.Overview)
	retryOpts := a.retryOptions()
	return llmretry.DoValue[QueryRouteDecision](ctx, retryOpts, queryLLMRetryCallback(ctx), func(attempt int) (QueryRouteDecision, bool, error) {
		content, retryable, err := a.chatOnce(ctx, system, user)
		if err != nil {
			return QueryRouteDecision{}, retryable, err
		}
		decision, err := decodeLLMJSONObject[QueryRouteDecision](content, "llm query route")
		if err != nil {
			return QueryRouteDecision{}, true, err
		}
		return decision, false, nil
	})
}

func (a OpenAICompatibleQueryAgent) AnswerGeneralQuery(input QueryGeneralAnswerInput) (string, error) {
	return a.AnswerGeneralQueryContext(context.Background(), input)
}

func (a OpenAICompatibleQueryAgent) AnswerGeneralQueryContext(ctx context.Context, input QueryGeneralAnswerInput) (string, error) {
	system := `You are Knowledge Core's general assistant mode.
Answer only low-risk general questions that do not need the local wiki.
Do not cite or claim to have read wiki/raw/graph evidence.
If the question asks for project, business, source, codebase, or knowledge-base facts, say it should be answered through the knowledge base instead.`
	user := fmt.Sprintf(`Question:
%s

Conversation context:
%s`, input.Question, input.ConversationContext)
	return a.chatContext(ctx, system, user)
}
