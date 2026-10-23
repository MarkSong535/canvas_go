// Package canvastools is the Canvas LMS toolset (student permissions), the OpenAI vector-store tools and the
// LaTeX check tool. Canvas tools read CANVAS_ACCESS_TOKEN and CANVAS_URL from the environment on every request.
package canvastools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"canvas_go/internal/jsonval"
	"canvas_go/internal/sandbox"
	"canvas_go/internal/textutil"
	"canvas_go/internal/tools"
)

const WriteBlocked = "Blocked: this assistant never sends changes (submissions, discussion posts) to Canvas, so nothing was " +
	"sent. Tell the student to do this in Canvas directly."

const WritePolicy = " Changes never reach the live Canvas: in sandbox mode they are recorded locally, otherwise the request " +
	"is blocked and nothing is sent."

// ErrNoToken is returned when a Canvas tool is created without CANVAS_ACCESS_TOKEN.
var ErrNoToken = errors.New("CANVAS_ACCESS_TOKEN environment variable not found")

// HTTPClient is shared by every Canvas request (tests may swap it).
var HTTPClient = &http.Client{}

// Param is one query parameter.
type Param struct {
	Key   string
	Value string
}

// base handles the common Canvas API request logic.
type base struct{ tools.Spec }

// CanvasURL is read on every request: in sandbox mode CANVAS_URL is switched to the local server after tools exist.
func CanvasURL() string {
	u := strings.TrimRight(os.Getenv("CANVAS_URL"), "/")
	if u == "" {
		u = "https://canvas.instructure.com"
	}
	if strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "http://127.0.0.1") && !strings.HasPrefix(u, "http://localhost") {
		u = "https://" + strings.TrimPrefix(u, "http://")
	}
	if !strings.Contains(u, "http") {
		u = "https://" + u
	}
	return u
}

// MakeRequest sends an API request. It returns the decoded JSON, or a map with an "error" message.
func MakeRequest(ctx context.Context, method, endpoint string, params []Param, data any) any {
	canvasURL := CanvasURL()
	// Escape the path: model-supplied ids and page URLs may hold spaces or non-ASCII characters
	target := canvasURL + "/api/v1/" + (&url.URL{Path: endpoint}).EscapedPath()
	// Writes never reach the live Canvas: they are only sent to the running local sandbox (which records them
	// instead); anywhere else they are blocked here, before any request is made.
	if strings.ToUpper(method) != http.MethodGet && sandbox.ActiveURL() != canvasURL {
		return map[string]any{"error": WriteBlocked}
	}
	if len(params) > 0 {
		var parts []string
		for _, p := range params {
			parts = append(parts, url.QueryEscape(p.Key)+"="+url.QueryEscape(p.Value))
		}
		target += "?" + strings.Join(parts, "&")
	}
	result, err := func() (any, error) {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		var body io.Reader
		if data != nil {
			encoded, err := jsonval.Marshal(data)
			if err != nil {
				return nil, err
			}
			body = strings.NewReader(string(encoded))
		}
		req, err := http.NewRequestWithContext(ctx, strings.ToUpper(method), target, body)
		if err != nil {
			return nil, err
		}
		// Read on every request, never stored: in sandbox mode the token is swapped for a simulated one at start
		req.Header.Set("Authorization", "Bearer "+os.Getenv("CANVAS_ACCESS_TOKEN"))
		req.Header.Set("Content-Type", "application/json")
		resp, err := HTTPClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}
		switch resp.StatusCode {
		case 200:
			return jsonval.Decode(raw)
		case 404:
			return map[string]any{"error": "Resource not found"}, nil
		default:
			return map[string]any{"error": fmt.Sprintf("API Request Failed (Status Code %d): %s", resp.StatusCode, textutil.DecodeUTF8(raw))}, nil
		}
	}()
	if err != nil {
		return map[string]any{"error": fmt.Sprintf("Request Error: %v", err)}
	}
	return result
}

// apiError returns the error message of a failed request.
func apiError(result any) (string, bool) {
	if m, ok := result.(map[string]any); ok {
		if e, has := m["error"]; has {
			return jsonval.Text(e), true
		}
	}
	return "", false
}

// errUnexpected is a successful Canvas answer that isn't shaped like the endpoint's documented response.
func errUnexpected(want string) error {
	return fmt.Errorf("unexpected response from Canvas (expected %s)", want)
}

// list returns a successful list response of objects.
func list(result any) ([]map[string]any, error) {
	items, ok := result.([]any)
	if !ok {
		return nil, errUnexpected("a list")
	}
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, errUnexpected("a list of objects")
		}
		out = append(out, m)
	}
	return out, nil
}

// field is a field's display text, or fallback when the field is missing or null.
func field(m map[string]any, key, fallback string) string {
	v, ok := m[key]
	if !ok || v == nil {
		return fallback
	}
	return jsonval.Text(v)
}

// object is a nested object field (empty when missing, null or not an object).
func object(m map[string]any, key string) map[string]any {
	v, _ := m[key].(map[string]any)
	if v == nil {
		return map[string]any{}
	}
	return v
}

// number is a numeric field (0 when missing, null or not a number).
func number(m map[string]any, key string) float64 {
	f, _ := jsonval.Float(m[key])
	return f
}

func str(name, description string) tools.Property {
	return tools.Property{Name: name, Type: "string", Description: description}
}

func optional(p tools.Property) tools.Property {
	p.Nullable = true
	return p
}

func spec(name, description string, required []string, props ...tools.Property) base {
	if required == nil {
		required = []string{}
	}
	return base{tools.Spec{ToolName: name, ToolDescription: description, Params: tools.Schema{Properties: props, Required: required}, Output: "any"}}
}
