package errclass

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
)

// codeStatus maps MiniMax base_resp.status_code values to the HTTP status
// that drives CPA's credential cooldown: 401/403 cool the credential down,
// 402 marks billing, 429 enters quota backoff, and 5xx retries elsewhere.
// https://platform.minimax.io/docs/api-reference/errorcode
var codeStatus = map[int]struct {
	status int
	class  Class
}{
	// Auth: invalid key, or a key used against the other region.
	1004: {401, ClassAuth},
	2049: {401, ClassAuth},
	// Pay-as-you-go balance exhausted.
	1008: {402, ClassBilling},
	// Subscription usage exhausted for the current window or credits.
	2053: {429, ClassQuota},
	2056: {429, ClassQuota},
	2067: {429, ClassQuota},
	// Rate shaping, including dynamic M Plan throttling.
	1002: {429, ClassRateLimit},
	1041: {429, ClassRateLimit},
	1045: {429, ClassRateLimit},
	2045: {429, ClassRateLimit},
	2046: {429, ClassRateLimit},
	2047: {429, ClassRateLimit},
	2062: {429, ClassRateLimit},
	// The key or plan cannot use this endpoint or model.
	2057: {403, ClassAuth},
	2061: {403, ClassAuth},
	2068: {403, ClassAuth},
	// Request and model errors are not credential problems.
	2013: {400, ClassUnsupported},
	2022: {404, ClassInvalidModel},
	2069: {404, ClassInvalidModel},
	// Transient upstream failures.
	1000: {503, ClassUpstream},
	1001: {503, ClassUpstream},
	1024: {503, ClassUpstream},
	1033: {503, ClassUpstream},
	1039: {503, ClassUpstream},
	2064: {503, ClassUpstream},
	2066: {503, ClassUpstream},
}

// messageCodeRe finds a parenthesized MiniMax code such as
// "insufficient balance (1008)" in compatible-endpoint error messages.
var messageCodeRe = regexp.MustCompile(`\((\d{4})\)`)

// statusCodeRe finds a status_code echoed inside an error message.
var statusCodeRe = regexp.MustCompile(`status_code["']?\s*[:=]\s*(\d{4})`)

type baseResp struct {
	StatusCode int    `json:"status_code"`
	StatusMsg  string `json:"status_msg"`
}

// MiniMaxCode extracts a known MiniMax error code from an error body:
// base_resp.status_code first, then a code embedded in the message. The
// compatible endpoints document no code-to-HTTP mapping, and the same code has
// been observed under different HTTP statuses, so the code wins.
func MiniMaxCode(body []byte, message string) int {
	var probe struct {
		BaseResp *baseResp `json:"base_resp"`
	}
	if json.Unmarshal(body, &probe) == nil && probe.BaseResp != nil {
		if _, ok := codeStatus[probe.BaseResp.StatusCode]; ok {
			return probe.BaseResp.StatusCode
		}
	}
	for _, re := range []*regexp.Regexp{messageCodeRe, statusCodeRe} {
		for _, m := range re.FindAllStringSubmatch(message, -1) {
			code, _ := strconv.Atoi(m[1])
			if _, ok := codeStatus[code]; ok {
				return code
			}
		}
	}
	return 0
}

// FromMiniMaxCode classifies a known MiniMax code. ok is false for unknown codes.
func FromMiniMaxCode(code int, message string) (*Error, bool) {
	mapped, ok := codeStatus[code]
	if !ok {
		return nil, false
	}
	retryable := mapped.class != ClassUnsupported && mapped.class != ClassInvalidModel
	return &Error{Class: mapped.class, Message: Redact(message), StatusCode: mapped.status, Retryable: retryable}, true
}

// BaseRespError reports a successful-status body that carries a MiniMax
// failure in base_resp, as the native and quota endpoints do. It returns nil
// when the body has no base_resp or reports success.
func BaseRespError(body []byte) *Error {
	var probe struct {
		BaseResp *baseResp `json:"base_resp"`
	}
	if json.Unmarshal(body, &probe) != nil || probe.BaseResp == nil || probe.BaseResp.StatusCode == 0 {
		return nil
	}
	msg := RedactedSnippet(fmt.Sprintf("%s (%d)", probe.BaseResp.StatusMsg, probe.BaseResp.StatusCode))
	if e, ok := FromMiniMaxCode(probe.BaseResp.StatusCode, msg); ok {
		return e
	}
	return &Error{Class: ClassUpstream, Message: msg, StatusCode: 502, Retryable: true}
}
