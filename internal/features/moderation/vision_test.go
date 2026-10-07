package moderation

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	qqbotsdk "github.com/fouc3/qq-bot-sdk"

	"github.com/fouc3/qq-group-management-bot/internal/feature"
	"github.com/fouc3/qq-group-management-bot/internal/imagecache"
)

// pictureStub stands in for the picture directory: what a judgement asks for is
// bytes and a type, so that is all a test has to provide.
type pictureStub struct {
	bodies     map[string][]byte
	unreadable map[string]bool
	// kept is what FetchAll answers with, and asks is every fetch the receive path
	// asked for.
	kept []string
	asks []fetchAsk
}

// fetchAsk is one message's pictures as the receive path handed them over.
type fetchAsk struct {
	messageID string
	urls      []string
}

func newPictureStub() *pictureStub {
	return &pictureStub{
		bodies:     map[string][]byte{},
		unreadable: map[string]bool{},
	}
}

// keep stores one picture and returns the name it is known by.
func (p *pictureStub) keep(ref string, body []byte) string {
	p.bodies[ref] = body
	return ref
}

func (p *pictureStub) Read(ref string) ([]byte, error) {
	if p.unreadable[ref] {
		return nil, errors.New("the picture is not there")
	}
	body, ok := p.bodies[ref]
	if !ok {
		return nil, errors.New("the picture is not there")
	}
	return body, nil
}

func (p *pictureStub) MIME(string) string { return "image/jpeg" }

// Dir is part of the same seam: the stub keeps nothing anywhere.
func (p *pictureStub) Dir() string { return "（测试没有目录）" }

// FetchAll records what the receive path asked to be fetched, and answers with the
// names the test prepared. What it is handed is the point of the test that uses it:
// the URLs that only exist while the event is in hand.
func (p *pictureStub) FetchAll(_ context.Context, messageID string, urls []string) []string {
	p.asks = append(p.asks, fetchAsk{messageID: messageID, urls: urls})
	return p.kept
}

// Sweep is part of the same seam, and unused here.
func (p *pictureStub) Sweep(context.Context) {}

// userParts is the user message of a recorded request, as the tests read it.
func userParts(t *testing.T, request string) (string, []requestPart) {
	t.Helper()
	return roleMessage(t, request, "user")
}

// imagesOf lists the pictures carried by a request, in order.
func imagesOf(parts []requestPart) []string {
	var urls []string
	for _, part := range parts {
		if part.ImageURL != nil {
			urls = append(urls, part.ImageURL.URL)
		}
	}
	return urls
}

// TestPicturesReachAModelThatMaySeeThem covers the switch being on: the picture is
// sent, base64 encoded, next to a line saying which message it belongs to.
func TestPicturesReachAModelThatMaySeeThem(t *testing.T) {
	stub := &modelStub{answer: `{"reason":"测试理由","verdict":"ok","confidence":0.9}`}
	h := judgeHarnessWith(t, stub, "  vision: true\n", "")

	pictures := newPictureStub()
	pictures.keep("a.jpg", []byte("假装这是图"))
	h.pictures = pictures

	chain := chainOf("正常聊天", "扫码进群")
	chain[1].ImgCount = 1
	chain[1].Imgs = []string{"a.jpg"}

	if _, err := h.Judge(context.Background(), "", chain); err != nil {
		t.Fatalf("Judge: %v", err)
	}
	block, parts := userParts(t, stub.lastRequest(t))
	if len(imagesOf(parts)) != 1 {
		t.Fatalf("the request carries %d pictures, want 1", len(imagesOf(parts)))
	}
	want := "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString([]byte("假装这是图"))
	if got := imagesOf(parts)[0]; got != want {
		t.Errorf("the picture was sent as %q, want a base64 data URL", got)
	}
	// The block marks the line the picture belongs to, and a part says so in words:
	// a picture the model cannot tie to a message is one it will tie to whatever it
	// likes.
	if !strings.Contains(block, "[图1]") {
		t.Errorf("the message line does not carry the picture marker:\n%s", block)
	}
	labelled := false
	for _, part := range parts {
		if part.ImageURL == nil && strings.Contains(part.Text, "图1") &&
			strings.Contains(part.Text, "第 2 条消息") {
			labelled = true
		}
	}
	if !labelled {
		t.Error("nothing ties the picture to the message it came from")
	}
	// And the instructions say what to do with it.
	if system := systemContent(t, stub.lastRequest(t)); !strings.Contains(system, "图片") {
		t.Error("the instructions were not told the messages carry pictures")
	}
}

// TestPicturesAreNotSentToAModelThatMayNotSee covers the switch being off, which is
// the default: the messages go, and the picture is simply not part of them.
func TestPicturesAreNotSentToAModelThatMayNotSee(t *testing.T) {
	stub := &modelStub{answer: `{"reason":"测试理由","verdict":"ok","confidence":0.9}`}
	h := judgeHarness(t, stub, "")

	pictures := newPictureStub()
	pictures.keep("a.jpg", []byte("假装这是图"))
	h.pictures = pictures

	chain := chainOf("正常聊天", "扫码进群")
	chain[1].ImgCount = 1
	chain[1].Imgs = []string{"a.jpg"}

	if _, err := h.Judge(context.Background(), "", chain); err != nil {
		t.Fatalf("Judge: %v", err)
	}
	block, parts := userParts(t, stub.lastRequest(t))
	if len(parts) != 0 || len(imagesOf(parts)) != 0 {
		t.Errorf("a picture was sent to a model that may not see: %v", imagesOf(parts))
	}
	if strings.Contains(block, "图1") {
		t.Error("the block marks a picture that was never sent")
	}
	if system := systemContent(t, stub.lastRequest(t)); strings.Contains(system, "图1") {
		t.Error("the instructions promise pictures that were not sent")
	}
}

// TestAPictureThatIsNoLongerInTheCacheIsNotSent covers the swept picture: the
// message still goes, without the picture and without a marker promising one.
func TestAPictureThatIsNoLongerInTheCacheIsNotSent(t *testing.T) {
	stub := &modelStub{answer: `{"reason":"测试理由","verdict":"ok","confidence":0.9}`}
	h := judgeHarnessWith(t, stub, "  vision: true\n", "")

	pictures := newPictureStub()
	pictures.unreadable["gone.jpg"] = true
	h.pictures = pictures

	chain := chainOf("正常聊天", "扫码进群")
	chain[1].ImgCount = 1
	chain[1].Imgs = []string{"gone.jpg"}

	if _, err := h.Judge(context.Background(), "", chain); err != nil {
		t.Fatalf("Judge: %v", err)
	}
	block, parts := userParts(t, stub.lastRequest(t))
	if len(imagesOf(parts)) != 0 {
		t.Error("a picture that is not in the cache was sent")
	}
	if strings.Contains(block, "[图1]") {
		t.Error("the block marks a picture that is not there")
	}
}

// TestAPictureOfATruncatedMessageIsNotSent covers the two rules meeting: the
// character limit cuts the window short, so the pictures of what was left out must
// not follow.
func TestAPictureOfATruncatedMessageIsNotSent(t *testing.T) {
	stub := &modelStub{answer: `{"reason":"测试理由","verdict":"ok","confidence":0.9}`}
	// Small enough that the second message does not fit in the block.
	h := judgeHarnessWith(t, stub, "  vision: true\n", "max_chars: 120\n")

	pictures := newPictureStub()
	pictures.keep("a.jpg", []byte("第一张"))
	pictures.keep("b.jpg", []byte("第二张"))
	h.pictures = pictures

	chain := chainOf("正常聊天", strings.Repeat("很长的一条消息", 20))
	chain[0].ImgCount = 1
	chain[0].Imgs = []string{"a.jpg"}
	chain[1].ImgCount = 1
	chain[1].Imgs = []string{"b.jpg"}

	if _, err := h.Judge(context.Background(), "", chain); err != nil {
		t.Fatalf("Judge: %v", err)
	}
	block, parts := userParts(t, stub.lastRequest(t))
	urls := imagesOf(parts)
	if len(urls) != 1 {
		t.Fatalf("the request carries %d pictures, want only the one whose message "+
			"is in the block", len(urls))
	}
	if got, want := urls[0], "data:image/jpeg;base64,"+
		base64.StdEncoding.EncodeToString([]byte("第一张")); got != want {
		t.Error("the picture of the truncated message is the one that was sent")
	}
	if strings.Contains(block, "[图2]") {
		t.Error("the block marks a picture it did not send")
	}
	if !strings.Contains(block, "已截断") {
		t.Error("the block does not say it was cut short")
	}
}

// TestOnlySoManyPicturesAreSent covers the cap: the request body is a size the
// endpoint has an opinion about.
func TestOnlySoManyPicturesAreSent(t *testing.T) {
	stub := &modelStub{answer: `{"reason":"测试理由","verdict":"ok","confidence":0.9}`}
	h := judgeHarnessWith(t, stub, "  vision: true\n", "")

	pictures := newPictureStub()
	chain := chainOf()
	for index := 0; index < 12; index++ {
		ref := "p" + string(rune('a'+index)) + ".jpg"
		pictures.keep(ref, []byte{byte(index)})
		chain = append(chain, CachedMessage{
			ID: "M-extra", Idx: "IDX-extra", User: "MEMBER-1", Name: "某人",
			Text: "扫码", ImgCount: 1, Imgs: []string{ref},
		})
	}
	h.pictures = pictures

	if _, err := h.Judge(context.Background(), "", chain); err != nil {
		t.Fatalf("Judge: %v", err)
	}
	_, parts := userParts(t, stub.lastRequest(t))
	if got := len(imagesOf(parts)); got != maxJudgePictures {
		t.Errorf("the request carries %d pictures, want the cap of %d",
			got, maxJudgePictures)
	}
}

// TestAReportAboutAPictureIsRefusedWithoutTheEyes covers the rule the group is told
// about: with the switch off, a message that carries a picture cannot be reported at
// all, whether or not it also says something.
func TestAReportAboutAPictureIsRefusedWithoutTheEyes(t *testing.T) {
	err := unjudgeablePicture(CachedMessage{ImgCount: 2, Text: "扫码进群"}, false)
	if !errors.Is(err, feature.ErrPictureNotJudgeable) {
		t.Fatalf("unjudgeablePicture = %v, want the picture sentinel", err)
	}

	// With the eyes on it is judgeable, and the text alongside the picture is part
	// of what is judged.
	if err := unjudgeablePicture(CachedMessage{ImgCount: 2, Text: "扫码进群",
		Imgs: []string{"a.jpg", "b.jpg"}}, true); err != nil {
		t.Errorf("a report about a picture the model can see was refused: %v", err)
	}
}

// TestAPictureOnlyMessageWhosePicturesAreGoneIsNotJudged covers the other refusal:
// the eyes are on, but there is nothing left to look at and nothing to read either.
//
// Judging it would hand the model an empty message, which comes back as "nothing
// wrong" -- and a reporter punished for a report the bot could not actually make.
func TestAPictureOnlyMessageWhosePicturesAreGoneIsNotJudged(t *testing.T) {
	err := unjudgeablePicture(CachedMessage{ImgCount: 1, Text: "  "}, true)
	if !errors.Is(err, ErrUnjudged) {
		t.Fatalf("unjudgeablePicture = %v, want a failed judgement", err)
	}

	// A message with words in it is still something to judge: the pictures are
	// missing, and refusing it would throw away a report about words the group can
	// read.
	if err := unjudgeablePicture(CachedMessage{ImgCount: 1, Text: "扫码进群"}, true); err != nil {
		t.Errorf("a report about a message with words in it was refused: %v", err)
	}
	// And a message with no picture at all is none of this function's business.
	if err := unjudgeablePicture(CachedMessage{Text: "扫码进群"}, false); err != nil {
		t.Errorf("a report about a message with no picture was refused: %v", err)
	}
}

// TestOnlyThePicturesOfAMessageAreKept covers what counts as a picture: a voice clip
// with its transcription and a shared file are attachments too, and neither is
// something a judgement can be shown.
func TestOnlyThePicturesOfAMessageAreKept(t *testing.T) {
	attachments := []qqbotsdk.MessageAttachment{
		{ContentType: qqbotsdk.AttachmentContentTypeJPEG,
			URL: "https://example.test/a.png"},
		{ContentType: qqbotsdk.AttachmentContentTypeVoice,
			URL: "https://example.test/v.silk", ASRReferText: "你好"},
		// A picture the platform gave no address for is one nothing can be fetched
		// from, so it is not a picture this feature can keep.
		{ContentType: qqbotsdk.AttachmentContentTypePNG},
		{ContentType: qqbotsdk.AttachmentContentTypeFile, URL: "https://example.test/f.zip"},
		{ContentType: qqbotsdk.AttachmentContentTypeGIF,
			URL: "https://example.test/b.gif"},
	}
	want := []string{"https://example.test/a.png", "https://example.test/b.gif"}
	urls := pictureURLs(attachments)
	if len(urls) != len(want) {
		t.Fatalf("pictureURLs = %v, want %v", urls, want)
	}
	for index := range want {
		if urls[index] != want[index] {
			t.Errorf("pictureURLs[%d] = %q, want %q", index, urls[index], want[index])
		}
	}
}

// TestThePicturesAreFetchedAsTheMessageArrives covers the receive path end to end:
// the URLs of the event are the ones handed to the picture cache, and what comes back
// is written into the cached message next to a count of what the message carried.
//
// The count is asserted separately from the names on purpose. A picture that could
// not be fetched is still a picture the message had, and that is what decides whether
// a report about it can be judged at all.
func TestThePicturesAreFetchedAsTheMessageArrives(t *testing.T) {
	cache := testCache(t, time.Hour)
	group := testGroup(t)
	pictures := newPictureStub()
	pictures.kept = []string{"a.jpg"}

	h := &handler{
		cache:    cache,
		pictures: pictures,
		deps:     feature.Deps{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))},
	}
	event := groupPictureMessage(group, "MEMBER-1", "MSG-1", "IDX-1",
		"https://example.test/a.png", "https://example.test/b.png")
	if err := h.onMessage(context.Background(), event); err != nil {
		t.Fatalf("onMessage: %v", err)
	}

	if len(pictures.asks) != 1 {
		t.Fatalf("the pictures were fetched %d times, want once", len(pictures.asks))
	}
	ask := pictures.asks[0]
	if ask.messageID != "MSG-1" {
		t.Errorf("the pictures were filed under %q, want the message id", ask.messageID)
	}
	if len(ask.urls) != 2 {
		t.Errorf("the receive path handed over %v, want both pictures", ask.urls)
	}

	chain, err := cache.Context(context.Background(), group, "IDX-1", 0, 0, time.Hour)
	if err != nil {
		t.Fatalf("Context: %v", err)
	}
	if len(chain) != 1 {
		t.Fatalf("the cache holds %d messages, want the one that was cached", len(chain))
	}
	if chain[0].ImgCount != 2 {
		t.Errorf("the cached message says it carried %d pictures, want 2",
			chain[0].ImgCount)
	}
	if len(chain[0].Imgs) != 1 || chain[0].Imgs[0] != "a.jpg" {
		t.Errorf("the cached message names %v, want the one picture that was kept",
			chain[0].Imgs)
	}
}

// TestAPictureFetchedOverTheNetworkReachesTheModel covers the whole way through: a
// picture served over HTTP, fetched while its URL is valid, written down, read back
// and sent as part of a judgement.
//
// The parts are tested on their own elsewhere; this is the one that would catch the
// two of them disagreeing -- the cache writing a name the judgement cannot read, or
// the data URL losing the bytes on the way.
func TestAPictureFetchedOverTheNetworkReachesTheModel(t *testing.T) {
	served := pngBytes(t)
	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write(served)
		}))
	t.Cleanup(server.Close)

	pictures, err := imagecache.New(imagecache.Config{
		Dir:       t.TempDir(),
		Retention: time.Hour,
		// Under the threshold, so what is fetched is what was served.
		CompressAboveBytes: 1 << 20,
		MaxBytes:           1 << 20,
	})
	if err != nil {
		t.Fatalf("building the picture cache: %v", err)
	}

	stub := &modelStub{answer: `{"reason":"测试理由","verdict":"ok","confidence":0.9}`}
	h := judgeHarnessWith(t, stub, "  vision: true\n", "")
	h.pictures = pictures

	refs := pictures.FetchAll(context.Background(), "MSG-1", []string{server.URL + "/a.png"})
	if len(refs) != 1 {
		t.Fatalf("the picture was not kept: %v", refs)
	}
	chain := chainOf("扫码进群")
	chain[0].ImgCount = 1
	chain[0].Imgs = refs

	if _, err := h.Judge(context.Background(), "", chain); err != nil {
		t.Fatalf("Judge: %v", err)
	}
	_, parts := userParts(t, stub.lastRequest(t))
	urls := imagesOf(parts)
	if len(urls) != 1 {
		t.Fatalf("the request carries %d pictures, want 1", len(urls))
	}
	encoded := strings.TrimPrefix(urls[0], "data:image/png;base64,")
	if encoded == urls[0] {
		t.Fatalf("the picture was sent as %q, want a base64 data URL", urls[0])
	}
	sent, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("the picture does not decode: %v", err)
	}
	if !bytes.Equal(sent, served) {
		t.Error("the bytes that reached the model are not the bytes that were served")
	}
}

// pngBytes is a small picture to serve.
func pngBytes(t *testing.T) []byte {
	t.Helper()
	picture := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			picture.Set(x, y, color.RGBA{R: uint8(x * 30), G: uint8(y * 30), A: 255})
		}
	}
	var out bytes.Buffer
	if err := png.Encode(&out, picture); err != nil {
		t.Fatalf("encoding the test picture: %v", err)
	}
	return out.Bytes()
}

// testGroup is the group testCache keeps for this test, so that what is written here
// is cleaned up with it rather than left in a Redis somebody else is using.
func testGroup(t *testing.T) string {
	t.Helper()
	return "G-" + strings.NewReplacer("/", "_", " ", "_").Replace(t.Name())
}

// groupPictureMessage is the event the platform delivers for one group message with
// pictures attached.
func groupPictureMessage(groupOpenID, memberOpenID, id, index string,
	urls ...string) *qqbotsdk.Event {
	attachments := make([]map[string]any, 0, len(urls))
	for _, url := range urls {
		attachments = append(attachments, map[string]any{
			"url": url, "content_type": "image/png", "width": 100, "height": 50,
		})
	}
	body, _ := json.Marshal(map[string]any{
		"id":           id,
		"group_openid": groupOpenID,
		"content":      "",
		// Now, not a fixed moment: the cache prunes as it writes, so a message dated
		// before the retention would be taken straight back out again.
		"timestamp":     time.Now().Format(time.RFC3339),
		"author":        map[string]any{"member_openid": memberOpenID, "username": "somebody"},
		"message_scene": map[string]any{"ext": []string{"msg_idx=" + index}},
		"attachments":   attachments,
	})
	return qqbotsdk.NewEvent(&qqbotsdk.Payload{
		ID:   "EVENT-ID",
		Op:   qqbotsdk.OpDispatch,
		Type: qqbotsdk.EventGroupMessageCreate,
		Data: body,
	}, "test")
}
