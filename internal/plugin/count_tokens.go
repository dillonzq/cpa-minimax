package plugin

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/dillonzq/cpa-minimax/internal/catalog"
	"github.com/dillonzq/cpa-minimax/internal/errclass"
)

// countTokensFields are the Messages fields the counter accepts; generation
// controls such as max_tokens and stream are dropped.
var countTokensFields = []string{"model", "messages", "system", "tools", "tool_choice", "thinking"}

// handleCountTokens implements executor.count_tokens for Messages clients on
// models served through Messages. The upstream {"input_tokens":N} body is
// already in the client's format. Other formats have no MiniMax counter.
func (m *Manager) handleCountTokens(request []byte) ([]byte, error) {
	var req executorRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return ErrEnvelope("invalid_request", "malformed executor request body"), nil
	}
	res, failEnv := m.resolveExecution(req)
	if res == nil {
		return failEnv, nil
	}
	target, ok := res.rec.Target(catalog.RouteMessages)
	if req.inputFormat() != "claude" || req.outputFormat() != "claude" || !ok {
		return classEnvelope(&errclass.Error{Class: errclass.ClassUnsupported, Message: "token counting requires a Claude-format request on a Messages model"}), nil
	}
	body, eErr := buildUpstreamRequest(catalog.RouteMessages, res.rec.UpstreamID, "claude", req.effectivePayload(), res.state)
	if eErr != nil {
		return classEnvelope(eErr), nil
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(body, &doc); err != nil || doc == nil {
		return classEnvelope(errclass.Translation("malformed count_tokens request")), nil
	}
	kept := make(map[string]json.RawMessage, len(countTokensFields))
	for _, field := range countTokensFields {
		if raw, ok := doc[field]; ok {
			kept[field] = raw
		}
	}
	body, _ = json.Marshal(kept)
	ctx, cancel := context.WithTimeout(withHostCallbackScope(m.workContext(), req.HostCallbackID), res.cfg.RequestTimeout)
	defer cancel()
	// The counter sits below the Messages endpoint:
	// /anthropic/v1/messages/count_tokens by default.
	counter := catalog.Target{Protocol: catalog.RouteMessages, EndpointPath: target.EndpointPath + "/count_tokens"}
	resp, err := m.bridge.Do(ctx, pluginapi.HTTPRequest{
		Method:  http.MethodPost,
		URL:     counter.URL(res.baseURL),
		Headers: upstreamAuthHeaders(catalog.RouteMessages, res.key),
		Body:    body,
	})
	if err != nil {
		return classEnvelope(errclass.FromNetwork(err)), nil
	}
	if resp.StatusCode >= 400 {
		return classEnvelope(errclass.UpstreamStatusError(resp.StatusCode, resp.Body)), nil
	}
	if eErr := errclass.BaseRespError(resp.Body); eErr != nil {
		return classEnvelope(eErr), nil
	}
	var counted struct {
		InputTokens *int64 `json:"input_tokens"`
	}
	if json.Unmarshal(resp.Body, &counted) != nil || counted.InputTokens == nil {
		return classEnvelope(errclass.UpstreamFallback("count_tokens response has no input_tokens")), nil
	}
	out, _ := json.Marshal(map[string]int64{"input_tokens": *counted.InputTokens})
	return okEnvelope(pluginapi.ExecutorResponse{Payload: out}), nil
}
