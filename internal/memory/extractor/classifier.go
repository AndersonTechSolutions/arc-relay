package extractor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ValidCategories is the closed taxonomy the classifier picks from. Mirrors
// the user's auto-memory taxonomy (user/feedback/project/reference) so /recall
// memories blend cleanly with hand-saved memories from any other source.
//
// "none" is the escape hatch when a chunk contains mixed content or pure
// narrative with no clear category — keeps the field stable instead of
// hallucinating a label.
var ValidCategories = map[string]bool{
	"user":      true,
	"feedback":  true,
	"project":   true,
	"reference": true,
	"none":      true,
}

// classifyPrompt is the single-turn prompt sent to gpt-4o-mini. Wording
// matches the auto-memory definitions so a chunk gets the same label a
// human writer would pick.
const classifyPrompt = `Classify the following Claude Code transcript chunk into ONE category.

Categories:
- user: facts about the user's role, goals, expertise, or preferences
- feedback: corrections or guidance the user has given to AI assistants ("don't X", "always Y")
- project: project decisions, technical choices, architecture rationale, work-in-progress facts
- reference: pointers to external resources (URLs, file paths, system names, dashboards, vault items)
- none: mixed content, pure conversational narrative, or no clear single category

Respond with ONLY the category name, lowercase, no punctuation, no explanation.

CHUNK:
%s`

// Classifier picks one of the ValidCategories for a chunk of text. Returns
// "none" on ambiguous content. Returns an error only on transport-level
// failures (network, auth) — caller should treat error as "skip the
// classification, send no category metadata".
type Classifier interface {
	Classify(ctx context.Context, chunkText string) (string, error)
}

// OpenAIClassifier hits any OpenAI-compatible chat/completions endpoint.
// Defaults to https://api.openai.com/v1 with gpt-4o-mini.
type OpenAIClassifier struct {
	apiKey  string
	model   string
	baseURL string
	client  *http.Client
}

// NewOpenAIClassifier builds a classifier. apiKey is required; model and
// baseURL fall back to gpt-4o-mini and OpenAI's public endpoint when empty.
func NewOpenAIClassifier(apiKey, model, baseURL string) *OpenAIClassifier {
	if model == "" {
		model = "gpt-4o-mini"
	}
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	return &OpenAIClassifier{
		apiKey:  apiKey,
		model:   model,
		baseURL: baseURL,
		client:  &http.Client{Timeout: 30 * time.Second},
	}
}

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	MaxTokens   int           `json:"max_tokens"`
	Temperature float64       `json:"temperature"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
}

// Classify runs one chat completion and parses the result down to a single
// valid category token. Anything outside ValidCategories collapses to "none"
// — the goal is to never poison metadata with a hallucinated label.
//
// chunkText is truncated to 6000 chars before sending so a pathologically
// large message can't blow up the classifier's input budget. The classifier
// only needs the gist; full text isn't necessary for taxonomy assignment.
func (c *OpenAIClassifier) Classify(ctx context.Context, chunkText string) (string, error) {
	if c.apiKey == "" {
		return "", fmt.Errorf("classifier: api key not configured")
	}

	if len(chunkText) > 6000 {
		chunkText = chunkText[:6000]
	}

	reqBody, err := json.Marshal(chatRequest{
		Model: c.model,
		Messages: []chatMessage{
			{Role: "user", Content: fmt.Sprintf(classifyPrompt, chunkText)},
		},
		MaxTokens:   10,
		Temperature: 0,
	})
	if err != nil {
		return "", fmt.Errorf("classifier: marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/chat/completions", bytes.NewReader(reqBody))
	if err != nil {
		return "", fmt.Errorf("classifier: new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("classifier: send: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("classifier: %d %s: %s",
			resp.StatusCode, resp.Status, strings.TrimSpace(string(body)))
	}

	var out chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("classifier: decode: %w", err)
	}
	if len(out.Choices) == 0 {
		return "none", nil
	}

	return NormalizeCategory(out.Choices[0].Message.Content), nil
}

// NormalizeCategory takes a raw model output and snaps it to the closed
// ValidCategories set. Tolerates trailing punctuation, surrounding quotes,
// stray prose. Anything unrecognized becomes "none".
func NormalizeCategory(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	// Strip wrapping quotes and trailing punctuation
	s = strings.Trim(s, `"'.,;:! `)
	// If the model returned a sentence, take the first word.
	if i := strings.IndexAny(s, " \t\n"); i > 0 {
		s = s[:i]
	}
	if ValidCategories[s] {
		return s
	}
	return "none"
}
