package builtin

// Characterization test for WeatherSkill's retry behavior — written BEFORE the
// hand-rolled loop is refactored to toolkit/util/retry, so it locks the
// observable contract (attempt count + each failure-reason message) and proves
// the refactor preserves behavior.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type weatherRT func(*http.Request) (*http.Response, error)

func (f weatherRT) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func weatherResp(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}

func TestWeatherSkill_RetryBehavior(t *testing.T) {
	cases := []struct {
		name         string
		fn           func(*http.Request) (*http.Response, error)
		wantCalls    int
		wantContains string
	}{
		{"network error retries then gives up", func(*http.Request) (*http.Response, error) { return nil, errors.New("refused") }, 2, "网络连接失败"},
		{"non-200 retries then gives up", func(*http.Request) (*http.Response, error) { return weatherResp(503, "down"), nil }, 2, "天气服务暂时不可用"},
		{"non-json body retries then gives up", func(*http.Request) (*http.Response, error) { return weatherResp(200, "not json"), nil }, 2, "非预期格式"},
		{"valid-format body exits after one call", func(*http.Request) (*http.Response, error) { return weatherResp(200, "{bad json"), nil }, 1, "天气数据解析异常"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			calls := 0
			s := &WeatherSkill{client: &http.Client{Transport: weatherRT(func(r *http.Request) (*http.Response, error) {
				calls++
				return c.fn(r)
			})}}
			res, err := s.Execute(context.Background(), map[string]any{"location": "北京"})
			if err != nil {
				t.Fatalf("Execute returned a Go error: %v", err)
			}
			if calls != c.wantCalls {
				t.Errorf("HTTP attempts = %d, want %d", calls, c.wantCalls)
			}
			if !strings.Contains(res.Content, c.wantContains) {
				t.Errorf("result %q must contain %q", res.Content, c.wantContains)
			}
		})
	}
}
