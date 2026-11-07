package protocol

import (
	"github.com/dillonzq/apix"
	"github.com/dillonzq/apix/protocol/anthropic"
	"github.com/dillonzq/apix/protocol/google"
	"github.com/dillonzq/apix/protocol/openai"
	"github.com/dillonzq/cpa-minimax/internal/errclass"
)

// countFormat maps a CPA format to its token counting protocol. Chat
// Completions has no counting endpoint.
func countFormat(format string) (apix.Protocol, *errclass.Error) {
	switch format {
	case "claude":
		return anthropic.MessagesCountTokens, nil
	case "openai-response":
		return openai.ResponsesInputTokens, nil
	case "gemini":
		return google.CountTokens, nil
	default:
		return "", &errclass.Error{Class: errclass.ClassUnsupported, Message: "token counting is not supported for " + format + " requests"}
	}
}

// CountTokens converts one token count request from a client format to the
// MiniMax Messages counter (/v1/messages/count_tokens) and its reply back.
type CountTokens struct {
	ex     apix.Exchange
	native bool
}

func NewCountTokens(source, output string) (*CountTokens, *errclass.Error) {
	client, err := countFormat(source)
	if err != nil {
		return nil, err
	}
	out, err := countFormat(output)
	if err != nil {
		return nil, err
	}
	ex, e := registry.NewExchange(client, anthropic.MessagesCountTokens, apix.ExchangeOptions{Output: out})
	if e != nil {
		return nil, Error(e)
	}
	return &CountTokens{ex: ex, native: client == anthropic.MessagesCountTokens}, nil
}

// Native reports whether the client already speaks the Messages counter, in
// which case apix keeps its body bytes apart from the model.
func (c *CountTokens) Native() bool { return c.native }

func (c *CountTokens) ConvertRequest(model string, body []byte) ([]byte, *errclass.Error) {
	out, err := c.ex.ConvertRequest(apix.Payload{Body: body}, model)
	return out.Body, Error(err)
}

func (c *CountTokens) ConvertResponse(status int, body []byte) ([]byte, *errclass.Error) {
	out, err := c.ex.ConvertResponse(status, apix.Payload{Body: body})
	return out.Body, Error(err)
}
