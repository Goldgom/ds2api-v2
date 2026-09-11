package client

import (
	"bytes"
	"context"
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

func TestCallCompletionDoesNotRetryFlashModelTypeOnSchemaRejection(t *testing.T) {
	calls := 0
	client := &Client{
		stream: doerFunc(func(*http.Request) (*http.Response, error) {
			calls++
			return &http.Response{
				StatusCode: http.StatusUnprocessableEntity,
				Body: io.NopCloser(bytes.NewBufferString(
					"Failed to deserialize model_type: unknown variant `deepseek-flash`, expected `default`",
				)),
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
	t.Cleanup(func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("close response body: %v", err)
		}
	})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d want=%d", resp.StatusCode, http.StatusUnprocessableEntity)
	}
	if calls != 1 {
		t.Fatalf("completion calls=%d want=1; completion requests must not be replayed", calls)
	}
}
