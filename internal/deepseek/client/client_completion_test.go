package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"

	"ds2api/internal/auth"
)

func TestCallCompletionDoesNotFallbackForNonIdempotentCompletion(t *testing.T) {
	var fallbackCalled bool
	client := &Client{
		stream: doerFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("ambiguous completion write failure")
		}),
		fallbackS: &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
			fallbackCalled = true
			return &http.Response{StatusCode: http.StatusOK}, nil
		})},
	}
	_, err := client.CallCompletion(
		context.Background(),
		&auth.RequestAuth{DeepSeekToken: "token"},
		map[string]any{"prompt": "hello"},
		"pow",
		3,
	)
	if err == nil {
		t.Fatal("expected completion error")
	}
	if fallbackCalled {
		t.Fatal("completion fallback should not be called for a non-idempotent request")
	}
}

func TestCallCompletionFallsBackToLegacyFlashModelTypeOnSchemaRejection(t *testing.T) {
	var seenModelTypes []string
	client := &Client{
		stream: doerFunc(func(req *http.Request) (*http.Response, error) {
			var payload map[string]any
			if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
				t.Fatalf("decode completion payload: %v", err)
			}
			seenModelTypes = append(seenModelTypes, payload["model_type"].(string))
			if len(seenModelTypes) == 1 {
				return &http.Response{
					StatusCode: http.StatusUnprocessableEntity,
					Body: io.NopCloser(bytes.NewBufferString(
						"Failed to deserialize model_type: unknown variant `deepseek-flash`, expected `default`",
					)),
				}, nil
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(bytes.NewBufferString("data: [DONE]\n\n")),
			}, nil
		}),
	}

	resp, err := client.CallCompletion(
		context.Background(),
		&auth.RequestAuth{DeepSeekToken: "token", AccountID: "acct"},
		map[string]any{"prompt": "hello", "model_type": "deepseek-flash"},
		"pow",
		1,
	)
	if err != nil {
		t.Fatalf("CallCompletion returned error: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want=%d", resp.StatusCode, http.StatusOK)
	}
	if len(seenModelTypes) != 2 || seenModelTypes[0] != "deepseek-flash" || seenModelTypes[1] != "default" {
		t.Fatalf("unexpected model_type attempts: %#v", seenModelTypes)
	}
}

func TestCallCompletionDoesNotFallbackForUnrelatedValidationError(t *testing.T) {
	calls := 0
	client := &Client{
		stream: doerFunc(func(*http.Request) (*http.Response, error) {
			calls++
			return &http.Response{
				StatusCode: http.StatusUnprocessableEntity,
				Body:       io.NopCloser(bytes.NewBufferString("invalid prompt")),
			}, nil
		}),
	}

	resp, err := client.CallCompletion(
		context.Background(),
		&auth.RequestAuth{DeepSeekToken: "token"},
		map[string]any{"prompt": "hello", "model_type": "deepseek-flash"},
		"pow",
		1,
	)
	if err != nil {
		t.Fatalf("CallCompletion returned error: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if calls != 1 {
		t.Fatalf("completion calls=%d want=1", calls)
	}
}
