package mcp

import (
	"fmt"

	"github.com/hexagon-codes/ai-core/llm"
)

const maxLLMToolSchemaDepth = 128

var validJSONSchemaTypes = map[string]struct{}{
	"array":   {},
	"boolean": {},
	"integer": {},
	"null":    {},
	"number":  {},
	"object":  {},
	"string":  {},
}

// validateLLMToolSchema is the final trust boundary before an external MCP
// schema is serialized into a provider request. It accepts standard
// composition-only and unconstrained nodes, but rejects values that providers
// cannot interpret safely. A rejected schema quarantines only its owning MCP
// tool; it must never fail the entire chat request.
func validateLLMToolSchema(root *llm.Schema) (string, error) {
	if root == nil {
		return "", nil
	}
	if root.Type != "object" {
		return "$", fmt.Errorf("tool parameter root type %q must be object", root.Type)
	}
	return validateLLMToolSchemaNode(root, "$", 0, make(map[*llm.Schema]bool))
}

func validateLLMToolSchemaNode(
	node *llm.Schema,
	path string,
	depth int,
	stack map[*llm.Schema]bool,
) (string, error) {
	if node == nil {
		return path, fmt.Errorf("schema node is nil")
	}
	if depth > maxLLMToolSchemaDepth {
		return path, fmt.Errorf("schema nesting exceeds %d levels", maxLLMToolSchemaDepth)
	}
	if stack[node] {
		return path, fmt.Errorf("schema contains a pointer cycle")
	}
	stack[node] = true
	defer delete(stack, node)

	if node.Type != "" {
		if _, ok := validJSONSchemaTypes[node.Type]; !ok {
			return path + ".type", fmt.Errorf("unsupported JSON Schema type %q", node.Type)
		}
	}

	for name, property := range node.Properties {
		if invalidPath, err := validateLLMToolSchemaNode(
			property,
			path+".properties."+name,
			depth+1,
			stack,
		); err != nil {
			return invalidPath, err
		}
	}
	if node.Items != nil {
		if invalidPath, err := validateLLMToolSchemaNode(
			node.Items,
			path+".items",
			depth+1,
			stack,
		); err != nil {
			return invalidPath, err
		}
	}
	for keyword, branches := range map[string][]*llm.Schema{
		"anyOf": node.AnyOf,
		"oneOf": node.OneOf,
		"allOf": node.AllOf,
	} {
		for index, branch := range branches {
			if invalidPath, err := validateLLMToolSchemaNode(
				branch,
				fmt.Sprintf("%s.%s[%d]", path, keyword, index),
				depth+1,
				stack,
			); err != nil {
				return invalidPath, err
			}
		}
	}
	if node.Not != nil {
		if invalidPath, err := validateLLMToolSchemaNode(
			node.Not,
			path+".not",
			depth+1,
			stack,
		); err != nil {
			return invalidPath, err
		}
	}
	if additional, ok := node.AdditionalProperties.(*llm.Schema); ok {
		if invalidPath, err := validateLLMToolSchemaNode(
			additional,
			path+".additionalProperties",
			depth+1,
			stack,
		); err != nil {
			return invalidPath, err
		}
	}
	return "", nil
}
