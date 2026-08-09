package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"aira/internal/core"
	"aira/internal/store"
)

type mcpProvider func(context.Context, core.Request) (*core.Core, func(), error)

type mcpServer struct {
	provider mcpProvider
	tools    []mcpTool
	byName   map[string]mcpToolBinding
}

type mcpTool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema any    `json:"inputSchema"`
}

type mcpToolBinding struct {
	tool        mcpTool
	byOperation map[string]mcpOperation
}

type mcpOperation struct {
	descriptor   core.DispatchDescriptor
	operation    string
	declaredArgs []core.ArgSpec
}

type mcpInputSchema struct {
	Type       string                 `json:"type"`
	Properties map[string]mcpProperty `json:"properties"`
	Required   []string               `json:"required,omitempty"`
}

type mcpProperty struct {
	Type        string   `json:"type"`
	Items       any      `json:"items,omitempty"`
	Enum        []string `json:"enum,omitempty"`
	Description string   `json:"description,omitempty"`
}

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type mcpResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *mcpRPCError    `json:"error,omitempty"`
}

type mcpRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func newMCPServer(provider mcpProvider) *mcpServer {
	descriptors := core.New(nil).DispatchDescriptors()
	grouped := map[string][]core.DispatchDescriptor{}
	for _, descriptor := range descriptors {
		if descriptor.MCPTool != "" {
			grouped[descriptor.MCPTool] = append(grouped[descriptor.MCPTool], descriptor)
		}
	}
	toolNames := make([]string, 0, len(grouped))
	for name := range grouped {
		toolNames = append(toolNames, name)
	}
	sort.Strings(toolNames)
	server := &mcpServer{provider: provider, byName: make(map[string]mcpToolBinding, len(toolNames))}
	for _, name := range toolNames {
		binding := makeToolBinding(name, grouped[name])
		server.tools = append(server.tools, binding.tool)
		server.byName[name] = binding
	}
	return server
}

func makeToolBinding(name string, descriptors []core.DispatchDescriptor) mcpToolBinding {
	sort.Slice(descriptors, func(i, j int) bool { return descriptors[i].Name < descriptors[j].Name })
	operations := map[string]mcpOperation{}
	allArgs := map[string]core.ArgSpec{}
	multiple := len(descriptors) > 1
	for _, descriptor := range descriptors {
		operation := descriptor.MCPOperation
		for _, arg := range descriptor.Args {
			if arg.Name == "subverb" {
				if operation == "" || operation == "subverb" {
					for _, value := range arg.Enum {
						operations[value] = makeMCPOperation(descriptor, value)
					}
				}
				multiple = true
				continue
			}
			if descriptor.Name == "link" && arg.Name == "list" {
				continue
			}
			allArgs[arg.Name] = arg
		}
		if operation != "" && operation != "subverb" {
			operations[operation] = makeMCPOperation(descriptor, operation)
		}
	}
	// `link ls <id>` is the third operation of the grouped link tool.
	if link, ok := descriptorNamed(descriptors, "link"); ok {
		if _, exists := operations["list"]; !exists {
			operations["list"] = makeMCPOperation(link, "list")
		}
	}
	if !multiple && len(operations) == 0 {
		operations[""] = makeMCPOperation(descriptors[0], "")
	}
	if len(operations) > 1 {
		allArgs["operation"] = core.ArgSpec{Name: "operation", Kind: core.ArgKindString, Required: true, Enum: sortedOperationNames(operations), Description: "Operation"}
	}
	propertyNames := make([]string, 0, len(allArgs))
	for arg := range allArgs {
		propertyNames = append(propertyNames, arg)
	}
	sort.Strings(propertyNames)
	properties := make(map[string]mcpProperty, len(propertyNames))
	for _, argName := range propertyNames {
		arg := allArgs[argName]
		property := mcpProperty{Enum: append([]string(nil), arg.Enum...), Description: arg.Description}
		switch arg.Kind {
		case core.ArgKindBool:
			property.Type = "boolean"
		case core.ArgKindStringList:
			property.Type = "array"
			property.Items = map[string]string{"type": "string"}
		default:
			property.Type = "string"
		}
		properties[arg.Name] = property
	}
	schema := mcpInputSchema{Type: "object", Properties: properties}
	if len(operations) == 1 {
		for _, operation := range operations {
			for _, arg := range operation.declaredArgs {
				if arg.Required {
					schema.Required = append(schema.Required, arg.Name)
				}
			}
		}
	} else {
		schema.Required = []string{"operation"}
	}
	sort.Strings(schema.Required)
	description := name
	if len(descriptors) > 0 && descriptors[0].Usage != "" {
		description = descriptors[0].Usage
	}
	return mcpToolBinding{
		tool:        mcpTool{Name: name, Description: description, InputSchema: schema},
		byOperation: operations,
	}
}

func makeMCPOperation(descriptor core.DispatchDescriptor, operation string) mcpOperation {
	declared := make([]core.ArgSpec, 0, len(descriptor.Args))
	for _, arg := range descriptor.Args {
		if arg.Name == "subverb" || (descriptor.Name == "link" && arg.Name == "list") {
			continue
		}
		declared = append(declared, arg)
	}
	return mcpOperation{descriptor: descriptor, operation: operation, declaredArgs: declared}
}

func descriptorNamed(descriptors []core.DispatchDescriptor, name string) (core.DispatchDescriptor, bool) {
	for _, descriptor := range descriptors {
		if descriptor.Name == name {
			return descriptor, true
		}
	}
	return core.DispatchDescriptor{}, false
}

func sortedOperationNames(operations map[string]mcpOperation) []string {
	result := make([]string, 0, len(operations))
	for name := range operations {
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}

// Serve implements the minimal line-delimited JSON-RPC stdio lifecycle. It
// intentionally writes no diagnostics to stdout.
func (s *mcpServer) Serve(ctx context.Context, input io.Reader, output, diagnostics io.Writer) error {
	reader := bufio.NewReader(input)
	for {
		line, err := reader.ReadString('\n')
		if len(strings.TrimSpace(line)) > 0 {
			response, respond := s.handle(ctx, []byte(line))
			if !respond {
				if err == io.EOF {
					return nil
				}
				continue
			}
			if err := writeMCP(output, response); err != nil {
				return err
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			if diagnostics != nil {
				_, _ = fmt.Fprintf(diagnostics, "aira mcp: %v\n", err)
			}
			return err
		}
	}
}

func (s *mcpServer) handle(ctx context.Context, line []byte) (mcpResponse, bool) {
	var request mcpRequest
	if err := json.Unmarshal(line, &request); err != nil {
		return protocolResponse(nil, -32700, "parse error", nil), true
	}
	if request.JSONRPC != "2.0" || request.Method == "" {
		return protocolResponse(requestID(request.ID), -32600, "invalid request", nil), true
	}
	id := requestID(request.ID)
	switch request.Method {
	case "initialize":
		result := map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]string{"name": "aira", "version": "m8a"},
		}
		return resultResponse(id, result), hasRequestID(request.ID)
	case "notifications/initialized":
		return mcpResponse{}, false
	case "tools/list":
		return resultResponse(id, map[string]any{"tools": s.tools}), hasRequestID(request.ID)
	case "tools/call":
		return s.call(ctx, id, request.Params), hasRequestID(request.ID)
	case "ping":
		// MCP hosts may ping for liveness; answer with an empty result.
		return resultResponse(id, map[string]any{}), hasRequestID(request.ID)
	default:
		return protocolResponse(id, -32601, "method not found", nil), hasRequestID(request.ID)
	}
}

func (s *mcpServer) call(ctx context.Context, id any, raw json.RawMessage) mcpResponse {
	var params struct {
		Name      string                     `json:"name"`
		Arguments map[string]json.RawMessage `json:"arguments"`
	}
	if len(raw) == 0 || string(raw) == "null" || json.Unmarshal(raw, &params) != nil || params.Name == "" {
		return protocolResponse(id, -32602, "invalid tool call parameters", stableArgumentData("invalid tool call parameters"))
	}
	binding, ok := s.byName[params.Name]
	if !ok {
		return protocolResponse(id, -32602, "unknown tool", stableArgumentData("unknown tool"))
	}
	request, err := decodeMCPRequest(binding, params.Arguments)
	if err != nil {
		return protocolResponse(id, -32602, err.Error(), stableArgumentData(err.Error()))
	}
	dispatcher, closeFn, err := s.provider(ctx, request)
	if err != nil {
		code := store.ErrorCode(err)
		response := core.Response{Code: code, Error: err.Error(), Exit: store.ExitForCode(code)}
		return toolResponse(id, response)
	}
	if closeFn != nil {
		defer closeFn()
	}
	return toolResponse(id, dispatcher.Do(ctx, request))
}

func decodeMCPRequest(binding mcpToolBinding, values map[string]json.RawMessage) (core.Request, error) {
	operation := ""
	if len(binding.byOperation) > 1 {
		raw, ok := values["operation"]
		if !ok {
			return core.Request{}, fmt.Errorf("E_ARGUMENT_INVALID: operation is required")
		}
		if err := json.Unmarshal(raw, &operation); err != nil {
			return core.Request{}, fmt.Errorf("E_ARGUMENT_INVALID: operation must be a string")
		}
	}
	bindingOperation, ok := binding.byOperation[operation]
	if !ok {
		return core.Request{}, fmt.Errorf("E_ARGUMENT_INVALID: unknown operation %q", operation)
	}
	args := map[string]any{}
	allowed := map[string]core.ArgSpec{}
	for _, arg := range bindingOperation.declaredArgs {
		allowed[arg.Name] = arg
	}
	for name := range values {
		if name == "operation" {
			if len(binding.byOperation) > 1 {
				continue
			}
			return core.Request{}, fmt.Errorf("E_ARGUMENT_INVALID: unknown argument %q", name)
		}
		if _, ok := allowed[name]; !ok {
			return core.Request{}, fmt.Errorf("E_ARGUMENT_INVALID: unknown argument %q", name)
		}
	}
	for _, arg := range bindingOperation.declaredArgs {
		raw, present := values[arg.Name]
		if !present {
			if arg.Required {
				return core.Request{}, fmt.Errorf("E_ARGUMENT_INVALID: argument %q is required", arg.Name)
			}
			continue
		}
		value, err := decodeMCPValue(arg, raw)
		if err != nil {
			return core.Request{}, err
		}
		args[arg.Name] = value
	}
	if bindingOperation.descriptor.MCPOperation == "subverb" {
		args["subverb"] = operation
	}
	if bindingOperation.descriptor.Name == "link" && operation == "list" {
		args["list"] = true
	}
	if bindingOperation.descriptor.Name == "find" && operation == "add" {
		// The CLI always carries the optional requirement field in the
		// canonical add request, even when it is empty.
		if _, present := args["requirement"]; !present {
			args["requirement"] = ""
		}
	}
	return core.Request{Verb: bindingOperation.descriptor.Name, Args: args}, nil
}

func decodeMCPValue(arg core.ArgSpec, raw json.RawMessage) (any, error) {
	if string(raw) == "null" {
		return nil, fmt.Errorf("E_ARGUMENT_INVALID: argument %q cannot be null", arg.Name)
	}
	var value any
	switch arg.Kind {
	case core.ArgKindString:
		var typed string
		if err := json.Unmarshal(raw, &typed); err != nil {
			return nil, fmt.Errorf("E_ARGUMENT_INVALID: argument %q must be a string", arg.Name)
		}
		if arg.Name == "line" {
			line, err := strconv.Atoi(typed)
			if err != nil || line <= 0 {
				return nil, fmt.Errorf("E_ARGUMENT_INVALID: argument %q must be a positive line number", arg.Name)
			}
			value = line
		} else {
			value = typed
		}
	case core.ArgKindBool:
		var typed bool
		if err := json.Unmarshal(raw, &typed); err != nil {
			return nil, fmt.Errorf("E_ARGUMENT_INVALID: argument %q must be a boolean", arg.Name)
		}
		value = typed
	case core.ArgKindStringList:
		var typed []string
		if err := json.Unmarshal(raw, &typed); err != nil {
			return nil, fmt.Errorf("E_ARGUMENT_INVALID: argument %q must be an array of strings", arg.Name)
		}
		value = typed
	default:
		return nil, fmt.Errorf("E_ARGUMENT_INVALID: argument %q has unsupported kind", arg.Name)
	}
	if len(arg.Enum) > 0 {
		stringValue, ok := value.(string)
		if !ok || !contains(arg.Enum, stringValue) {
			return nil, fmt.Errorf("E_ARGUMENT_INVALID: argument %q is outside its closed enum", arg.Name)
		}
	}
	return value, nil
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func toolResponse(id any, response core.Response) mcpResponse {
	encoded, _ := json.Marshal(response)
	result := map[string]any{
		"isError":           !response.OK,
		"structuredContent": response,
		"content":           []map[string]string{{"type": "text", "text": string(encoded)}},
	}
	return resultResponse(id, result)
}

func stableArgumentData(message string) map[string]any {
	return map[string]any{"code": "E_ARGUMENT_INVALID", "exit": store.ExitForCode("E_ARGUMENT_INVALID"), "message": message}
}

func resultResponse(id any, result any) mcpResponse {
	encoded, _ := json.Marshal(result)
	return mcpResponse{JSONRPC: "2.0", ID: id, Result: encoded}
}

func protocolResponse(id any, code int, message string, data any) mcpResponse {
	return mcpResponse{JSONRPC: "2.0", ID: id, Error: &mcpRPCError{Code: code, Message: message, Data: data}}
}

func requestID(raw json.RawMessage) any {
	if !hasRequestID(raw) {
		return nil
	}
	var id any
	if json.Unmarshal(raw, &id) != nil {
		return nil
	}
	return id
}

func hasRequestID(raw json.RawMessage) bool {
	return len(raw) > 0
}

func writeMCP(output io.Writer, response mcpResponse) error {
	encoded, err := json.Marshal(response)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(output, "%s\n", encoded)
	return err
}
