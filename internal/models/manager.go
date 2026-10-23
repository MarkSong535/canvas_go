package models

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"sync"
	"time"

	"canvas_go/internal/logger"
)

const placeholder = "PLACEHOLDER"

// MaxPricePer1MTokens: neither the input nor the output price (USD per 1M tokens) may exceed it.
const MaxPricePer1MTokens = 5.0

// ModelConfig is one entry of the model table.
type ModelConfig struct {
	ModelID     string
	Aliases     []string
	InputPrice  float64
	OutputPrice float64
	Params      map[string]any
}

// OpenAIModels lists OpenAI standard-tier prices.
var OpenAIModels = []ModelConfig{
	{ModelID: "gpt-6-luna", Aliases: []string{"gpt-6-luna", "openai-gpt-6-luna"}, InputPrice: 0.10, OutputPrice: 0.50,
		Params: map[string]any{"reasoning_effort": "none"}},
	{ModelID: "gpt-5.6-luna", Aliases: []string{"gpt-5.6-luna", "openai-gpt-5.6-luna"}, InputPrice: 0.20, OutputPrice: 1.20},
	{ModelID: "gpt-5.4-mini", Aliases: []string{"gpt-5.4-mini", "openai-gpt-5.4-mini"}, InputPrice: 0.75, OutputPrice: 4.50},
	{ModelID: "gpt-5.4-nano", Aliases: []string{"gpt-5.4-nano", "openai-gpt-5.4-nano"}, InputPrice: 0.20, OutputPrice: 1.25},
	{ModelID: "gpt-5-mini", Aliases: []string{"gpt-5-mini", "openai-gpt-5-mini"}, InputPrice: 0.25, OutputPrice: 2.00},
	{ModelID: "gpt-5-nano", Aliases: []string{"gpt-5-nano", "openai-gpt-5-nano"}, InputPrice: 0.05, OutputPrice: 0.40},
	{ModelID: "gpt-4.1-mini", Aliases: []string{"gpt-4.1-mini", "openai-gpt-4.1-mini"}, InputPrice: 0.40, OutputPrice: 1.60},
	{ModelID: "gpt-4.1-nano", Aliases: []string{"gpt-4.1-nano", "openai-gpt-4.1-nano"}, InputPrice: 0.10, OutputPrice: 0.40},
	{ModelID: "gpt-4o-mini", Aliases: []string{"gpt-4o-mini", "openai-gpt-4o-mini"}, InputPrice: 0.15, OutputPrice: 0.60},
	{ModelID: "o4-mini", Aliases: []string{"o4-mini", "openai-o4-mini"}, InputPrice: 1.10, OutputPrice: 4.40},
}

// ModelManager registers and looks up the available models.
type ModelManager struct {
	mu               sync.RWMutex
	RegisteredModels map[string]Model
	order            []string
}

// Manager is the global model manager.
var Manager = &ModelManager{RegisteredModels: map[string]Model{}}

// Register adds (or replaces) a model under a name.
func (m *ModelManager) Register(name string, model Model) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.RegisteredModels[name]; !ok {
		m.order = append(m.order, name)
	}
	m.RegisteredModels[name] = model
}

// InitModels registers every model (currently only the official OpenAI API).
func (m *ModelManager) InitModels(useLocalProxy bool) (int, error) {
	m.mu.Lock()
	m.RegisteredModels = map[string]Model{}
	m.order = nil
	m.mu.Unlock()
	count := m.registerOpenAIModels(useLocalProxy)
	if count == 0 {
		l := logger.Default
		l.Errorf("%s", repeat("=", 70))
		l.Errorf("No usable LLM configuration found!")
		l.Errorf("%s", repeat("=", 70))
		l.Errorf("Configure at least one of the following in .env:")
		l.Errorf("- OPENAI_API_KEY (+ optional OPENAI_BASE_URL / OPENAI_ORGANIZATION / OPENAI_PROJECT)")
		l.Errorf("%s", repeat("=", 70))
		return 0, fmt.Errorf("Model configuration missing; check your .env file")
	}
	return count, nil
}

func repeat(s string, n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += s
	}
	return out
}

func getenv(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

func (m *ModelManager) registerOpenAIModels(useLocalProxy bool) int {
	l := logger.Default
	l.Infof("Registering OpenAI models")
	var apiKey, apiBase string
	if useLocalProxy {
		apiKey = getenv("LOCAL_OPENAI_API_KEY", placeholder)
		if apiKey == placeholder {
			l.Warnf("Local API key LOCAL_OPENAI_API_KEY is not set, using remote API key OPENAI_API_KEY")
			apiKey = getenv("OPENAI_API_KEY", placeholder)
		}
		apiBase = getenv("LOCAL_OPENAI_API_BASE", placeholder)
		if apiBase == placeholder {
			l.Warnf("Local API base LOCAL_OPENAI_API_BASE is not set, using remote API base OPENAI_API_BASE")
			apiBase = getenv("OPENAI_API_BASE", placeholder)
		}
	} else {
		apiKey = getenv("OPENAI_API_KEY", placeholder)
		apiBase = getenv("OPENAI_API_BASE", placeholder)
	}
	// Also accept other common names for the base-URL environment variable
	if apiBase == placeholder {
		apiBase = getenv("OPENAI_BASE_URL", placeholder)
	}
	if apiBase == placeholder {
		apiBase = getenv("OPENAI_API_URL", placeholder)
	}
	if apiKey == placeholder {
		l.Warnf("OPENAI_API_KEY not found; skipping OpenAI model registration")
		return 0
	}
	organization := os.Getenv("OPENAI_ORGANIZATION")
	if organization == "" {
		organization = os.Getenv("OPENAI_ORG")
	}
	if apiBase == placeholder {
		apiBase = ""
	}
	cfg := ClientConfig{APIKey: apiKey, BaseURL: apiBase, Organization: organization, Project: os.Getenv("OPENAI_PROJECT")}
	if raw := os.Getenv("OPENAI_MAX_RETRIES"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			cfg.MaxRetries = &n
		} else {
			l.Warnf("OPENAI_MAX_RETRIES is not a valid integer; ignoring it")
		}
	}
	if raw := os.Getenv("OPENAI_TIMEOUT"); raw != "" {
		if f, err := strconv.ParseFloat(raw, 64); err == nil {
			cfg.Timeout = time.Duration(f * float64(time.Second))
		} else {
			l.Warnf("OPENAI_TIMEOUT is not a valid number; ignoring it")
		}
	}
	client := &SDKClient{Client: NewSDKClient(cfg)}

	registered := 0
	for _, cfg := range OpenAIModels {
		aliases := cfg.Aliases
		if len(aliases) == 0 {
			aliases = []string{cfg.ModelID}
		}
		// Price-cap guard: a model whose input or output price exceeds the cap is never registered
		if math.Max(cfg.InputPrice, cfg.OutputPrice) > MaxPricePer1MTokens {
			l.Warnf("Model %s costs more than $%v/1M tokens; not registering it", cfg.ModelID, MaxPricePer1MTokens)
			continue
		}
		params := map[string]any{}
		for k, v := range cfg.Params {
			params[k] = v
		}
		model := NewOpenAIServerModel(cfg.ModelID, client, params)
		for _, alias := range aliases {
			m.mu.RLock()
			_, exists := m.RegisteredModels[alias]
			m.mu.RUnlock()
			if exists {
				l.Debugf("Model alias %s already exists; not overwriting it", alias)
				continue
			}
			m.Register(alias, model)
			registered++
			l.Infof("Registered OpenAI model: %s (ID: %s)", alias, cfg.ModelID)
		}
	}
	if registered == 0 {
		l.Warnf("No OpenAI models were registered; check the model aliases or configuration")
	}
	return registered
}

// GetModel returns a registered model.
func (m *ModelManager) GetModel(name string) (Model, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	model, ok := m.RegisteredModels[name]
	if !ok {
		return nil, fmt.Errorf("Model %s is not registered. Available models: %v", name, m.order)
	}
	return model, nil
}

// ListModels lists registered model names in registration order.
func (m *ModelManager) ListModels() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]string(nil), m.order...)
}
