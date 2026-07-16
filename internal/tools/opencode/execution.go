package opencode

import (
	"fmt"
	"strings"
)

// ModelRef identifies an opencode model by provider and model ID.
type ModelRef struct {
	ProviderID string
	ModelID    string
}

// ParseModelRef parses a model in provider/model form.
func ParseModelRef(value string) (ModelRef, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return ModelRef{}, nil
	}

	providerID, modelID, ok := strings.Cut(value, "/")
	providerID = strings.TrimSpace(providerID)
	modelID = strings.TrimSpace(modelID)
	if !ok || providerID == "" || modelID == "" {
		return ModelRef{}, fmt.Errorf("model %q must use provider/model format", value)
	}
	return ModelRef{ProviderID: providerID, ModelID: modelID}, nil
}

func (m ModelRef) String() string {
	if m.ProviderID == "" || m.ModelID == "" {
		return ""
	}
	return m.ProviderID + "/" + m.ModelID
}

// ExecutorOptions controls defaults used when MCP calls omit execution choices.
type ExecutorOptions struct {
	DefaultModel ModelRef
	DefaultAgent string
}

func (o ExecutorOptions) sessionRequest(args createSessionParams) (CreateSessionRequest, error) {
	model, err := o.modelFor(args.Model, args.ProviderID, args.ModelID)
	if err != nil {
		return CreateSessionRequest{}, err
	}

	agent := strings.TrimSpace(args.Agent)
	if agent == "" {
		agent = strings.TrimSpace(o.DefaultAgent)
	}

	return CreateSessionRequest{
		Title:      args.Title,
		ParentID:   args.ParentID,
		ProviderID: model.ProviderID,
		ModelID:    model.ModelID,
		Agent:      agent,
	}, nil
}

func (o ExecutorOptions) promptRequest(args fireParams) PromptRequest {
	agent := strings.TrimSpace(args.Agent)
	if agent == "" {
		agent = strings.TrimSpace(o.DefaultAgent)
	}
	return PromptRequest{
		Prompt: PromptPayload{Text: args.Prompt},
		Agent:  agent,
	}
}

func (o ExecutorOptions) modelFor(value, providerID, modelID string) (ModelRef, error) {
	value = strings.TrimSpace(value)
	providerID = strings.TrimSpace(providerID)
	modelID = strings.TrimSpace(modelID)

	if value != "" && (providerID != "" || modelID != "") {
		return ModelRef{}, fmt.Errorf("model cannot be combined with provider_id or model_id")
	}
	if value != "" {
		return ParseModelRef(value)
	}
	if providerID != "" || modelID != "" {
		if providerID == "" || modelID == "" {
			return ModelRef{}, fmt.Errorf("provider_id and model_id must be provided together")
		}
		return ModelRef{ProviderID: providerID, ModelID: modelID}, nil
	}
	return o.DefaultModel, nil
}
