package extractor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNormalizeCategory(t *testing.T) {
	cases := map[string]string{
		"user":                       "user",
		"USER":                       "user",
		"  project ":                 "project",
		"feedback.":                  "feedback",
		"\"reference\"":              "reference",
		"none":                       "none",
		"none.":                      "none",
		"hobbies":                    "none", // not in taxonomy
		"":                           "none",
		"user is the right category": "user", // takes first word
		"random gibberish":           "none",
		"projects":                   "none", // close-but-no-cigar
	}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			got := NormalizeCategory(in)
			if got != want {
				t.Errorf("NormalizeCategory(%q): got %q want %q", in, got, want)
			}
		})
	}
}

func TestOpenAIClassifier_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify auth + endpoint
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization header: got %q", got)
		}
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			t.Errorf("path: got %q", r.URL.Path)
		}
		// Echo back a fake chat response with our chosen category
		_ = json.NewEncoder(w).Encode(chatResponse{
			Choices: []struct {
				Message chatMessage `json:"message"`
			}{
				{Message: chatMessage{Role: "assistant", Content: "feedback"}},
			},
		})
	}))
	defer srv.Close()

	c := NewOpenAIClassifier("test-key", "gpt-4o-mini", srv.URL)
	got, err := c.Classify(context.Background(), "Don't break this — bug in production once already.")
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if got != "feedback" {
		t.Errorf("category: got %q want feedback", got)
	}
}

func TestOpenAIClassifier_NoiseyOutputCollapsesToNone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(chatResponse{
			Choices: []struct {
				Message chatMessage `json:"message"`
			}{
				{Message: chatMessage{Role: "assistant", Content: "I'm not sure"}},
			},
		})
	}))
	defer srv.Close()

	c := NewOpenAIClassifier("k", "", srv.URL)
	got, err := c.Classify(context.Background(), "ambiguous content")
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if got != "none" {
		t.Errorf("expected fallback to none, got %q", got)
	}
}

func TestOpenAIClassifier_HTTPErrorReturnsErr(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := NewOpenAIClassifier("bad-key", "", srv.URL)
	_, err := c.Classify(context.Background(), "anything")
	if err == nil {
		t.Fatalf("expected error on 401, got nil")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error should mention 401: %v", err)
	}
}

func TestOpenAIClassifier_TruncatesLargeInput(t *testing.T) {
	// Verify that input >6000 chars is truncated before hitting the API.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body chatRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		// classifyPrompt has a fixed prefix (~600 chars); the rest is the chunk.
		// Total content ≤ classifyPrompt+6000.
		if len(body.Messages[0].Content) > len(classifyPrompt)+6000+8 {
			t.Errorf("input not truncated: %d chars", len(body.Messages[0].Content))
		}
		_ = json.NewEncoder(w).Encode(chatResponse{
			Choices: []struct {
				Message chatMessage `json:"message"`
			}{{Message: chatMessage{Role: "assistant", Content: "project"}}},
		})
	}))
	defer srv.Close()

	c := NewOpenAIClassifier("k", "", srv.URL)
	huge := strings.Repeat("a", 100000)
	got, err := c.Classify(context.Background(), huge)
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if got != "project" {
		t.Errorf("unexpected: %q", got)
	}
}

func TestOpenAIClassifier_NoAPIKeyReturnsError(t *testing.T) {
	c := NewOpenAIClassifier("", "", "http://nowhere")
	_, err := c.Classify(context.Background(), "x")
	if err == nil {
		t.Fatalf("expected error when api key is empty")
	}
}
