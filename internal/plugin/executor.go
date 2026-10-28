// Non-stream and stream execution paths: resolve the public model ID against
// the catalog snapshot, translate the inbound payload to the record's
// upstream protocol, call the credential's regional MiniMax host through the
// host HTTP callbacks, and translate back to the client protocol. Request
// authentication, rotation, and cooldown are selected by CPA.

package plugin

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/dillonzq/apix/reasoning"
	"github.com/dillonzq/cpa-minimax/internal/catalog"
	"github.com/dillonzq/cpa-minimax/internal/config"
	"github.com/dillonzq/cpa-minimax/internal/errclass"
	"github.com/dillonzq/cpa-minimax/internal/protocol"
)

// executorRequest mirrors rpcExecutorRequest: the SDK embeds
// pluginapi.ExecutorRequest untagged, so its fields marshal under Go field
// names ("Model", "SourceFormat", "OriginalRequest", "Stream"). StreamID is
// the DOWNSTREAM host-allocated id for executor.execute_stream emissions;
// ids returned by DoStream are UPSTREAM and never interchangeable (§4).
type executorRequest struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// resolvedExecution carries everything both execution paths need after
// model/key resolution succeeded. route is the upstream selected for the
// request's input format. suffix is the requested model's thinking suffix: a
// zero value means the model ID carried none, or that a literal parenthesized
// catalog ID won routing and keeps its own identity.
type resolvedExecution struct {
	cfg     config.Config
	rec     catalog.ModelRecord
	route   catalog.Target
	key     string
	baseURL string
	suffix  reasoning.Suffix
	state   *protocol.State
}

// resolveExecution resolves the requested model against the snapshot and
// extracts the key selected by CPA. A non-nil second return is a ready-made
// failure envelope.
func (m *Manager) resolveExecution(req executorRequest) (*resolvedExecution, []byte) {
	m.mu.RLock()
	cfg, mgr := m.cfg, m.mgr
	m.mu.RUnlock()
	if req.AuthProvider != ProviderID {
		return nil, classEnvelope(&errclass.Error{Class: errclass.ClassAuth, Message: "selected auth provider is not minimax"})
	}
	key := strings.TrimSpace(req.AuthAttributes["api_key"])
	debugTrace("executor auth model=%s auth_id=%s provider=%s attr_api_key_present=%t attr_count=%d storage_json_bytes=%d", req.Model, req.AuthID, req.AuthProvider, key != "", len(req.AuthAttributes), len(req.StorageJSON))
	if key == "" {
		return nil, classEnvelope(&errclass.Error{Class: errclass.ClassAuth, Message: "selected auth has no api key"})
	}
	// CPA parses a model-name thinking suffix (model(high)) for its own
	// executors but hands the raw ID to plugins, so resolve the base model and
	// carry the suffix to the route override. A catalog ID that literally
	// carries parentheses still resolves when the base does not, and then keeps
	// its own identity with no suffix override.
	suffix := reasoning.ParseSuffix(req.Model)
	lookupID := req.Model
	if suffix.HasSuffix && suffix.Model != "" {
		lookupID = suffix.Model
	}
	var rec catalog.ModelRecord
	var found bool
	if mgr != nil && lookupID != "" {
		rec, found = mgr.Lookup(lookupID)
	}
	if !found && lookupID != req.Model && mgr != nil {
		if rec, found = mgr.Lookup(req.Model); found {
			suffix = reasoning.Suffix{}
		}
	}
	if !found {
		return nil, classEnvelope(&errclass.Error{
			Class:      errclass.ClassInvalidModel,
			Message:    "model not in routable catalog",
			StatusCode: http.StatusNotFound,
		})
	}
	route := selectRoute(rec, req.inputFormat())
	debugTrace("executor resolved model lookup=%s suffix_raw=%q suffix_effort=%q route=%s", lookupID, suffix.Raw, suffix.Effort, route.Protocol)
	entry, err := resolveCredential(cfg, key, req.AuthAttributes["region"], req.AuthAttributes["key_type"], req.AuthAttributes["base_url"])
	if err != nil {
		return nil, classEnvelope(&errclass.Error{Class: errclass.ClassAuth, Message: err.Error()})
	}
	baseURL := cfg.UpstreamBaseURL(entry)
	return &resolvedExecution{cfg: cfg, rec: rec, route: route, key: key, baseURL: baseURL, suffix: suffix, state: &protocol.State{Output: req.outputFormat(), Stream: req.Stream}}, nil
}

// handleExecute implements executor.execute (non-stream). Stream-flagged
// requests are routed to the stream path instead of rejected.
func (m *Manager) handleExecute(request []byte) ([]byte, error) {
	var req executorRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return ErrEnvelope("invalid_request", "malformed executor request body"), nil
	}
	debugTrace("executor invoked model=%s source_format=%s stream=%t original_body_%s payload_%s", req.Model, req.SourceFormat, req.Stream, debugBodyMeta(req.OriginalRequest), debugBodyMeta(req.Payload))
	if req.Stream {
		return m.executeStream(req)
	}
	res, failEnv := m.resolveExecution(req)
	if res == nil {
		return failEnv, nil
	}
	upstreamBody, eErr := buildUpstreamBody(res, req)
	if eErr != nil {
		return classEnvelope(eErr), nil
	}

	if _, eErr := protocol.Format(req.outputFormat()); eErr != nil {
		return classEnvelope(eErr), nil
	}
	url := res.route.URL(res.baseURL)
	debugTrace("executor resolved public_model=%s upstream_model=%s route=%s url=%s key_count=%d", req.Model, res.rec.UpstreamID, res.route.Protocol, url, len(res.cfg.APIKeys))
	debugTrace("executor sending non-stream url=%s body_len=%d", url, len(upstreamBody))
	ctx, cancel := context.WithTimeout(withHostCallbackScope(m.workContext(), req.HostCallbackID), res.cfg.RequestTimeout)
	defer cancel()
	resp, err := m.bridge.Do(ctx, pluginapi.HTTPRequest{
		Method:  http.MethodPost,
		URL:     url,
		Headers: upstreamAuthHeaders(res.route.Protocol, res.key),
		Body:    upstreamBody,
	})
	if err != nil {
		debugTrace("executor non-stream network error: %v", err)
		return classEnvelope(errclass.FromNetwork(err)), nil
	}
	debugTrace("executor received non-stream status=%d body_len=%d", resp.StatusCode, len(resp.Body))
	if resp.StatusCode >= 400 {
		return classEnvelope(errclass.UpstreamStatusError(resp.StatusCode, resp.Body)), nil
	}
	// MiniMax can report failures in base_resp under a successful status.
	if eErr := errclass.BaseRespError(resp.Body); eErr != nil {
		return classEnvelope(eErr), nil
	}
	// Parse/envelope guard only; true OOM prevention belongs to the host transport's byte cap.
	if int64(len(resp.Body)) > res.cfg.MaxResponseBytes {
		return classEnvelope(errclass.Translation("response exceeds max-response-bytes")), nil
	}
	converted, eErr := convertNonStream(res.route.Protocol, req.outputFormat(), resp.StatusCode, resp.Body, res.state)
	if eErr != nil {
		return classEnvelope(eErr), nil
	}
	return okEnvelope(pluginapi.ExecutorResponse{Payload: converted, Headers: resp.Headers}), nil
}

// buildUpstreamRequest applies native policy before translation.
func buildUpstreamRequest(route catalog.Route, model, source string, body []byte, states ...*protocol.State) ([]byte, *errclass.Error) {
	state := requestState(states)
	body, err := normalizeNativeRequest(route, source, body, state)
	if err != nil {
		return nil, err
	}
	return protocol.ConvertRequest(route, model, source, body, state)
}

func requestState(states []*protocol.State) *protocol.State {
	if len(states) > 0 && states[0] != nil {
		return states[0]
	}
	return &protocol.State{}
}

// upstreamAuthHeaders uses Bearer on every endpoint, as MiniMax recommends;
// it also takes precedence over x-api-key on the Anthropic endpoint.
func upstreamAuthHeaders(route catalog.Route, key string) http.Header {
	h := make(http.Header)
	h.Set("Authorization", "Bearer "+key)
	h.Set("Content-Type", "application/json")
	if route == catalog.RouteMessages {
		h.Set("anthropic-version", "2023-06-01")
	}
	return h
}

func convertNonStream(route catalog.Route, output string, status int, body []byte, states ...*protocol.State) ([]byte, *errclass.Error) {
	return protocol.ConvertResponse(route, output, status, body, requestState(states))
}

// classEnvelope renders a classified failure as the wire error envelope;
// ToEnvelopeError redacts and propagates Retryable/HTTPStatus host-side.
func classEnvelope(e *errclass.Error) []byte {
	wire := errclass.ToEnvelopeError(e)
	out, _ := json.Marshal(pluginabi.Envelope{OK: false, Error: &wire})
	return out
}

type streamConverter = protocol.StreamConverter

func newStreamConverter(route catalog.Route, output string, state *protocol.State) (streamConverter, *errclass.Error) {
	return protocol.NewStreamConverter(route, output, state)
}

// handleExecuteStream implements executor.execute_stream (FR-006, §7).
// It decodes once and delegates to executeStream so a stream-flagged
// request arriving via executor.execute is never parsed twice.
func (m *Manager) handleExecuteStream(request []byte) ([]byte, error) {
	var req executorRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return ErrEnvelope("invalid_request", "malformed executor request body"), nil
	}
	debugTrace("executor stream invoked model=%s source_format=%s stream=%t original_body_%s payload_%s", req.Model, req.SourceFormat, req.Stream, debugBodyMeta(req.OriginalRequest), debugBodyMeta(req.Payload))
	return m.executeStream(req)
}

// executeStream runs the already-decoded stream execution: pre-first-byte errors
// (invalid request, unroutable model, upstream HTTP >=400) return immediately as
// error envelopes so CPA can failover pre-emission.
// On success (upstream HTTP 200 OK), the reading and emitting pump loop runs in
// a background goroutine and executeStream returns okEnvelope immediately so the
// host can start draining chunks to the downstream client without buffer deadlocks.
func (m *Manager) executeStream(req executorRequest) ([]byte, error) {
	res, failEnv := m.resolveExecution(req)
	if res == nil {
		return failEnv, nil
	}
	// This path always streams, whatever the request's Stream flag says, so
	// a converted Gemini source must ask the upstream for a stream too.
	res.state.Stream = true
	upstreamBody, eErr := buildUpstreamBody(res, req)
	if eErr != nil {
		return classEnvelope(eErr), nil
	}

	conv, eErr := newStreamConverter(res.route.Protocol, req.outputFormat(), res.state)
	if eErr != nil {
		return classEnvelope(eErr), nil
	}
	url := res.route.URL(res.baseURL)
	debugTrace("executor sending stream url=%s body_len=%d", url, len(upstreamBody))
	ctx, cancel := context.WithTimeout(withHostCallbackScope(m.workContext(), req.HostCallbackID), res.cfg.RequestTimeout)
	owned := false
	defer func() {
		if !owned {
			cancel()
		}
	}()
	st, headers, id, err := m.bridge.DoStream(ctx, pluginapi.HTTPRequest{
		Method:  http.MethodPost,
		URL:     url,
		Headers: upstreamAuthHeaders(res.route.Protocol, res.key),
		Body:    upstreamBody,
	})
	debugTrace("executor stream DoStream status=%d upstreamID=%s err=%v", st, id, err)
	if err != nil {
		return classEnvelope(errclass.FromNetwork(err)), nil
	}
	if st >= 400 {
		return classEnvelope(errclass.UpstreamStatusError(st, m.readStreamErrorBody(ctx, id))), nil
	}
	// MiniMax can answer a stream request with a plain JSON base_resp error
	// under HTTP 200. Classify it before the first byte so CPA can cool the
	// credential down and retry another one.
	if isJSONContentType(headers.Get("Content-Type")) {
		body := m.readStreamErrorBody(ctx, id)
		if eErr := errclass.BaseRespError(body); eErr != nil {
			return classEnvelope(eErr), nil
		}
		return classEnvelope(errclass.UpstreamFallback("stream request returned a JSON body instead of an event stream")), nil
	}

	downID := req.StreamID
	owned = true
	m.work.Add(1) // the admitted handler still holds a work count
	if m.bridge != nil {
		m.bridge.inFlight.Add(1)
	}
	go func() {
		defer m.work.Done()
		defer cancel()
		if m.bridge != nil {
			defer m.bridge.inFlight.Done()
		}
		m.pumpStreamContext(ctx, downID, id, res, conv)
	}()
	return okEnvelope(struct{}{}), nil
}

// readStreamErrorBody reads the first chunk of a failed upstream stream, which
// holds the error body, and closes the stream. ctx cancellation closes it early.
const streamErrorBodyLimit = 64 << 10

func (m *Manager) readStreamErrorBody(ctx context.Context, id string) []byte {
	if id == "" {
		return nil
	}
	var once sync.Once
	closeUpstream := func() { once.Do(func() { _ = m.bridge.StreamClose(id) }) }
	watchDone, watchExited := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(watchExited)
		select {
		case <-ctx.Done():
			closeUpstream()
		case <-watchDone:
		}
	}()
	// The host may split a small error body across reads; collect it up to
	// streamErrorBodyLimit so the JSON still parses.
	var body []byte
	for len(body) < streamErrorBodyLimit && ctx.Err() == nil {
		payload, errMsg, done, err := m.bridge.StreamRead(id)
		body = append(body, payload...)
		if err != nil || errMsg != "" || done {
			break
		}
	}
	closeUpstream()
	close(watchDone)
	<-watchExited
	return body
}

func isJSONContentType(value string) bool {
	mediaType, _, _ := strings.Cut(strings.ToLower(strings.TrimSpace(value)), ";")
	return strings.TrimSpace(mediaType) == "application/json"
}

func (m *Manager) pumpStreamContext(ctx context.Context, downID, upstreamID string, res *resolvedExecution, conv streamConverter) {
	var closeOnce sync.Once
	closeStreams := func(downErrMsg string) {
		closeOnce.Do(func() {
			_ = m.bridge.StreamClose(upstreamID)
			_ = m.bridge.StreamCloseDownstream(downID, downErrMsg)
		})
	}
	defer closeStreams("")

	watchDone := make(chan struct{})
	watchExited := make(chan struct{})
	go func() {
		defer close(watchExited)
		select {
		case <-ctx.Done():
			closeStreams("stream canceled or exceeded request-timeout")
		case <-watchDone:
		}
	}()
	defer func() { close(watchDone); <-watchExited }()

	var (
		total          int64
		upstreamClosed bool
		convDone       bool
	)
	for {
		payload, readErrMsg, closed, err := m.bridge.StreamRead(upstreamID)
		debugTrace("executor stream read chunk_len=%d closed=%t readErrMsg=%q err=%v", len(payload), closed, readErrMsg, err)
		upstreamClosed = closed
		if ctx.Err() != nil {
			closeStreams("stream canceled or exceeded request-timeout")
			return
		}
		if err != nil {
			closeStreams(errclass.Redact(err.Error()))
			return
		}
		if readErrMsg != "" {
			closeStreams(errclass.Redact(readErrMsg))
			return
		}
		total += int64(len(payload))
		if total > res.cfg.MaxResponseBytes {
			closeStreams(errclass.Redact("stream exceeded max-response-bytes"))
			return
		}
		events, done, convErr := conv.Feed(payload)
		if emitErr := m.emitAll(downID, events); emitErr != nil {
			closeStreams(errclass.Redact(emitErr.Error()))
			return
		}
		if convErr != nil {
			closeStreams(errclass.Redact(convErr.Message))
			return
		}
		convDone = done
		if convDone || upstreamClosed {
			break
		}
	}
	debugTrace("executor stream loop end convDone=%t upstreamClosed=%t", convDone, upstreamClosed)
	if !convDone && upstreamClosed {
		events, finishErr := conv.Finish()
		if emitErr := m.emitAll(downID, events); emitErr != nil {
			closeStreams(errclass.Redact(emitErr.Error()))
			return
		}
		if finishErr != nil {
			closeStreams(errclass.Redact(finishErr.Message))
			return
		}
	}
}

// emitAll feeds converted events downstream in order. StreamEmit is
// deadline-bounded (emitTimeout), so a host that stops draining the
// downstream stream surfaces here as an error; the pump loop treats that
// as a post-first-byte stream-fatal failure and runs fail(), whose
// Once-closer releases both streams.
func (m *Manager) emitAll(downStreamID string, events [][]byte) error {
	for _, evt := range events {
		if err := m.bridge.StreamEmit(downStreamID, evt); err != nil {
			return err
		}
	}
	return nil
}
