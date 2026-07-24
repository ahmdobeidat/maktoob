// Package asr transcribes audio using a resident whisper.cpp server.
//
// whisper-server is supervised as a long-lived process rather than invoked once
// per note. A per-note invocation would re-read a multi-gigabyte model from disk
// every time, which costs more than the inference itself.
//
// The response format is verbose_json, which is the only format that carries the
// per-segment statistics maktoob depends on: avg_logprob for confidence, and
// no_speech_prob to distinguish an uncertain transcript from a fabricated one.
package asr

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// Result is one transcription.
type Result struct {
	Language string
	Duration float64
	Segments []Segment
}

// Segment is one span of transcribed audio with the model's own statistics.
type Segment struct {
	Start float64
	End   float64
	Text  string

	// AvgLogprob is the mean log probability across the segment's tokens.
	// Exponentiating it gives a geometric mean, which is the right shape for a
	// confidence score: one catastrophic token drags the result down instead of
	// hiding behind several confident ones.
	AvgLogprob float64

	// NoSpeechProb is the model's estimate that this span contains no speech.
	// It is the signal that separates an uncertain transcript from an invented
	// one, because fabricated text over silence carries high token confidence.
	NoSpeechProb float64

	// Temperature above zero means whisper's internal fallback fired: the
	// segment failed its first decode and was retried with more randomness.
	Temperature float64

	Words []Word
}

// Word is one token span with its probability.
type Word struct {
	Word        string
	Start       float64
	End         float64
	Probability float64
}

// Confidence returns exp(AvgLogprob), clamped to [0,1].
//
// This is whisper's own per-segment statistic rather than anything derived from
// the word list. The word array merges multi-token sequences and keeps only the
// first fragment's probability, which discards exactly the uncertain
// continuation tokens — and Arabic is fragmented into byte-level pieces more
// than most languages, so a word-derived mean is least trustworthy precisely
// where maktoob needs it most.
func (s Segment) Confidence() float64 {
	c := math.Exp(s.AvgLogprob)
	switch {
	case math.IsNaN(c), c < 0:
		return 0
	case c > 1:
		return 1
	}
	return c
}

// MinWordP returns the lowest word probability in the segment, or zero when no
// word data is present. A single very low word is worth surfacing even when the
// segment average looks healthy.
func (s Segment) MinWordP() float64 {
	if len(s.Words) == 0 {
		return 0
	}
	min := s.Words[0].Probability
	for _, w := range s.Words[1:] {
		if w.Probability < min {
			min = w.Probability
		}
	}
	return min
}

// MeanWordP returns the arithmetic mean of word probabilities, or zero when no
// word data is present. Kept for calibration and display only; Confidence is
// the metric that decides anything.
func (s Segment) MeanWordP() float64 {
	if len(s.Words) == 0 {
		return 0
	}
	var sum float64
	for _, w := range s.Words {
		sum += w.Probability
	}
	return sum / float64(len(s.Words))
}

// Client talks to a running whisper-server.
type Client struct {
	BaseURL string
	HTTP    *http.Client

	// Language pins the decode language. Auto-detection runs on the first
	// 30-second window and is close to a coin flip on a short Levantine note,
	// and a wrong guess produces confident garbage that no confidence score
	// will flag.
	Language string
}

// NewClient returns a Client for a whisper-server at baseURL.
func NewClient(baseURL string) *Client {
	return &Client{
		BaseURL:  baseURL,
		Language: "ar",
		HTTP:     &http.Client{},
	}
}

// wireResponse mirrors whisper-server's verbose_json body.
type wireResponse struct {
	Language string  `json:"language"`
	Duration float64 `json:"duration"`
	Text     string  `json:"text"`
	Segments []struct {
		Start        float64 `json:"start"`
		End          float64 `json:"end"`
		Text         string  `json:"text"`
		AvgLogprob   float64 `json:"avg_logprob"`
		NoSpeechProb float64 `json:"no_speech_prob"`
		Temperature  float64 `json:"temperature"`
		Words        []struct {
			Word        string  `json:"word"`
			Start       float64 `json:"start"`
			End         float64 `json:"end"`
			Probability float64 `json:"probability"`
		} `json:"words"`
	} `json:"segments"`
}

// Timeout returns the transcription deadline for a note of the given duration.
//
// Whisper pads every input to a 30-second analysis window, so a short note costs
// roughly as much as a long one and the floor matters more than the multiple.
// The multiple exists to catch the repetition-loop failure mode on degenerate
// audio, which can otherwise run far past real time and stall the queue.
func Timeout(durationMS int64) time.Duration {
	const floor = 90 * time.Second
	scaled := time.Duration(durationMS) * time.Millisecond * 10
	if scaled < floor {
		return floor
	}
	return scaled
}

// Transcribe sends a 16 kHz mono WAV to whisper-server and returns the parsed
// result.
func (c *Client) Transcribe(ctx context.Context, wavPath string) (Result, error) {
	f, err := os.Open(wavPath)
	if err != nil {
		return Result{}, fmt.Errorf("open audio: %w", err)
	}
	defer f.Close()

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)

	part, err := mw.CreateFormFile("file", filepath.Base(wavPath))
	if err != nil {
		return Result{}, fmt.Errorf("build request: %w", err)
	}
	if _, err := io.Copy(part, f); err != nil {
		return Result{}, fmt.Errorf("build request: %w", err)
	}

	fields := map[string]string{
		"response_format": "verbose_json",
		"temperature":     "0.0",
	}
	if c.Language != "" {
		fields["language"] = c.Language
	}
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			return Result{}, fmt.Errorf("build request: %w", err)
		}
	}
	if err := mw.Close(); err != nil {
		return Result{}, fmt.Errorf("build request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/inference", &body)
	if err != nil {
		return Result{}, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}

	resp, err := client.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("whisper-server unreachable at %s: %w", c.BaseURL, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return Result{}, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return Result{}, fmt.Errorf("whisper-server returned %s: %s",
			resp.Status, truncate(string(raw), 200))
	}

	return Parse(raw)
}

// Parse converts a verbose_json body into a Result. Exported so the parsing
// contract can be tested against a committed fixture without a running server.
func Parse(raw []byte) (Result, error) {
	var wire wireResponse
	if err := json.Unmarshal(raw, &wire); err != nil {
		return Result{}, fmt.Errorf("parse whisper response: %w", err)
	}

	res := Result{Language: wire.Language, Duration: wire.Duration}
	for _, ws := range wire.Segments {
		seg := Segment{
			Start:        ws.Start,
			End:          ws.End,
			Text:         ws.Text,
			AvgLogprob:   ws.AvgLogprob,
			NoSpeechProb: ws.NoSpeechProb,
			Temperature:  ws.Temperature,
		}
		for _, ww := range ws.Words {
			seg.Words = append(seg.Words, Word{
				Word:        ww.Word,
				Start:       ww.Start,
				End:         ww.End,
				Probability: ww.Probability,
			})
		}
		res.Segments = append(res.Segments, seg)
	}
	return res, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
