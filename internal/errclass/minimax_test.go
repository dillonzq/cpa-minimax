package errclass

import (
	"strings"
	"testing"
)

func TestUpstreamStatusErrorMiniMaxCodes(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		body      string
		class     Class
		http      int
		retryable bool
	}{
		{"auth 1004", 401, `{"base_resp":{"status_code":1004,"status_msg":"login fail"}}`, ClassAuth, 401, true},
		{"auth 2049 under 400", 400, `{"base_resp":{"status_code":2049,"status_msg":"invalid api key"}}`, ClassAuth, 401, true},
		{"balance 1008 under 500", 500, `{"type":"error","error":{"type":"api_error","message":"insufficient balance (1008)"}}`, ClassBilling, 402, true},
		{"balance 1008 under 400", 400, `{"base_resp":{"status_code":1008,"status_msg":"insufficient balance"}}`, ClassBilling, 402, true},
		{"plan usage 2056", 429, `{"type":"error","error":{"type":"rate_limit_error","message":"usage limit exceeded (2056)"}}`, ClassQuota, 429, true},
		{"rate limit 1002", 200, `{"base_resp":{"status_code":1002,"status_msg":"rate limit"}}`, ClassRateLimit, 429, true},
		{"plan throttle 2062 in status_code text", 500, `{"error":{"message":"base_resp status_code: 2062, too many requests"}}`, ClassRateLimit, 429, true},
		{"endpoint needs plan key 2057", 400, `{"base_resp":{"status_code":2057,"status_msg":"x"}}`, ClassAuth, 403, true},
		{"bad params 2013", 500, `{"base_resp":{"status_code":2013,"status_msg":"invalid params"}}`, ClassUnsupported, 400, false},
		{"model retired 2069", 400, `{"base_resp":{"status_code":2069,"status_msg":"model retired"}}`, ClassInvalidModel, 404, false},
		{"no access 2022", 400, `{"error":{"message":"model not found (2022)"}}`, ClassInvalidModel, 404, false},
		{"transient 1000", 400, `{"base_resp":{"status_code":1000,"status_msg":"unknown error"}}`, ClassUpstream, 503, true},
		{"overloaded 2064", 500, `{"error":{"message":"server overloaded (2064)"}}`, ClassUpstream, 503, true},
		{"anthropic overload 529", 529, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`, ClassUpstream, 503, true},
		{"unknown code uses status", 401, `{"base_resp":{"status_code":9999,"status_msg":"strange"}}`, ClassAuth, 401, true},
		{"unknown parenthesized number uses status", 400, `{"error":{"message":"max_tokens (4096) too large"}}`, ClassUnsupported, 400, false},
		{"plain 429 without code", 429, `{"error":{"message":"slow down"}}`, ClassRateLimit, 429, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := UpstreamStatusError(c.status, []byte(c.body))
			if e.Class != c.class || e.StatusCode != c.http || e.Retryable != c.retryable {
				t.Fatalf("got class=%s status=%d retryable=%v (%q), want %s/%d/%v", e.Class, e.StatusCode, e.Retryable, e.Message, c.class, c.http, c.retryable)
			}
			wire := ToEnvelopeError(e)
			if wire.HTTPStatus != c.http || wire.Code != string(c.class) {
				t.Fatalf("envelope = %+v", wire)
			}
		})
	}
}

func TestUpstreamStatusErrorBaseRespMessage(t *testing.T) {
	e := UpstreamStatusError(400, []byte(`{"base_resp":{"status_code":1008,"status_msg":"insufficient balance"}}`))
	if e.Message != "insufficient balance (1008)" {
		t.Fatalf("message = %q", e.Message)
	}
	// A key echoed in an upstream message is redacted before classification output.
	e = UpstreamStatusError(401, []byte(`{"error":{"message":"invalid key Bearer sk-cp-secret123 (1004)"}}`))
	if strings.Contains(e.Message, "sk-cp-secret123") || e.Class != ClassAuth {
		t.Fatalf("message = %q class=%s", e.Message, e.Class)
	}
}

func TestMiniMaxCode(t *testing.T) {
	cases := []struct {
		body, msg string
		want      int
	}{
		{`{"base_resp":{"status_code":2056}}`, "", 2056},
		{`{"base_resp":{"status_code":0}}`, "", 0},
		{`{"base_resp":{"status_code":1234}}`, "quota (1008)", 1008}, // unknown base_resp code falls through to the message
		{`not json`, "insufficient balance (1008)", 1008},
		{``, `status_code": 1002`, 1002},
		{``, "status_code=2062", 2062},
		{``, "took (1500) ms then (2056)", 2056},
		{``, "1008 without parentheses", 0},
		{``, "(12345)", 0},
	}
	for _, c := range cases {
		if got := MiniMaxCode([]byte(c.body), c.msg); got != c.want {
			t.Errorf("MiniMaxCode(%q, %q) = %d, want %d", c.body, c.msg, got, c.want)
		}
	}
}

func TestFromMiniMaxCodeUnknown(t *testing.T) {
	if e, ok := FromMiniMaxCode(0, "x"); ok || e != nil {
		t.Fatalf("code 0 classified: %+v", e)
	}
	if e, ok := FromMiniMaxCode(4242, "x"); ok || e != nil {
		t.Fatalf("unknown code classified: %+v", e)
	}
	e, ok := FromMiniMaxCode(1004, "Bearer sk-secret-123")
	if !ok || strings.Contains(e.Message, "sk-secret-123") {
		t.Fatalf("message not redacted: %+v", e)
	}
}

func TestBaseRespError(t *testing.T) {
	for _, body := range []string{
		`{"id":"msg_1","type":"message","content":[]}`,
		`{"base_resp":{"status_code":0,"status_msg":"success"}}`,
		`{"choices":[],"base_resp":{"status_code":0}}`,
		`not json`,
		``,
	} {
		if e := BaseRespError([]byte(body)); e != nil {
			t.Errorf("BaseRespError(%q) = %+v, want nil", body, e)
		}
	}
	e := BaseRespError([]byte(`{"choices":null,"base_resp":{"status_code":1008,"status_msg":"insufficient balance"}}`))
	if e == nil || e.Class != ClassBilling || e.StatusCode != 402 || !e.Retryable || e.Message != "insufficient balance (1008)" {
		t.Fatalf("1008 = %+v", e)
	}
	e = BaseRespError([]byte(`{"base_resp":{"status_code":2056,"status_msg":"usage limit exceeded"}}`))
	if e == nil || e.Class != ClassQuota || e.StatusCode != 429 {
		t.Fatalf("2056 = %+v", e)
	}
	e = BaseRespError([]byte(`{"base_resp":{"status_code":7777,"status_msg":"mystery"}}`))
	if e == nil || e.Class != ClassUpstream || e.StatusCode != 502 || !e.Retryable || e.Message != "mystery (7777)" {
		t.Fatalf("unknown = %+v", e)
	}
}

// Every mapped code yields a status CPA's cooldown understands.
func TestCodeStatusTableShape(t *testing.T) {
	allowed := map[int]bool{400: true, 401: true, 402: true, 403: true, 404: true, 429: true, 503: true}
	for code, mapped := range codeStatus {
		if code < 1000 || code > 2999 || !allowed[mapped.status] || mapped.class == "" {
			t.Errorf("code %d -> %+v", code, mapped)
		}
	}
}
