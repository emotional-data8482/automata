package claude

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/emotional-data8482/automata/retry"
)

func TestAPIError_Retryable(t *testing.T) {
	cases := []struct {
		code int
		want bool
	}{
		// Retryable: timeout, rate limit, overloaded, any 5xx.
		{408, true},
		{429, true},
		{500, true},
		{502, true},
		{503, true},
		{529, true},
		{599, true},
		// Not retryable: client errors that won't change on retry.
		{400, false},
		{401, false},
		{403, false},
		{404, false},
		{409, false}, // semantic conflict — explicit
		{422, false},
		// Not retryable: success codes.
		{200, false},
		{201, false},
	}
	for _, c := range cases {
		got := (&APIError{StatusCode: c.code}).Retryable()
		if got != c.want {
			t.Errorf("StatusCode=%d: Retryable() = %v, want %v", c.code, got, c.want)
		}
	}
}

func TestWrapAPIError_SDKError(t *testing.T) {
	sdkErr := &anthropic.Error{
		StatusCode: 429,
		Request:    httptest.NewRequest("POST", "/v1/messages", nil),
		Response:   &http.Response{StatusCode: 429},
	}

	wrapped := wrapAPIError(sdkErr)

	var apiErr *APIError
	if !errors.As(wrapped, &apiErr) {
		t.Fatalf("expected *APIError, got %T", wrapped)
	}
	if apiErr.StatusCode != 429 {
		t.Errorf("StatusCode = %d, want 429", apiErr.StatusCode)
	}
}

func TestWrapAPIError_NonSDKError_PassesThrough(t *testing.T) {
	plain := errors.New("network unreachable")
	wrapped := wrapAPIError(plain)
	if wrapped != plain {
		t.Errorf("expected non-SDK error to pass through unchanged, got %v", wrapped)
	}
}

func TestWrapAPIError_Nil(t *testing.T) {
	if wrapAPIError(nil) != nil {
		t.Error("wrapAPIError(nil) should return nil")
	}
}

func TestWrapAPIError_SatisfiesRetryable(t *testing.T) {
	// The whole point: after wrapping, the retry layer's errors.As probe for
	// retry.Retryable must succeed and report Retryable() == true for 429.
	sdkErr := &anthropic.Error{
		StatusCode: 429,
		Request:    httptest.NewRequest("POST", "/v1/messages", nil),
		Response:   &http.Response{StatusCode: 429},
	}

	// Mirror the wrapping in anthropic.go Invoke.
	wrapped := fmt.Errorf("anthropic invoke: %w", wrapAPIError(sdkErr))

	var r retry.Retryable
	if !errors.As(wrapped, &r) {
		t.Fatal("expected wrapped error to satisfy retry.Retryable")
	}
	if !r.Retryable() {
		t.Error("expected Retryable() == true for 429")
	}
}
