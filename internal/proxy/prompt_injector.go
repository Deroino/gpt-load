package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/sirupsen/logrus"
)

// PromptInjector handles injection of system prompts for completion verification
type PromptInjector struct{}

// NewPromptInjector creates a new prompt injector
func NewPromptInjector() *PromptInjector {
	return &PromptInjector{}
}

const (
	// CompletionToken is the token we require AI to add at the end of responses
	CompletionToken = "[done]"
	
	// SystemPromptText is the text we inject to ensure completion verification
	SystemPromptText = "IMPORTANT: At the very end of your entire response, you must write the token " + CompletionToken + " to signal completion. This is a mandatory technical requirement."
)

// InjectCompletionPrompt injects completion verification prompt into the request
func (pi *PromptInjector) InjectCompletionPrompt(requestBody []byte, provider string) ([]byte, error) {
	var request map[string]interface{}
	if err := json.Unmarshal(requestBody, &request); err != nil {
		return nil, fmt.Errorf("failed to parse request body: %w", err)
	}

	// Handle different provider formats
	switch provider {
	case "openai":
		return pi.injectOpenAIPrompt(request)
	case "gemini":
		return pi.injectGeminiPrompt(request)
	case "anthropic":
		return pi.injectAnthropicPrompt(request)
	default:
		return pi.injectGenericPrompt(request)
	}
}

// injectOpenAIPrompt injects system prompt for OpenAI format
func (pi *PromptInjector) injectOpenAIPrompt(request map[string]interface{}) ([]byte, error) {
	messages, ok := request["messages"].([]interface{})
	if !ok {
		// No messages array, return original request
		return json.Marshal(request)
	}

	// Check if there's already a system message
	hasSystemMessage := false
	for i, message := range messages {
		if msgMap, ok := message.(map[string]interface{}); ok {
			if role, ok := msgMap["role"].(string); ok && role == "system" {
				// Append to existing system message
				if content, ok := msgMap["content"].(string); ok {
					msgMap["content"] = content + "\n\n" + SystemPromptText
					messages[i] = msgMap
					hasSystemMessage = true
					logrus.Debug("Appended completion prompt to existing OpenAI system message")
					break
				}
			}
		}
	}

	// If no system message exists, prepend one
	if !hasSystemMessage {
		systemMessage := map[string]interface{}{
			"role":    "system",
			"content": SystemPromptText,
		}
		messages = append([]interface{}{systemMessage}, messages...)
		request["messages"] = messages
		logrus.Debug("Prepended completion prompt as new OpenAI system message")
	}

	return json.Marshal(request)
}

// injectGeminiPrompt injects system prompt for Gemini format
func (pi *PromptInjector) injectGeminiPrompt(request map[string]interface{}) ([]byte, error) {
	// Gemini uses systemInstruction field (recommended) or system_instruction (legacy)
	systemPromptPart := map[string]interface{}{
		"text": SystemPromptText,
	}

	// Handle legacy snake_case format by converting to camelCase
	if snakeVal, snakeExists := request["system_instruction"]; snakeExists {
		// Convert snake_case to camelCase
		camelMap, _ := request["systemInstruction"].(map[string]interface{})
		if camelMap == nil {
			camelMap = make(map[string]interface{})
		}

		// Merge snake_case content into camelCase
		if snakeMap, snakeOk := snakeVal.(map[string]interface{}); snakeOk {
			if snakeParts, snakePartsOk := snakeMap["parts"].([]interface{}); snakePartsOk {
				existingParts, _ := camelMap["parts"].([]interface{})
				camelMap["parts"] = append(snakeParts, existingParts...)
			}
		}

		request["systemInstruction"] = camelMap
		delete(request, "system_instruction")
		logrus.Debug("Converted snake_case system_instruction to camelCase systemInstruction")
	}

	// Handle systemInstruction field
	if val, exists := request["systemInstruction"]; exists {
		instruction, ok := val.(map[string]interface{})
		if !ok {
			// Invalid format, create new instruction
			instruction = map[string]interface{}{
				"parts": []interface{}{systemPromptPart},
			}
		} else {
			// Append to existing instruction
			parts, ok := instruction["parts"].([]interface{})
			if !ok {
				parts = []interface{}{}
			}
			parts = append(parts, systemPromptPart)
			instruction["parts"] = parts
		}
		request["systemInstruction"] = instruction
		logrus.Debug("Appended completion prompt to existing Gemini system instruction")
	} else {
		// Create new systemInstruction
		request["systemInstruction"] = map[string]interface{}{
			"parts": []interface{}{systemPromptPart},
		}
		logrus.Debug("Created new Gemini system instruction with completion prompt")
	}

	return json.Marshal(request)
}

// injectAnthropicPrompt injects system prompt for Anthropic format
func (pi *PromptInjector) injectAnthropicPrompt(request map[string]interface{}) ([]byte, error) {
	// Anthropic uses a separate "system" field
	if existingSystem, exists := request["system"]; exists {
		// Append to existing system prompt
		if systemText, ok := existingSystem.(string); ok {
			request["system"] = systemText + "\n\n" + SystemPromptText
			logrus.Debug("Appended completion prompt to existing Anthropic system message")
		}
	} else {
		// Create new system field
		request["system"] = SystemPromptText
		logrus.Debug("Created new Anthropic system field with completion prompt")
	}

	return json.Marshal(request)
}

// injectGenericPrompt attempts to inject prompt with auto-detection
func (pi *PromptInjector) injectGenericPrompt(request map[string]interface{}) ([]byte, error) {
	// Try to detect format based on structure
	if _, hasMessages := request["messages"]; hasMessages {
		// Looks like OpenAI format
		return pi.injectOpenAIPrompt(request)
	} else if _, hasContents := request["contents"]; hasContents {
		// Looks like Gemini format
		return pi.injectGeminiPrompt(request)
	} else if _, hasSystem := request["system"]; hasSystem {
		// Looks like Anthropic format
		return pi.injectAnthropicPrompt(request)
	}

	// Unknown format, return original
	logrus.Warn("Unknown request format for prompt injection, returning original request")
	return json.Marshal(request)
}

// RemoveCompletionToken removes the completion token from response content
func (pi *PromptInjector) RemoveCompletionToken(responseBody []byte, provider string) []byte {
	var response map[string]interface{}
	if err := json.Unmarshal(responseBody, &response); err != nil {
		logrus.Debugf("Failed to parse response for token removal: %v", err)
		return responseBody
	}

	// Handle different provider formats
	var modified bool
	switch provider {
	case "openai":
		modified = pi.removeOpenAIToken(response)
	case "gemini":
		modified = pi.removeGeminiToken(response)
	case "anthropic":
		modified = pi.removeAnthropicToken(response)
	default:
		modified = pi.removeGenericToken(response)
	}

	if modified {
		if modifiedBytes, err := json.Marshal(response); err == nil {
			logrus.Debug("Successfully removed completion token from response")
			return modifiedBytes
		}
	}

	return responseBody
}

// removeOpenAIToken removes completion token from OpenAI response format
func (pi *PromptInjector) removeOpenAIToken(response map[string]interface{}) bool {
	if choices, ok := response["choices"].([]interface{}); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]interface{}); ok {
			if message, ok := choice["message"].(map[string]interface{}); ok {
				if content, ok := message["content"].(string); ok {
					cleaned := pi.cleanTokenFromText(content)
					if cleaned != content {
						message["content"] = cleaned
						return true
					}
				}
			}
		}
	}
	return false
}

// removeGeminiToken removes completion token from Gemini response format
func (pi *PromptInjector) removeGeminiToken(response map[string]interface{}) bool {
	if candidates, ok := response["candidates"].([]interface{}); ok && len(candidates) > 0 {
		if candidate, ok := candidates[0].(map[string]interface{}); ok {
			if content, ok := candidate["content"].(map[string]interface{}); ok {
				if parts, ok := content["parts"].([]interface{}); ok {
					modified := false
					for i, part := range parts {
						if partMap, ok := part.(map[string]interface{}); ok {
							if text, ok := partMap["text"].(string); ok {
								cleaned := pi.cleanTokenFromText(text)
								if cleaned != text {
									partMap["text"] = cleaned
									parts[i] = partMap
									modified = true
								}
							}
						}
					}
					return modified
				}
			}
		}
	}
	return false
}

// removeAnthropicToken removes completion token from Anthropic response format
func (pi *PromptInjector) removeAnthropicToken(response map[string]interface{}) bool {
	if content, ok := response["content"].([]interface{}); ok {
		modified := false
		for i, item := range content {
			if itemMap, ok := item.(map[string]interface{}); ok {
				if text, ok := itemMap["text"].(string); ok {
					cleaned := pi.cleanTokenFromText(text)
					if cleaned != text {
						itemMap["text"] = cleaned
						content[i] = itemMap
						modified = true
					}
				}
			}
		}
		return modified
	}
	return false
}

// removeGenericToken attempts to remove token with auto-detection
func (pi *PromptInjector) removeGenericToken(response map[string]interface{}) bool {
	// Try different formats
	if pi.removeOpenAIToken(response) {
		return true
	}
	if pi.removeGeminiToken(response) {
		return true
	}
	if pi.removeAnthropicToken(response) {
		return true
	}
	return false
}

// cleanTokenFromText removes the completion token from text content
func (pi *PromptInjector) cleanTokenFromText(text string) string {
	// Remove the completion token and any trailing whitespace
	cleaned := strings.TrimSpace(text)
	
	// Remove the exact token if it appears at the end
	if strings.HasSuffix(cleaned, CompletionToken) {
		cleaned = strings.TrimSuffix(cleaned, CompletionToken)
		cleaned = strings.TrimSpace(cleaned)
		logrus.Debugf("Removed completion token from text. Original length: %d, Cleaned length: %d", len(text), len(cleaned))
	}

	return cleaned
}

// HasCompletionToken checks if the response contains the completion token
func (pi *PromptInjector) HasCompletionToken(responseBody []byte) bool {
	return bytes.Contains(responseBody, []byte(CompletionToken))
}

// IsCompletionTokenOnly checks if the response only contains the completion token (empty response case)
func (pi *PromptInjector) IsCompletionTokenOnly(responseBody []byte) bool {
	bodyStr := strings.TrimSpace(string(responseBody))
	return bodyStr == CompletionToken
}