package moderation

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	openai "github.com/sashabaranov/go-openai"
)

// TestTheLiveJudgementRequest covers the request as a provider actually receives
// it: the address it is posted to, the header, and the shape of the answer. That
// is the part no stub can confirm -- the stub answers whatever path it is asked on,
// with whatever header it is given.
//
// Skipped unless LIVE_MODEL_URL and LIVE_MODEL_KEY are set, so the suite stays
// offline by default.
func TestTheLiveJudgementRequest(t *testing.T) {
	address, key := os.Getenv("LIVE_MODEL_URL"), os.Getenv("LIVE_MODEL_KEY")
	if address == "" || key == "" {
		t.Skip("set LIVE_MODEL_URL and LIVE_MODEL_KEY to post a real judgement")
	}
	var cfg Config
	cfg.Model.BaseURL = address
	cfg.Model.APIKey = key
	cfg.Model.Name = "deepseek-chat"
	cfg.Model.Thinking = "hide"
	cfg.Model.ReasoningEffort = "high"
	if err := cfg.applyDefaults(); err != nil {
		t.Fatalf("applyDefaults: %v", err)
	}
	h := &handler{cfg: cfg}
	messages := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "你是群审核助手，只回一个 JSON"},
		{Role: openai.ChatMessageRoleUser,
			Content: "判断这条是否群广告：deepseek0.01x 限时0.01倍率 https://q1.example/" +
				" 防屏蔽：api.example.test"},
	}

	content, reasoning, err := h.ask(context.Background(), h.judgeRequest(messages))
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if strings.TrimSpace(content) == "" {
		t.Error("the provider answered with nothing")
	}
	// The point of the whole change: reasoning was asked for, so it has to arrive.
	// An empty chain here means the switch is not reaching the provider, and the
	// judgements would go back to being five-token guesses.
	if strings.TrimSpace(reasoning) == "" {
		t.Error("thinking was asked for and no chain of thought came back")
	}
	t.Logf("answer=%q, chain of thought=%d characters",
		strings.TrimSpace(content), len(reasoning))
}

// TestTheThinkingSwitchIsSent covers the field that decides whether the model
// reasons at all.
//
// It is what the OpenAI-compatible client cannot send, and what its absence cost:
// measured against DeepSeek's own endpoint, the same question came back in five
// output tokens with an empty reasoning_content.
func TestTheThinkingSwitchIsSent(t *testing.T) {
	messages := []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "x"}}

	body := func(thinking, effort string) string {
		t.Helper()
		var cfg Config
		cfg.Model.Name = "stub-model"
		cfg.Model.Thinking = thinking
		cfg.Model.ReasoningEffort = effort
		cfg.Model.Temperature = 0.7
		// Defaults first: the request is built from a configuration that has been
		// through the same checks a running bot's has.
		if err := cfg.applyDefaults(); err != nil {
			t.Fatalf("applyDefaults: %v", err)
		}
		h := &handler{cfg: cfg}
		raw, err := json.Marshal(h.judgeRequest(messages))
		if err != nil {
			t.Fatalf("encoding the request: %v", err)
		}
		return string(raw)
	}

	hidden := body("hide", "high")
	if !strings.Contains(hidden, `"thinking":{"type":"enabled"}`) {
		t.Errorf("hide must ask for reasoning, got %s", hidden)
	}
	if !strings.Contains(hidden, `"reasoning_effort":"high"`) {
		t.Errorf("the configured effort was dropped, got %s", hidden)
	}
	// Temperature is accepted in thinking mode and ignored, so sending it would be a
	// setting that looks applied and is not.
	if strings.Contains(hidden, "temperature") {
		t.Errorf("temperature was sent with reasoning on, got %s", hidden)
	}
	if !strings.Contains(hidden, `"max_tokens":`) {
		t.Errorf("the answer cap was not sent, got %s", hidden)
	}

	off := body("off", "")
	if !strings.Contains(off, `"thinking":{"type":"disabled"}`) {
		t.Errorf("off must turn reasoning off, got %s", off)
	}
	if !strings.Contains(off, "temperature") {
		t.Errorf("temperature belongs in a request without reasoning, got %s", off)
	}
	if strings.Contains(off, "reasoning_effort") {
		t.Errorf("an effort with reasoning off asks the server for nothing, got %s", off)
	}

	// Empty means the provider's own default, which is sent by the library without
	// this field at all.
	if unset := body("", ""); strings.Contains(unset, "thinking") {
		t.Errorf("an empty thinking setting must send nothing, got %s", unset)
	}
}

// TestTheThinkingConfigurationIsCheckedAtStartup covers the combinations that
// cannot work, refused when the bot starts rather than when somebody reports.
func TestTheThinkingConfigurationIsCheckedAtStartup(t *testing.T) {
	cases := map[string]struct {
		thinking string
		effort   string
		stream   bool
		refused  bool
	}{
		"hide":                      {thinking: "hide"},
		"show":                      {thinking: "show"},
		"off":                       {thinking: "off"},
		"unset":                     {},
		"an effort with the switch": {thinking: "hide", effort: "max"},
		"a name that is not one":    {thinking: "yes", refused: true},
		"an effort that is not one": {thinking: "hide", effort: "extreme", refused: true},
		// Effort without the switch would be a setting the provider ignores.
		"an effort without the switch": {effort: "high", refused: true},
		// Streaming goes through the library, which cannot carry the switch, so the
		// pair would promise reasoning and not ask for it.
		"streaming with reasoning": {thinking: "hide", stream: true, refused: true},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			var cfg Config
			cfg.Model.Name = "stub-model"
			cfg.Model.Thinking = testCase.thinking
			cfg.Model.ReasoningEffort = testCase.effort
			cfg.Model.Stream = testCase.stream

			err := cfg.applyDefaults()
			if testCase.refused && err == nil {
				t.Error("this configuration cannot work, and must be refused")
			}
			if !testCase.refused && err != nil {
				t.Errorf("applyDefaults: %v", err)
			}
		})
	}

	// And the cap has a default, because the chain of thought is paid for out of it.
	var cfg Config
	cfg.Model.Name = "stub-model"
	cfg.Model.Thinking = "hide"
	if err := cfg.applyDefaults(); err != nil {
		t.Fatalf("applyDefaults: %v", err)
	}
	if cfg.Model.MaxTokens <= 0 {
		t.Error("the answer cap must have a default: thinking is paid for out of it")
	}
}
