package moderation

import (
	"context"
	"errors"
	"testing"
)

// TestAnUnreadableAnswerIsAskedAgain covers the retry.
//
// A model is not deterministic even at temperature zero, so an answer that arrived but
// was not the JSON it was asked for is worth asking for once more. Measured against the
// real endpoint the answer is nearly always well formed -- but "nearly" is not a reason
// to throw a report away.
func TestAnUnreadableAnswerIsAskedAgain(t *testing.T) {
	stub := &modelStub{answers: []string{"我看看啊", `{"verdict":"ok","confidence":0.9}`}}
	h := judgeHarness(t, stub, "")

	verdict, err := h.Judge(context.Background(), "", chainOf("正常聊天"))
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}
	if len(stub.requests) != 2 {
		t.Errorf("the model was asked %d times, want twice", len(stub.requests))
	}
	if verdict.Category != "" {
		t.Errorf("verdict = %+v, want nothing found", verdict)
	}
}

// TestAnAnswerThatStaysUnreadableIsGivenUpOn covers the other end: the retries run out,
// and the report fails rather than being guessed at.
func TestAnAnswerThatStaysUnreadableIsGivenUpOn(t *testing.T) {
	stub := &modelStub{answers: []string{"不是 JSON", "还是不是", "仍然不是"}}
	h := judgeHarness(t, stub, "")

	_, err := h.Judge(context.Background(), "", chainOf("正常聊天"))
	if err == nil {
		t.Fatal("an unreadable answer must not become a verdict")
	}
	if !errors.Is(err, ErrUnjudged) {
		t.Errorf("err = %v, want ErrUnjudged", err)
	}
	// One attempt plus the two default retries.
	if len(stub.requests) != 3 {
		t.Errorf("the model was asked %d times, want three", len(stub.requests))
	}
}

// TestTheRetryCountIsBounded covers the ceiling the configuration may not cross, and the
// default it gets when it says nothing.
func TestTheRetryCountIsBounded(t *testing.T) {
	tooMany := 4
	var refused Config
	refused.Model.Name = "stub-model"
	refused.Model.Retries = &tooMany
	if err := refused.applyDefaults(); err == nil {
		t.Error("more than three retries must be refused")
	}

	none := 0
	var never Config
	never.Model.Name = "stub-model"
	never.Model.Retries = &none
	if err := never.applyDefaults(); err != nil {
		t.Fatalf("applyDefaults: %v", err)
	}
	if never.judgeRetries() != 0 {
		t.Error("zero retries is a real choice and must be kept")
	}

	var silent Config
	silent.Model.Name = "stub-model"
	if err := silent.applyDefaults(); err != nil {
		t.Fatalf("applyDefaults: %v", err)
	}
	if silent.judgeRetries() != defaultJudgeRetries {
		t.Errorf("retries = %d, want the default %d",
			silent.judgeRetries(), defaultJudgeRetries)
	}
	if silent.Model.MaxTokens != 4096 {
		t.Errorf("max_tokens = %d, want the roomier default: the chain of thought is "+
			"paid for out of it", silent.Model.MaxTokens)
	}
}
