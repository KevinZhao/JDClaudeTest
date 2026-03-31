package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"runtime"
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
	region      = "ap-northeast-1"
	runs        = 3
)

var models = map[string]string{
	"Opus 4.6":   "global.anthropic.claude-opus-4-6-v1",
	"Sonnet 4.6": "global.anthropic.claude-sonnet-4-6",
}

// ── 环境检查 ─────────────────────────────────────────────
func checkEnv() {
	fmt.Printf("Go:       %s\n", runtime.Version())
	fmt.Printf("OS/Arch:  %s/%s\n", runtime.GOOS, runtime.GOARCH)
	fmt.Printf("TLS 1.3:  supported (Go >= 1.13)\n")
	fmt.Printf("HTTP/2:   supported (Go >= 1.6)\n")
	fmt.Println()
}

// ── DNS 重写 transport: Bedrock 域名 → NLB IP ────────────
func newTransport() *http.Transport {
	// 先解析 NLB IP
	nlbAddrs, err := net.LookupHost(nlbHost)
	if err != nil || len(nlbAddrs) == 0 {
		fmt.Fprintf(os.Stderr, "Failed to resolve NLB: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("NLB resolved: %v\n\n", nlbAddrs)

	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}

	t := &http.Transport{
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS13,
		},
		ForceAttemptHTTP2: true,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, _ := net.SplitHostPort(addr)
			if host == bedrockHost {
				// 重写到 NLB IP
				addr = net.JoinHostPort(nlbAddrs[0], port)
			}
			return dialer.DialContext(ctx, network, addr)
		},
	}
	http2.ConfigureTransport(t)
	return t
}

// ── 测量结果 ─────────────────────────────────────────────
type Result struct {
	TTFT_ms      float64
	E2E_ms       float64
	OutputTokens int
	Throughput   float64
}

// ── 单次请求 ─────────────────────────────────────────────
func measure(client *bedrockruntime.Client, modelID, prompt string, maxTokens int) (*Result, error) {
	body := map[string]interface{}{
		"anthropic_version": "bedrock-2023-05-31",
		"max_tokens":        maxTokens,
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
	}
	bodyBytes, _ := json.Marshal(body)

	tStart := time.Now()
	var firstTokenTime time.Time
	outputTokens := 0

	resp, err := client.InvokeModelWithResponseStream(context.Background(),
		&bedrockruntime.InvokeModelWithResponseStreamInput{
			ModelId:     aws.String(modelID),
			Body:        bodyBytes,
			ContentType: aws.String("application/json"),
		})
	if err != nil {
		return nil, err
	}

	stream := resp.GetStream()
	defer stream.Close()

	for event := range stream.Events() {
		switch v := event.(type) {
		case *types.ResponseStreamMemberChunk:
			var chunk map[string]interface{}
			json.Unmarshal(v.Value.Bytes, &chunk)
			chunkType, _ := chunk["type"].(string)

			if chunkType == "content_block_delta" {
				if firstTokenTime.IsZero() {
					firstTokenTime = time.Now()
				}
				outputTokens++
			}
			if chunkType == "message_delta" {
				if usage, ok := chunk["usage"].(map[string]interface{}); ok {
					if ot, ok := usage["output_tokens"].(float64); ok {
						outputTokens = int(ot)
					}
				}
			}
		}
	}

	tEnd := time.Now()

	if firstTokenTime.IsZero() {
		return nil, fmt.Errorf("no output received")
	}

	ttft := firstTokenTime.Sub(tStart).Seconds() * 1000
	e2e := tEnd.Sub(tStart).Seconds() * 1000
	genTime := tEnd.Sub(firstTokenTime).Seconds()
	throughput := 0.0
	if genTime > 0 {
		throughput = float64(outputTokens) / genTime
	}

	return &Result{
		TTFT_ms:      ttft,
		E2E_ms:       e2e,
		OutputTokens: outputTokens,
		Throughput:   throughput,
	}, nil
}

func runTest(client *bedrockruntime.Client, name, modelID, prompt string, maxTokens, numRuns int) {
	fmt.Printf("\n%s\n", "=================================================================")
	fmt.Printf("  Model: %s (%s)\n", name, modelID)
	fmt.Printf("  max_tokens: %d, runs: %d\n", maxTokens, numRuns)
	fmt.Printf("%s\n", "=================================================================")

	// warmup
	fmt.Printf("  [warmup] ...")
	_, err := measure(client, modelID, "hi", 10)
	if err != nil {
		fmt.Printf(" FAILED: %v\n", err)
		return
	}
	fmt.Println(" done")

	var results []Result
	for i := 0; i < numRuns; i++ {
		fmt.Printf("  [run %d/%d] ", i+1, numRuns)
		r, err := measure(client, modelID, prompt, maxTokens)
		if err != nil {
			fmt.Printf("ERROR: %v\n", err)
			continue
		}
		results = append(results, *r)
		fmt.Printf("TTFT=%8.1fms  E2E=%8.1fms  tokens=%4d  throughput=%6.1f t/s\n",
			r.TTFT_ms, r.E2E_ms, r.OutputTokens, r.Throughput)
	}

	if len(results) > 0 {
		var sumTTFT, sumE2E, sumTPS float64
		for _, r := range results {
			sumTTFT += r.TTFT_ms
			sumE2E += r.E2E_ms
			sumTPS += r.Throughput
		}
		n := float64(len(results))
		fmt.Printf("\n  >>> AVG  TTFT=%.1fms  E2E=%.1fms  throughput=%.1f t/s\n",
			sumTTFT/n, sumE2E/n, sumTPS/n)
	}
}

func main() {
	checkEnv()

	transport := newTransport()
	httpClient := &http.Client{Transport: transport}

	cfg, err := config.LoadDefaultConfig(context.Background(),
		config.WithRegion(region),
		config.WithHTTPClient(httpClient),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		os.Exit(1)
	}

	client := bedrockruntime.NewFromConfig(cfg)

	prompt := "Write a detailed explanation of how TCP/IP networking works, including all 4 layers."

	// 按顺序测试
	for _, name := range []string{"Opus 4.6", "Sonnet 4.6"} {
		runTest(client, name, models[name], prompt, 256, runs)
	}

	fmt.Println("\nDone.")
}
