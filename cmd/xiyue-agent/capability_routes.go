package main

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type capabilityRequestKey struct{}

func capabilityEndpoint(path string) bool {
	return path == "/mcp" || strings.HasPrefix(path, "/api/v1/capabilities") || strings.HasPrefix(path, "/api/v1/operations/")
}

func sendCapabilityError(w http.ResponseWriter, err error) {
	problem := capabilityFailure(err)
	code := http.StatusUnprocessableEntity
	switch problem.Code {
	case "invalid_arguments":
		code = http.StatusBadRequest
	case "forbidden":
		code = http.StatusForbidden
	case "not_found":
		code = http.StatusNotFound
	case "conflict", "disabled":
		code = http.StatusConflict
	case "busy", "unavailable":
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, map[string]any{"error": problem.Message, "code": problem.Code})
}

func (a *api) externalCapabilityRequest(w http.ResponseWriter, r *http.Request, next http.Handler) {
	w.Header().Set("Cache-Control", "no-store")
	if a.admin == nil || r.TLS == nil {
		http.NotFound(w, r)
		return
	}
	grant := a.platform.externalCapabilityGrant(r)
	if grant == nil {
		w.Header().Set("WWW-Authenticate", `Bearer realm="xiyue-capabilities"`)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "请提供有效的外部能力授权", "code": "unauthorized"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 128<<10)
	c := capabilityContext{platform: a.platform, room: a.room, model: a.deviceModel, firmware: a.deviceFirmware, grant: grant}
	next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), capabilityRequestKey{}, c)))
}

func (a *api) capabilityRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/capability-grants", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		result := []map[string]any{}
		s := a.platform.snapshot()
		for _, g := range s.CapabilityGrants {
			result = append(result, grantView(g))
		}
		operations := []map[string]any{}
		for _, op := range deviceCapabilities {
			if op.External {
				operations = append(operations, map[string]any{"id": op.ID, "description": op.Description, "writes": op.Write})
			}
		}
		workflows := []map[string]string{}
		for _, rule := range s.Rules {
			if externalWorkflowAllowed(rule) {
				workflows = append(workflows, map[string]string{"id": rule.ID, "name": rule.Name})
			}
		}
		writeJSON(w, 200, map[string]any{"grants": result, "operations": operations, "workflows": workflows, "available": a.admin != nil})
	})
	m.HandleFunc("POST /api/capability-grants", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if a.admin == nil {
			http.NotFound(w, r)
			return
		}
		var input capabilityGrantInput
		if err := decode(w, r, &input); err != nil {
			bad(w, err)
			return
		}
		grant, token, err := a.grantCapabilityAccess(input)
		if err != nil {
			sendCapabilityError(w, err)
			return
		}
		writeJSON(w, 201, map[string]any{"grant": grantView(grant), "token": token})
	})
	m.HandleFunc("DELETE /api/capability-grants/{id}", func(w http.ResponseWriter, r *http.Request) {
		if a.admin == nil {
			http.NotFound(w, r)
			return
		}
		id := r.PathValue("id")
		if err := a.platform.change(func(s *State) error {
			s.CapabilityGrants = slices.DeleteFunc(s.CapabilityGrants, func(g capabilityGrant) bool { return g.ID == id })
			s.CapabilityReceipts = slices.DeleteFunc(s.CapabilityReceipts, func(entry capabilityReceipt) bool { return entry.Grant == id })
			return nil
		}); err != nil {
			sendCapabilityError(w, err)
			return
		}
		writeJSON(w, 200, map[string]bool{"revoked": true})
	})
	m.HandleFunc("GET /api/v1/capabilities", func(w http.ResponseWriter, r *http.Request) {
		c := r.Context().Value(capabilityRequestKey{}).(capabilityContext)
		result := []capability{}
		for _, op := range deviceCapabilities {
			if op.External && slices.Contains(c.grant.Operations, op.ID) {
				result = append(result, op)
			}
		}
		writeJSON(w, 200, map[string]any{"operations": result})
	})
	run := func(w http.ResponseWriter, r *http.Request, id string, args json.RawMessage) {
		c := r.Context().Value(capabilityRequestKey{}).(capabilityContext)
		result, err := c.call(r.Context(), id, args)
		a.appendCapabilityAudit(c, id, err)
		if err != nil {
			sendCapabilityError(w, err)
			return
		}
		code := http.StatusOK
		if status, ok := result.(capabilityOutcome); ok && status.Status == "accepted" {
			code = http.StatusAccepted
		}
		writeJSON(w, code, result)
	}
	m.HandleFunc("POST /api/v1/capabilities/{operation}/invoke", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := decode(w, r, &in); err != nil {
			bad(w, err)
			return
		}
		run(w, r, r.PathValue("operation"), in.Arguments)
	})
	m.HandleFunc("GET /api/v1/operations/{id}", func(w http.ResponseWriter, r *http.Request) {
		args, _ := json.Marshal(operationCapabilityInput{OperationID: r.PathValue("id")})
		run(w, r, "operation.get", args)
	})
	mcpEndpoint := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		c := r.Context().Value(capabilityRequestKey{}).(capabilityContext)
		server := mcp.NewServer(&mcp.Implementation{Name: "xiyue-voice", Version: "1.0.0"}, nil)
		for _, op := range deviceCapabilities {
			if !op.External || !slices.Contains(c.grant.Operations, op.ID) {
				continue
			}
			server.AddTool(&mcp.Tool{Name: op.name(), Description: op.Description, InputSchema: op.Input, OutputSchema: op.Output,
				Annotations: &mcp.ToolAnnotations{ReadOnlyHint: !op.Write}}, func(ctx context.Context, r *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				result, err := c.call(ctx, op.ID, r.Params.Arguments)
				a.appendCapabilityAudit(c, op.ID, err)
				if err != nil {
					problem, _ := json.Marshal(capabilityFailure(err))
					return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: string(problem)}}}, nil
				}
				text, err := json.Marshal(result)
				if err != nil {
					return nil, err
				}
				return &mcp.CallToolResult{StructuredContent: result, Content: []mcp.Content{&mcp.TextContent{Text: string(text)}}}, nil
			})
		}
		return server
	}, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	for _, method := range []string{"GET", "POST", "DELETE"} {
		m.Handle(method+" /mcp", mcpEndpoint)
	}
}
