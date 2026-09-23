package media

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestOpenAIASRTranscribesInSegments(t *testing.T) {
	var (
		mu        sync.Mutex
		calls     int
		sawRIFF   bool
		seenModel string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/audio/transcriptions" {
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("鉴权头错误: %q", got)
		}
		if err := r.ParseMultipartForm(8 << 20); err != nil {
			t.Errorf("解析 multipart 失败: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if f, _, err := r.FormFile("file"); err == nil {
			head := make([]byte, 4)
			if _, err := io.ReadFull(f, head); err == nil && string(head) == "RIFF" {
				mu.Lock()
				sawRIFF = true
				mu.Unlock()
			}
			_ = f.Close()
		}
		mu.Lock()
		calls++
		n := calls
		seenModel = r.FormValue("model")
		mu.Unlock()
		fmt.Fprintf(w, `{"text":"第%d段"}`+"\n", n)
	}))
	defer srv.Close()

	p := &OpenAIASR{BaseURL: srv.URL, APIKey: "test-key", SegmentMS: 100}
	stream, err := p.Open(context.Background(), ASRConfig{Language: "zh", HotWords: []string{"ZSet"}})
	if err != nil {
		t.Fatalf("打开识别流失败: %v", err)
	}

	// 400ms 音频, 按 100ms 切片。
	//
	// 这里刻意按真实时序推送而不是一次性灌进去: 一次性灌会让后台
	// 消费协程来不及调度, 所有分片被合并成一次请求, 于是测试验证的
	// 就不是"增量转写"这个真正的行为了。真实音频本来就是 20ms 一帧
	// 到达的, 按时序推送也更接近线上。
	for i := 0; i < 20; i++ {
		if err := stream.Push(AudioChunk{PCM: SynthSilence(FrameMS)}); err != nil {
			t.Fatalf("推送音频失败: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("关闭识别流失败: %v", err)
	}

	var results []ASRResult
	for r := range stream.Results() {
		results = append(results, r)
	}
	if len(results) == 0 {
		t.Fatal("应至少产出一个识别结果")
	}
	if len(results) < 2 {
		t.Fatal("增量转写应产出中间结果(partial)后再给 final, 否则实时上屏就没有内容")
	}
	final := results[len(results)-1]
	if !final.Final {
		t.Fatal("最后一个结果必须是 final")
	}

	mu.Lock()
	defer mu.Unlock()
	if calls < 2 {
		t.Fatalf("300ms 音频按 100ms 切片应至少转写 2 次, 实际 %d", calls)
	}
	if !sawRIFF {
		t.Fatal("上传的音频应带 WAV 容器头, 否则多数转写接口会直接拒绝")
	}
	if seenModel != "whisper-1" {
		t.Fatalf("model 字段应为默认值 whisper-1, 实际 %q", seenModel)
	}
	if got := strings.Count(final.Text, "段"); got != calls {
		t.Fatalf("final 文本应拼接全部 %d 段, 实际 %q", calls, final.Text)
	}
}

func TestOpenAIASRRequiresAPIKey(t *testing.T) {
	p := &OpenAIASR{BaseURL: "http://127.0.0.1:1"}
	if _, err := p.Open(context.Background(), ASRConfig{}); err == nil {
		t.Fatal("缺少 API Key 时应直接报错, 而不是等到第一次请求才失败")
	}
}

// 单段转写失败不能让候选人的整段回答丢失: 已确认部分要照常上屏,
// 并且流必须正常收尾(否则上层会一直等一个永远不来的 final)。
func TestOpenAIASRSurvivesSegmentFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"upstream busy"}`, http.StatusInternalServerError)
	}))
	defer srv.Close()

	p := &OpenAIASR{BaseURL: srv.URL, APIKey: "k", SegmentMS: 100}
	stream, err := p.Open(context.Background(), ASRConfig{})
	if err != nil {
		t.Fatalf("打开识别流失败: %v", err)
	}
	for i := 0; i < 10; i++ {
		_ = stream.Push(AudioChunk{PCM: SynthSilence(FrameMS)})
	}
	_ = stream.Close()

	var results []ASRResult
	for r := range stream.Results() {
		results = append(results, r)
	}
	if len(results) == 0 {
		t.Fatal("即使转写全部失败, 也应产出结果(内容为空)并正常收尾")
	}
	if !results[len(results)-1].Final {
		t.Fatal("失败场景下也必须给出 final, 否则上层会一直等待")
	}
}

func TestOpenAITTSStreamsAudio(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/audio/speech" {
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("鉴权头错误: %q", got)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("解析请求体失败: %v", err)
			return
		}
		if body["response_format"] != "pcm" {
			t.Errorf("默认应请求 pcm 裸音频(便于精确换算播放进度), 实际 %v", body["response_format"])
		}
		if body["voice"] != "alloy" {
			t.Errorf("音色应透传, 实际 %v", body["voice"])
		}

		fl, _ := w.(http.Flusher)
		for i := 0; i < 4; i++ {
			if _, err := w.Write(make([]byte, 640)); err != nil {
				return
			}
			if fl != nil {
				fl.Flush()
			}
		}
	}))
	defer srv.Close()

	p := &OpenAITTS{BaseURL: srv.URL, APIKey: "test-key", ChunkBytes: 640}
	stream, err := p.Speak(context.Background(), "你好, 我是本轮的面试官。", Voice{ID: "alloy"})
	if err != nil {
		t.Fatalf("打开合成流失败: %v", err)
	}

	var chunks []AudioChunk
	for c := range stream.Chunks() {
		chunks = append(chunks, c)
	}
	if len(chunks) != 4 {
		t.Fatalf("应收到 4 块音频, 实际 %d", len(chunks))
	}
	if len(chunks[0].PCM) != 640 {
		t.Fatalf("分块大小应为 640 字节, 实际 %d", len(chunks[0].PCM))
	}
	for i, c := range chunks {
		if c.Seq != i {
			t.Errorf("第 %d 块的 Seq 应为 %d, 实际 %d", i, i, c.Seq)
		}
	}
}

func TestOpenAITTSRequiresAPIKey(t *testing.T) {
	p := &OpenAITTS{BaseURL: "http://127.0.0.1:1"}
	if _, err := p.Speak(context.Background(), "你好", Voice{}); err == nil {
		t.Fatal("缺少 API Key 时应直接报错")
	}
}

func TestOpenAITTSHTTPErrorSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"invalid voice"}`, http.StatusBadRequest)
	}))
	defer srv.Close()

	p := &OpenAITTS{BaseURL: srv.URL, APIKey: "k"}
	if _, err := p.Speak(context.Background(), "你好", Voice{}); err == nil {
		t.Fatal("HTTP 4xx 应作为错误返回, 而不是产出一个空音频流")
	}
}

// 取消 context 必须真正中断底层 HTTP 请求, 让服务端也停止产出。
//
// 这是打断链路里最容易被忽略的一环: 只在客户端停止读取,
// 服务端仍会把剩余音频传完, 连接和带宽都被无效流量占住。
func TestOpenAITTSCancelStopsStream(t *testing.T) {
	var serverStopped sync.WaitGroup
	serverStopped.Add(1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer serverStopped.Done()
		fl, _ := w.(http.Flusher)
		for {
			if _, err := w.Write(make([]byte, 640)); err != nil {
				return
			}
			if fl != nil {
				fl.Flush()
			}
			select {
			case <-r.Context().Done():
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	p := &OpenAITTS{BaseURL: srv.URL, APIKey: "k", ChunkBytes: 640}
	stream, err := p.Speak(ctx, "这是一句很长的话。", Voice{})
	if err != nil {
		t.Fatalf("打开合成流失败: %v", err)
	}

	select {
	case <-stream.Chunks():
	case <-time.After(2 * time.Second):
		t.Fatal("未收到任何音频")
	}

	cancel()

	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-stream.Chunks():
			if !ok {
				serverStopped.Wait()
				return
			}
		case <-deadline:
			t.Fatal("取消 context 后音频流没有关闭")
		}
	}
}
