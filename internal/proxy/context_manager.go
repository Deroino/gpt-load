package proxy

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/sirupsen/logrus"
)

// ContextManager handles building retry requests with accumulated context
type ContextManager struct{}

// NewContextManager creates a new context manager
func NewContextManager() *ContextManager {
	return &ContextManager{}
}

// BuildRetryRequest builds a new request body for retry with accumulated context
func (cm *ContextManager) BuildRetryRequest(originalBody []byte, accumulatedText string, provider string) ([]byte, error) {
	if strings.TrimSpace(accumulatedText) == "" {
		// No accumulated text, return original request
		return originalBody, nil
	}

	logrus.Debugf("Building retry request with accumulated text length: %d", len(accumulatedText))

	var originalRequest map[string]interface{}
	if err := json.Unmarshal(originalBody, &originalRequest); err != nil {
		return nil, fmt.Errorf("failed to parse original request: %w", err)
	}

	// Handle different provider formats
	switch provider {
	case "openai":
		return cm.buildOpenAIRetryRequest(originalRequest, accumulatedText)
	case "gemini":
		return cm.buildGeminiRetryRequest(originalRequest, accumulatedText)
	case "anthropic":
		return cm.buildAnthropicRetryRequest(originalRequest, accumulatedText)
	default:
		// Try to auto-detect format and fallback
		return cm.buildGenericRetryRequest(originalRequest, accumulatedText)
	}
}

// buildOpenAIRetryRequest builds retry request for OpenAI format
func (cm *ContextManager) buildOpenAIRetryRequest(originalRequest map[string]interface{}, accumulatedText string) ([]byte, error) {
	retryRequest := make(map[string]interface{})
	for k, v := range originalRequest {
		retryRequest[k] = v
	}

	messages, ok := retryRequest["messages"].([]interface{})
	if !ok {
		return json.Marshal(retryRequest)
	}

	// Add accumulated context
	contextMessages := []interface{}{
		map[string]interface{}{
			"role":    "assistant",
			"content": accumulatedText,
		},
		map[string]interface{}{
			"role":    "user", 
			"content": "Continue exactly where you left off without any preamble or repetition.",
		},
	}

	// Insert context after the last user message
	newMessages := cm.insertContextAfterLastUser(messages, contextMessages)
	retryRequest["messages"] = newMessages

	logrus.Debugf("OpenAI retry request built with %d messages", len(newMessages))
	return json.Marshal(retryRequest)
}

// buildGeminiRetryRequest builds retry request for Gemini format
func (cm *ContextManager) buildGeminiRetryRequest(originalRequest map[string]interface{}, accumulatedText string) ([]byte, error) {
	retryRequest := make(map[string]interface{})
	for k, v := range originalRequest {
		retryRequest[k] = v
	}

	contents, ok := retryRequest["contents"].([]interface{})
	if !ok {
		contents = []interface{}{}
	}

	// Add accumulated context in Gemini format
	contextMessages := []interface{}{
		map[string]interface{}{
			"role": "model",
			"parts": []interface{}{
				map[string]interface{}{"text": accumulatedText},
			},
		},
		map[string]interface{}{
			"role": "user",
			"parts": []interface{}{
				map[string]interface{}{"text": "Continue exactly where you left off without any preamble or repetition."},
			},
		},
	}

	// Insert context after the last user message
	newContents := cm.insertContextAfterLastUser(contents, contextMessages)
	retryRequest["contents"] = newContents

	logrus.Debugf("Gemini retry request built with %d contents", len(newContents))
	return json.Marshal(retryRequest)
}

// buildAnthropicRetryRequest builds retry request for Anthropic format
func (cm *ContextManager) buildAnthropicRetryRequest(originalRequest map[string]interface{}, accumulatedText string) ([]byte, error) {
	retryRequest := make(map[string]interface{})
	for k, v := range originalRequest {
		retryRequest[k] = v
	}

	messages, ok := retryRequest["messages"].([]interface{})
	if !ok {
		return json.Marshal(retryRequest)
	}

	// Add accumulated context
	contextMessages := []interface{}{
		map[string]interface{}{
			"role": "assistant",
			"content": []interface{}{
				map[string]interface{}{
					"type": "text",
					"text": accumulatedText,
				},
			},
		},
		map[string]interface{}{
			"role": "user",
			"content": []interface{}{
				map[string]interface{}{
					"type": "text", 
					"text": "Continue exactly where you left off without any preamble or repetition.",
				},
			},
		},
	}

	// Insert context after the last user message
	newMessages := cm.insertContextAfterLastUser(messages, contextMessages)
	retryRequest["messages"] = newMessages

	logrus.Debugf("Anthropic retry request built with %d messages", len(newMessages))
	return json.Marshal(retryRequest)
}

// buildGenericRetryRequest builds retry request with auto-detection
func (cm *ContextManager) buildGenericRetryRequest(originalRequest map[string]interface{}, accumulatedText string) ([]byte, error) {
	// Try to detect format based on structure
	if _, hasMessages := originalRequest["messages"]; hasMessages {
		// Looks like OpenAI or Anthropic format
		return cm.buildOpenAIRetryRequest(originalRequest, accumulatedText)
	} else if _, hasContents := originalRequest["contents"]; hasContents {
		// Looks like Gemini format
		return cm.buildGeminiRetryRequest(originalRequest, accumulatedText)
	}

	// Fallback: return original request
	logrus.Warn("Unknown request format, returning original request without context")
	return json.Marshal(originalRequest)
}

// insertContextAfterLastUser inserts context messages after the last user message
func (cm *ContextManager) insertContextAfterLastUser(messages []interface{}, contextMessages []interface{}) []interface{} {
	// Find last user message index
	lastUserIndex := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if message, ok := messages[i].(map[string]interface{}); ok {
			if role, ok := message["role"].(string); ok && role == "user" {
				lastUserIndex = i
				break
			}
		}
	}

	// Insert context messages
	if lastUserIndex != -1 {
		newMessages := make([]interface{}, 0, len(messages)+len(contextMessages))
		newMessages = append(newMessages, messages[:lastUserIndex+1]...)
		newMessages = append(newMessages, contextMessages...)
		newMessages = append(newMessages, messages[lastUserIndex+1:]...)
		logrus.Debugf("Inserted context after user message at index %d", lastUserIndex)
		return newMessages
	} else {
		// No user message found, append context to end
		newMessages := append(messages, contextMessages...)
		logrus.Debug("Appended context to end of conversation")
		return newMessages
	}
}

// ExtractAccumulatedText extracts accumulated text from response body for context building
func (cm *ContextManager) ExtractAccumulatedText(responseBody []byte, provider string) string {
	var response map[string]interface{}
	if err := json.Unmarshal(responseBody, &response); err != nil {
		logrus.Debugf("Failed to parse response for text extraction: %v", err)
		return ""
	}

	switch provider {
	case "openai":
		return cm.extractOpenAIText(response)
	case "gemini":
		return cm.extractGeminiText(response)
	case "anthropic":
		return cm.extractAnthropicText(response)
	default:
		return cm.extractGenericText(response)
	}
}

// extractOpenAIText extracts text from OpenAI response format
func (cm *ContextManager) extractOpenAIText(response map[string]interface{}) string {
	if choices, ok := response["choices"].([]interface{}); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]interface{}); ok {
			if message, ok := choice["message"].(map[string]interface{}); ok {
				if content, ok := message["content"].(string); ok {
					return content
				}
			}
		}
	}
	return ""
}

// extractGeminiText extracts text from Gemini response format
func (cm *ContextManager) extractGeminiText(response map[string]interface{}) string {
	if candidates, ok := response["candidates"].([]interface{}); ok && len(candidates) > 0 {
		if candidate, ok := candidates[0].(map[string]interface{}); ok {
			if content, ok := candidate["content"].(map[string]interface{}); ok {
				if parts, ok := content["parts"].([]interface{}); ok {
					var text strings.Builder
					for _, part := range parts {
						if partMap, ok := part.(map[string]interface{}); ok {
							if partText, ok := partMap["text"].(string); ok {
								text.WriteString(partText)
							}
						}
					}
					return text.String()
				}
			}
		}
	}
	return ""
}

// extractAnthropicText extracts text from Anthropic response format
func (cm *ContextManager) extractAnthropicText(response map[string]interface{}) string {
	if content, ok := response["content"].([]interface{}); ok {
		var text strings.Builder
		for _, item := range content {
			if itemMap, ok := item.(map[string]interface{}); ok {
				if itemText, ok := itemMap["text"].(string); ok {
					text.WriteString(itemText)
				}
			}
		}
		return text.String()
	}
	return ""
}

// extractGenericText attempts to extract text with auto-detection
func (cm *ContextManager) extractGenericText(response map[string]interface{}) string {
	// Try OpenAI format first
	if text := cm.extractOpenAIText(response); text != "" {
		return text
	}
	
	// Try Gemini format
	if text := cm.extractGeminiText(response); text != "" {
		return text
	}
	
	// Try Anthropic format
	if text := cm.extractAnthropicText(response); text != "" {
		return text
	}
	
	return ""
}