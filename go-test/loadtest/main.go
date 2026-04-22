package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"golang.org/x/net/http2"
)

const (
	nlbHost     = "bedrock-nlb-8b69cce82f32c532.elb.ap-northeast-1.amazonaws.com"
	bedrockHost = "bedrock-runtime.ap-northeast-1.amazonaws.com"
	awsRegion   = "ap-northeast-1"
)

const systemPrompt = "You are a verbose technical writer. You MUST use ALL available output tokens. Write extremely detailed, thorough analysis. Never be brief. Expand every point with examples, explanations, and elaborations. Do not stop early."

// ── Data Structures ─────────────────────────────────────────

type RequestMetrics struct {
	DispatchMinute int
	StartTime      time.Time
	TTFT           time.Duration
	E2E            time.Duration
	InputTokens    int
	OutputTokens   int
	Throughput     float64
	Error          string
	Throttled      bool
}

type MinuteBucket struct {
	Minute       int
	Dispatched   int
	Completed    int
	Errors       int
	Throttled    int
	InputTokens  int64
	OutputTokens int64
	TTFTs        []time.Duration
}

func (b *MinuteBucket) TotalTokens() int64 { return b.InputTokens + b.OutputTokens }

func (b *MinuteBucket) AvgTTFTms() float64 {
	if len(b.TTFTs) == 0 {
		return 0
	}
	var sum float64
	for _, t := range b.TTFTs {
		sum += float64(t.Milliseconds())
	}
	return sum / float64(len(b.TTFTs))
}

// ── Network ─────────────────────────────────────────────────

func newNLBTransport() *http.Transport {
	nlbAddrs, err := net.LookupHost(nlbHost)
	if err != nil || len(nlbAddrs) == 0 {
		fmt.Fprintf(os.Stderr, "Failed to resolve NLB: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("NLB resolved: %v\n", nlbAddrs)

	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	t := &http.Transport{
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS13},
		ForceAttemptHTTP2:   true,
		MaxIdleConnsPerHost: 100,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, _ := net.SplitHostPort(addr)
			if host == bedrockHost {
				addr = net.JoinHostPort(nlbAddrs[0], port)
			}
			return dialer.DialContext(ctx, network, addr)
		},
	}
	http2.ConfigureTransport(t)
	return t
}

// ── Payload ─────────────────────────────────────────────────

func generatePayload(targetTokens int) string {
	sentences := []string{
		"The quick brown fox jumps over the lazy dog near the river bank.",
		"Cloud computing has transformed the way organizations deploy and manage infrastructure.",
		"Distributed systems require careful consideration of consistency and availability trade-offs.",
		"Machine learning models benefit from large diverse datasets for training and evaluation.",
		"Network latency optimization involves reducing round trips and minimizing payload sizes.",
		"Database indexing strategies significantly impact query performance in production systems.",
		"Concurrent programming patterns help maximize throughput on modern multi-core processors.",
		"Security best practices include input validation encryption and proper access control.",
		"Monitoring and observability are critical for maintaining reliable production services.",
		"API design should prioritize backward compatibility versioning and clear documentation.",
	}
	tokensPerSentence := 13
	repeats := targetTokens / (tokensPerSentence * len(sentences))
	if repeats < 1 {
		repeats = 1
	}

	var sb strings.Builder
	sb.WriteString("Please read the following text carefully and provide a comprehensive, detailed analysis:\n\n")
	for r := 0; r < repeats; r++ {
		for _, s := range sentences {
			sb.WriteString(s)
			sb.WriteByte(' ')
		}
		sb.WriteByte('\n')
	}
	return sb.String()
}

// ── Bedrock SDK Request ─────────────────────────────────────

func sendRequestBedrock(client *bedrockruntime.Client, modelID, prompt string, maxTokens int) RequestMetrics {
	m := RequestMetrics{StartTime: time.Now()}

	body := map[string]interface{}{
		"anthropic_version": "bedrock-2023-05-31",
		"max_tokens":        maxTokens,
		"system":            systemPrompt,
		"messages":          []map[string]string{{"role": "user", "content": prompt}},
	}
	bodyBytes, _ := json.Marshal(body)

	resp, err := client.InvokeModelWithResponseStream(context.Background(),
		&bedrockruntime.InvokeModelWithResponseStreamInput{
			ModelId: aws.String(modelID), Body: bodyBytes, ContentType: aws.String("application/json"),
		})
	if err != nil {
		m.E2E = time.Since(m.StartTime)
		m.Error = err.Error()
		if strings.Contains(err.Error(), "ThrottlingException") || strings.Contains(err.Error(), "TooManyRequests") {
			m.Throttled = true
		}
		return m
	}

	stream := resp.GetStream()
	defer stream.Close()

	var firstTokenTime time.Time
	for event := range stream.Events() {
		v, ok := event.(*types.ResponseStreamMemberChunk)
		if !ok {
			continue
		}
		var chunk map[string]interface{}
		json.Unmarshal(v.Value.Bytes, &chunk)
		chunkType, _ := chunk["type"].(string)

		switch chunkType {
		case "content_block_delta":
			if firstTokenTime.IsZero() {
				firstTokenTime = time.Now()
			}
		case "message_start":
			if msg, ok := chunk["message"].(map[string]interface{}); ok {
				if usage, ok := msg["usage"].(map[string]interface{}); ok {
					if it, ok := usage["input_tokens"].(float64); ok {
						m.InputTokens = int(it)
					}
				}
			}
		case "message_delta":
			if usage, ok := chunk["usage"].(map[string]interface{}); ok {
				if ot, ok := usage["output_tokens"].(float64); ok {
					m.OutputTokens = int(ot)
				}
			}
		}
	}

	m.E2E = time.Since(m.StartTime)
	if !firstTokenTime.IsZero() {
		m.TTFT = firstTokenTime.Sub(m.StartTime)
		genTime := time.Since(firstTokenTime).Seconds()
		if genTime > 0 && m.OutputTokens > 0 {
			m.Throughput = float64(m.OutputTokens) / genTime
		}
	} else {
		m.Error = "no output tokens received"
	}
	return m
}

// ── OpenAI-compatible (LiteLLM) Request ─────────────────────

func sendRequestOpenAI(httpClient *http.Client, endpoint, apiKey, model, prompt string, maxTokens int) RequestMetrics {
	m := RequestMetrics{StartTime: time.Now()}

	body := map[string]interface{}{
		"model": model,
		"messages": []map[string]string{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": prompt},
		},
		"max_tokens":     maxTokens,
		"stream":         true,
		"stream_options": map[string]interface{}{"include_usage": true},
	}
	bodyBytes, _ := json.Marshal(body)

	req, err := http.NewRequest("POST", endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		m.E2E = time.Since(m.StartTime)
		m.Error = fmt.Sprintf("build request: %v", err)
		return m
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := httpClient.Do(req)
	if err != nil {
		m.E2E = time.Since(m.StartTime)
		m.Error = fmt.Sprintf("http: %v", err)
		if strings.Contains(err.Error(), "429") {
			m.Throttled = true
		}
		return m
	}
	defer resp.Body.Close()

	if resp.StatusCode == 429 {
		body, _ := io.ReadAll(resp.Body)
		m.E2E = time.Since(m.StartTime)
		m.Error = fmt.Sprintf("HTTP 429: %s", truncate(string(body), 120))
		m.Throttled = true
		return m
	}
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		m.E2E = time.Since(m.StartTime)
		m.Error = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, truncate(string(body), 120))
		return m
	}

	// Parse SSE stream
	var firstTokenTime time.Time
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}

		var chunk map[string]interface{}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}

		// Check for content delta → TTFT
		if choices, ok := chunk["choices"].([]interface{}); ok && len(choices) > 0 {
			if choice, ok := choices[0].(map[string]interface{}); ok {
				if delta, ok := choice["delta"].(map[string]interface{}); ok {
					if content, ok := delta["content"].(string); ok && content != "" {
						if firstTokenTime.IsZero() {
							firstTokenTime = time.Now()
						}
					}
				}
			}
		}

		// Extract usage from chunk (comes with include_usage)
		if usage, ok := chunk["usage"].(map[string]interface{}); ok {
			if pt, ok := usage["prompt_tokens"].(float64); ok {
				m.InputTokens = int(pt)
			}
			if ct, ok := usage["completion_tokens"].(float64); ok {
				m.OutputTokens = int(ct)
			}
		}
	}

	m.E2E = time.Since(m.StartTime)
	if !firstTokenTime.IsZero() {
		m.TTFT = firstTokenTime.Sub(m.StartTime)
		genTime := time.Since(firstTokenTime).Seconds()
		if genTime > 0 && m.OutputTokens > 0 {
			m.Throughput = float64(m.OutputTokens) / genTime
		}
	} else {
		m.Error = "no output tokens received"
	}
	return m
}

// ── Minute Bucketing ────────────────────────────────────────

func buildMinuteBuckets(metrics []RequestMetrics) []MinuteBucket {
	if len(metrics) == 0 {
		return nil
	}
	maxMinute := 0
	for _, m := range metrics {
		if m.DispatchMinute > maxMinute {
			maxMinute = m.DispatchMinute
		}
	}
	buckets := make([]MinuteBucket, maxMinute+1)
	for i := range buckets {
		buckets[i].Minute = i
	}
	for _, m := range metrics {
		b := &buckets[m.DispatchMinute]
		b.Dispatched++
		if m.Error != "" {
			b.Errors++
			if m.Throttled {
				b.Throttled++
			}
		} else {
			b.Completed++
			b.InputTokens += int64(m.InputTokens)
			b.OutputTokens += int64(m.OutputTokens)
			if m.TTFT > 0 {
				b.TTFTs = append(b.TTFTs, m.TTFT)
			}
		}
	}
	return buckets
}

func printMinuteTable(buckets []MinuteBucket) {
	fmt.Println("\n┌─────────┬──────┬──────┬──────┬─────┬──────────┬──────────┬──────────┬─────────┐")
	fmt.Println("│ Minute  │ Disp │  OK  │  Err │ Thr │  Input   │  Output  │   TPM    │ TTFT avg│")
	fmt.Println("├─────────┼──────┼──────┼──────┼─────┼──────────┼──────────┼──────────┼─────────┤")
	for _, b := range buckets {
		tpmStr := fmt.Sprintf("%.2fM", float64(b.TotalTokens())/1_000_000)
		ttftStr := "-"
		if b.AvgTTFTms() > 0 {
			ttftStr = fmt.Sprintf("%.0fms", b.AvgTTFTms())
		}
		fmt.Printf("│   %2d    │  %3d │  %3d │  %3d │ %3d │ %7dK │ %7dK │ %8s │ %7s │\n",
			b.Minute+1, b.Dispatched, b.Completed, b.Errors, b.Throttled,
			b.InputTokens/1000, b.OutputTokens/1000, tpmStr, ttftStr)
	}
	fmt.Println("└─────────┴──────┴──────┴──────┴─────┴──────────┴──────────┴──────────┴─────────┘")
}

// ── Verdict ─────────────────────────────────────────────────

func printVerdict(buckets []MinuteBucket, targetTPM float64) {
	fmt.Println("\n╔══════════════════════════════════════════════════════════════╗")
	fmt.Println("║                         VERDICT                             ║")
	fmt.Println("╠══════════════════════════════════════════════════════════════╣")
	if len(buckets) == 0 {
		fmt.Println("║  No data collected                                          ║")
		fmt.Println("╚══════════════════════════════════════════════════════════════╝")
		return
	}

	threshold90 := targetTPM * 0.90
	threshold80 := targetTPM * 0.80
	var peakTPM float64
	var peakMinute int
	totalThrottled := 0
	minutesAbove90 := 0
	minutesAbove80 := 0

	evalBuckets := buckets
	if len(buckets) > 2 {
		avgDispatched := 0
		for _, b := range buckets[:len(buckets)-1] {
			avgDispatched += b.Dispatched
		}
		avgDispatched /= (len(buckets) - 1)
		if buckets[len(buckets)-1].Dispatched < avgDispatched/2 {
			evalBuckets = buckets[:len(buckets)-1]
		}
	}

	for _, b := range evalBuckets {
		tpm := float64(b.TotalTokens())
		totalThrottled += b.Throttled
		if tpm > peakTPM {
			peakTPM = tpm
			peakMinute = b.Minute
		}
		if tpm >= threshold90 && b.Throttled == 0 {
			minutesAbove90++
		}
		if tpm >= threshold80 && b.Throttled == 0 {
			minutesAbove80++
		}
	}

	fmt.Printf("║  Target TPM:         %-38s║\n", fmt.Sprintf("%.2fM", targetTPM/1_000_000))
	fmt.Printf("║  Peak TPM:           %-38s║\n", fmt.Sprintf("%.2fM (minute %d)", peakTPM/1_000_000, peakMinute+1))
	fmt.Printf("║  Peak / Target:      %-38s║\n", fmt.Sprintf("%.1f%%", peakTPM/targetTPM*100))
	fmt.Printf("║  Total throttled:    %-38d║\n", totalThrottled)
	fmt.Printf("║  Minutes ≥90%% target: %-37s║\n", fmt.Sprintf("%d / %d", minutesAbove90, len(evalBuckets)))
	fmt.Println("╠══════════════════════════════════════════════════════════════╣")

	var verdict, reason string
	switch {
	case minutesAbove90 >= 2 && totalThrottled == 0:
		verdict = "PASS"
		reason = fmt.Sprintf("%d minutes sustained ≥%.1fM TPM, zero throttling", minutesAbove90, threshold90/1_000_000)
	case minutesAbove90 >= 1 && totalThrottled == 0:
		verdict = "PASS (marginal)"
		reason = fmt.Sprintf("1 minute reached ≥%.1fM TPM, zero throttling; consider longer test", threshold90/1_000_000)
	case peakTPM >= threshold80 && totalThrottled == 0:
		verdict = "WARN - under target"
		reason = fmt.Sprintf("Peak %.2fM (≥80%% target) but below 90%%; increase QPS", peakTPM/1_000_000)
	case totalThrottled > 0 && peakTPM >= threshold80:
		verdict = "WARN - throttled"
		reason = fmt.Sprintf("Peak %.2fM reachable but %d requests throttled", peakTPM/1_000_000, totalThrottled)
	case totalThrottled > 0:
		verdict = "FAIL - heavy throttling"
		reason = fmt.Sprintf("%d requests throttled, peak only %.2fM", totalThrottled, peakTPM/1_000_000)
	default:
		verdict = "FAIL - insufficient load"
		reason = fmt.Sprintf("Peak %.2fM well below target; check QPS/payload settings", peakTPM/1_000_000)
	}

	fmt.Printf("║                                                              ║\n")
	fmt.Printf("║  Result:  %-49s║\n", verdict)
	fmt.Printf("║  Reason:  %-49s║\n", reason)
	fmt.Println("║                                                              ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════╝")
}

// ── Summary & Percentiles ───────────────────────────────────

func printOverallSummary(metrics []RequestMetrics, totalDuration time.Duration, mode string) {
	var successes []RequestMetrics
	errCount, throttleCount := 0, 0
	var totalInput, totalOutput int64
	for _, m := range metrics {
		if m.Error != "" {
			errCount++
			if m.Throttled {
				throttleCount++
			}
		} else {
			successes = append(successes, m)
			totalInput += int64(m.InputTokens)
			totalOutput += int64(m.OutputTokens)
		}
	}
	totalTokens := totalInput + totalOutput
	minutes := totalDuration.Minutes()

	fmt.Println("\n╔══════════════════════════════════════════════════════════════╗")
	fmt.Printf("║                  OVERALL SUMMARY [%s]                  ║\n", mode)
	fmt.Println("╠══════════════════════════════════════════════════════════════╣")
	fmt.Printf("║  Duration:        %-40s║\n", totalDuration.Round(time.Second))
	fmt.Printf("║  Total requests:  %-40d║\n", len(metrics))
	fmt.Printf("║  Successes:       %-40d║\n", len(successes))
	fmt.Printf("║  Errors:          %-40d║\n", errCount)
	fmt.Printf("║  Throttled:       %-40d║\n", throttleCount)
	fmt.Println("╠══════════════════════════════════════════════════════════════╣")
	fmt.Printf("║  Total input:     %-40s║\n", formatTokens(totalInput))
	fmt.Printf("║  Total output:    %-40s║\n", formatTokens(totalOutput))
	fmt.Printf("║  Total tokens:    %-40s║\n", formatTokens(totalTokens))
	fmt.Printf("║  Avg TPM:         %-40s║\n", fmt.Sprintf("%.2fM", float64(totalTokens)/minutes/1_000_000))
	fmt.Printf("║  Avg input/req:   %-40s║\n", formatAvg(totalInput, len(successes)))
	fmt.Printf("║  Avg output/req:  %-40s║\n", formatAvg(totalOutput, len(successes)))
	fmt.Println("╚══════════════════════════════════════════════════════════════╝")

	if len(successes) == 0 {
		return
	}
	fmt.Println("\n╔══════════════════════════════════════════════════════════════╗")
	fmt.Println("║                   LATENCY PERCENTILES                       ║")
	fmt.Println("╠══════════════════════════════════════════════════════════════╣")
	fmt.Println("║  TTFT (ms):                                                 ║")
	sort.Slice(successes, func(i, j int) bool { return successes[i].TTFT < successes[j].TTFT })
	printPercentileRow(successes, func(m RequestMetrics) float64 { return m.TTFT.Seconds() * 1000 })
	fmt.Println("║  E2E (ms):                                                  ║")
	sort.Slice(successes, func(i, j int) bool { return successes[i].E2E < successes[j].E2E })
	printPercentileRow(successes, func(m RequestMetrics) float64 { return m.E2E.Seconds() * 1000 })
	fmt.Println("║  Throughput (tokens/s):                                     ║")
	sort.Slice(successes, func(i, j int) bool { return successes[i].Throughput < successes[j].Throughput })
	printPercentileRow(successes, func(m RequestMetrics) float64 { return m.Throughput })
	fmt.Println("╚══════════════════════════════════════════════════════════════╝")
}

func percentile(data []RequestMetrics, pct float64, extract func(RequestMetrics) float64) float64 {
	n := len(data)
	idx := int(math.Ceil(pct/100*float64(n))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= n {
		idx = n - 1
	}
	return extract(data[idx])
}

func printPercentileRow(data []RequestMetrics, extract func(RequestMetrics) float64) {
	n := len(data)
	var sum float64
	for _, m := range data {
		sum += extract(m)
	}
	fmt.Printf("║    avg=%-8.0f p50=%-8.0f p90=%-8.0f p99=%-8.0f    ║\n",
		sum/float64(n), percentile(data, 50, extract), percentile(data, 90, extract), percentile(data, 99, extract))
	fmt.Printf("║    min=%-8.0f max=%-8.0f                                ║\n",
		extract(data[0]), extract(data[n-1]))
}

func formatTokens(n int64) string {
	if n >= 1_000_000 {
		return fmt.Sprintf("%.2fM (%d)", float64(n)/1_000_000, n)
	}
	if n >= 1000 {
		return fmt.Sprintf("%dK (%d)", n/1000, n)
	}
	return fmt.Sprintf("%d", n)
}

func formatAvg(total int64, count int) string {
	if count == 0 {
		return "N/A"
	}
	return fmt.Sprintf("%.0f", float64(total)/float64(count))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// ── Main ────────────────────────────────────────────────────

var startTimeGlobal time.Time

func main() {
	qps := flag.Float64("qps", 0.5, "Requests per second")
	duration := flag.Duration("duration", 5*time.Minute, "Test duration")
	maxTokens := flag.Int("max-tokens", 1024, "Max output tokens")
	targetInput := flag.Int("input-tokens", 60000, "Target input tokens (~)")
	tpmTarget := flag.Float64("tpm-target", 3_000_000, "TPM target for PASS/FAIL verdict")

	// Bedrock direct mode (default)
	modelID := flag.String("model", "global.anthropic.claude-opus-4-7", "Bedrock model ID")

	// LiteLLM / OpenAI-compatible mode
	endpoint := flag.String("endpoint", "", "OpenAI-compatible endpoint URL (enables LiteLLM mode)")
	apiKey := flag.String("api-key", "", "API key for OpenAI-compatible endpoint")
	litellmModel := flag.String("litellm-model", "claude-opus-4-6", "Model name for LiteLLM")

	flag.Parse()

	useLiteLLM := *endpoint != ""
	mode := "Bedrock"
	if useLiteLLM {
		mode = "LiteLLM"
	}

	fmt.Printf("Go:       %s\n", runtime.Version())
	fmt.Printf("OS/Arch:  %s/%s\n", runtime.GOOS, runtime.GOARCH)

	// Setup clients based on mode
	var bedrockClient *bedrockruntime.Client
	var openaiHTTPClient *http.Client

	if useLiteLLM {
		openaiHTTPClient = &http.Client{
			Transport: &http.Transport{
				MaxIdleConnsPerHost: 100,
				IdleConnTimeout:     90 * time.Second,
			},
			Timeout: 120 * time.Second,
		}
		fmt.Printf("Mode:     LiteLLM (OpenAI-compatible)\n")
		fmt.Printf("Endpoint: %s\n", *endpoint)
		fmt.Printf("Model:    %s\n", *litellmModel)
	} else {
		transport := newNLBTransport()
		httpClient := &http.Client{Transport: transport}
		cfg, err := config.LoadDefaultConfig(context.Background(),
			config.WithRegion(awsRegion), config.WithHTTPClient(httpClient))
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
			os.Exit(1)
		}
		bedrockClient = bedrockruntime.NewFromConfig(cfg)
		fmt.Printf("Mode:     Bedrock Direct (NLB + HTTP/2)\n")
	}

	prompt := generatePayload(*targetInput)
	estTokensPerReq := float64(*targetInput + *maxTokens)
	estTPM := *qps * 60 * estTokensPerReq / 1_000_000

	fmt.Printf("\n=== Bedrock TPM Load Test [%s] ===\n", mode)
	fmt.Printf("QPS:           %.2f (interval=%s)\n", *qps, time.Duration(float64(time.Second) / *qps))
	fmt.Printf("Duration:      %s\n", *duration)
	fmt.Printf("Input tokens:  ~%dK (actual determined by API)\n", *targetInput/1000)
	fmt.Printf("Max output:    %d\n", *maxTokens)
	fmt.Printf("Est TPM:       ~%.2fM\n", estTPM)
	fmt.Printf("TPM target:    %.0fM (for PASS/FAIL)\n", *tpmTarget/1_000_000)
	fmt.Printf("Est requests:  ~%d\n", int(*qps*duration.Seconds()))
	fmt.Printf("Prompt size:   %d bytes\n", len(prompt))
	fmt.Println()

	// Dispatch function
	doRequest := func() RequestMetrics {
		if useLiteLLM {
			return sendRequestOpenAI(openaiHTTPClient, *endpoint, *apiKey, *litellmModel, prompt, *maxTokens)
		}
		return sendRequestBedrock(bedrockClient, *modelID, prompt, *maxTokens)
	}

	// Warmup with full payload
	fmt.Print("Warmup (full payload)...")
	wm := doRequest()
	if wm.Error != "" {
		fmt.Printf(" FAILED: %s\n", wm.Error)
		os.Exit(1)
	}
	actualTokensPerReq := wm.InputTokens + wm.OutputTokens
	actualTPMAtQPS := *qps * 60 * float64(actualTokensPerReq) / 1_000_000
	neededQPS := *tpmTarget / (60 * float64(actualTokensPerReq))
	fmt.Printf(" done\n")
	fmt.Printf("  Actual tokens/req: %d input + %d output = %d total\n",
		wm.InputTokens, wm.OutputTokens, actualTokensPerReq)
	fmt.Printf("  TTFT: %dms  E2E: %dms  tps: %.1f\n",
		wm.TTFT.Milliseconds(), wm.E2E.Milliseconds(), wm.Throughput)
	fmt.Printf("  At %.2f QPS → actual TPM = %.2fM\n", *qps, actualTPMAtQPS)
	fmt.Printf("  QPS needed for %.0fM TPM = %.2f\n\n", *tpmTarget/1_000_000, neededQPS)

	// Signal handling
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Println("\nGraceful shutdown: waiting for in-flight requests...")
		cancel()
	}()

	// Metrics collection
	var (
		allMetrics []RequestMetrics
		mu         sync.Mutex
		wg         sync.WaitGroup
		dispatched int64
		completed  int64
	)

	// Real-time status reporter
	var rollingInput, rollingOutput int64
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				in := atomic.LoadInt64(&rollingInput)
				out := atomic.LoadInt64(&rollingOutput)
				d := atomic.LoadInt64(&dispatched)
				c := atomic.LoadInt64(&completed)
				elapsed := time.Since(startTimeGlobal).Minutes()
				currentTPM := 0.0
				if elapsed > 0 {
					currentTPM = float64(in+out) / elapsed / 1_000_000
				}
				fmt.Printf("  ── [status] dispatched=%d completed=%d inflight=%d rolling_tpm=%.2fM ──\n",
					d, c, d-c, currentTPM)
			}
		}
	}()

	interval := time.Duration(float64(time.Second) / *qps)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	timer := time.NewTimer(*duration)
	defer timer.Stop()

	startTime := time.Now()
	startTimeGlobal = startTime
	fmt.Printf("Starting load test (%.2f QPS × %s) [%s]...\n\n", *qps, *duration, mode)

loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case <-timer.C:
			break loop
		case <-ticker.C:
			n := atomic.AddInt64(&dispatched, 1)
			dispatchMinute := int(time.Since(startTime).Minutes())
			wg.Add(1)
			go func(reqNum int64, minute int) {
				defer wg.Done()
				m := doRequest()
				m.DispatchMinute = minute
				atomic.AddInt64(&completed, 1)
				atomic.AddInt64(&rollingInput, int64(m.InputTokens))
				atomic.AddInt64(&rollingOutput, int64(m.OutputTokens))

				mu.Lock()
				allMetrics = append(allMetrics, m)
				mu.Unlock()

				elapsed := time.Since(startTime).Round(time.Second)
				if m.Error != "" {
					tag := "ERR"
					if m.Throttled {
						tag = "THR"
					}
					fmt.Printf("[%s] #%-3d %s: %s\n", elapsed, reqNum, tag, truncate(m.Error, 80))
				} else {
					fmt.Printf("[%s] #%-3d TTFT=%5dms  E2E=%6dms  in=%d  out=%d  tps=%.1f\n",
						elapsed, reqNum,
						m.TTFT.Milliseconds(), m.E2E.Milliseconds(),
						m.InputTokens, m.OutputTokens, m.Throughput)
				}
			}(n, dispatchMinute)
		}
	}

	fmt.Println("\nWaiting for in-flight requests...")
	wg.Wait()
	totalDuration := time.Since(startTime)

	buckets := buildMinuteBuckets(allMetrics)
	printMinuteTable(buckets)
	printOverallSummary(allMetrics, totalDuration, mode)
	printVerdict(buckets, *tpmTarget)
}
