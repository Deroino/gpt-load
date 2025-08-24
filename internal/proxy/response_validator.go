package proxy

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/sirupsen/logrus"
)

// ValidationResult represents the result of response validation
type ValidationResult struct {
	IsValid      bool
	ErrorType    string
	ErrorMessage string
	ShouldRetry  bool
}

// ResponseValidator handles validation of AI service responses
type ResponseValidator struct {
	punctuationEndCount int // Track consecutive punctuation endings for heuristic
}

// NewResponseValidator creates a new response validator
func NewResponseValidator() *ResponseValidator {
	return &ResponseValidator{
		punctuationEndCount: 0,
	}
}

// ValidateResponse performs comprehensive validation of AI service responses
func (rv *ResponseValidator) ValidateResponse(body []byte, provider string, isStream bool) ValidationResult {
	// Skip validation for streaming responses for now
	if isStream {
		return ValidationResult{IsValid: true}
	}

	// Check for empty response
	if rv.isEmptyResponse(body) {
		logrus.Debug("Empty response detected during validation")
		return ValidationResult{
			IsValid:      false,
			ErrorType:    "EMPTY_RESPONSE",
			ErrorMessage: "AI service returned empty content (completion_tokens: 0)",
			ShouldRetry:  true,
		}
	}

	// Check for blocked content
	if rv.isBlockedResponse(body) {
		logrus.Debug("Blocked content detected during validation")
		return ValidationResult{
			IsValid:      false,
			ErrorType:    "BLOCKED_CONTENT",
			ErrorMessage: "Content was blocked by safety filters",
			ShouldRetry:  true,
		}
	}

	// Check for abnormal finish reasons
	if abnormalReason := rv.getAbnormalFinishReason(body); abnormalReason != "" {
		logrus.Debugf("Abnormal finish reason detected: %s", abnormalReason)
		return ValidationResult{
			IsValid:      false,
			ErrorType:    "ABNORMAL_FINISH",
			ErrorMessage: "Response ended with abnormal reason: " + abnormalReason,
			ShouldRetry:  true,
		}
	}

	// Check for completion token-based incomplete response
	if rv.isIncompleteResponse(body) {
		logrus.Debug("Incomplete response detected (missing completion token)")
		return ValidationResult{
			IsValid:      false,
			ErrorType:    "INCOMPLETE_RESPONSE",
			ErrorMessage: "Response appears incomplete (missing completion token)",
			ShouldRetry:  true,
		}
	}

	// All validations passed
	return ValidationResult{IsValid: true}
}

// ValidateStreamResponse performs validation for streaming responses
func (rv *ResponseValidator) ValidateStreamResponse(body []byte, provider string) ValidationResult {
	bodyStr := string(body)

	// Heuristic for stream completion. Currently focused on OpenAI's `[DONE]` marker.
	// This can be extended for other providers.
	isDone := strings.Contains(bodyStr, "data: [DONE]")

	// Check if there is any actual content in the stream
	hasContent := rv.hasNonEmptyStreamContent(body, provider)

	if !hasContent {
		logrus.Debug("Stream validation failed: No content found.")
		return ValidationResult{
			IsValid:      false,
			ErrorType:    "EMPTY_RESPONSE",
			ErrorMessage: "Stream response contained no meaningful content.",
			ShouldRetry:  true,
		}
	}

	if !isDone {
		logrus.Debug("Stream validation failed: Stream ended prematurely without [DONE] marker.")
		return ValidationResult{
			IsValid:      false,
			ErrorType:    "INCOMPLETE_STREAM",
			ErrorMessage: "Stream ended prematurely without the [DONE] marker.",
			ShouldRetry:  true,
		}
	}

	return ValidationResult{IsValid: true}
}

// hasNonEmptyStreamContent checks if a stream response has content besides control messages.
func (rv *ResponseValidator) hasNonEmptyStreamContent(body []byte, provider string) bool {
	lines := strings.Split(string(body), "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "data:") {
			content := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if content != "" && content != "[DONE]" {
				return true // Found a data line with actual content
			}
		}
	}
	return false
}

// isEmptyResponse checks if the response indicates empty AI content
func (rv *ResponseValidator) isEmptyResponse(body []byte) bool {
	// Check for common patterns indicating empty responses across different AI providers
	
	// OpenAI format: "completion_tokens": 0
	if bytes.Contains(body, []byte(`"completion_tokens":0`)) {
		return true
	}

	// Gemini format: "candidatesTokenCount": 0
	if bytes.Contains(body, []byte(`"candidatesTokenCount":0`)) {
		return true
	}

	// Anthropic format: "output_tokens": 0  
	if bytes.Contains(body, []byte(`"output_tokens":0`)) {
		return true
	}

	// Additional check: parse JSON and look for empty content
	var response map[string]interface{}
	if err := json.Unmarshal(body, &response); err == nil {
		if rv.hasEmptyContent(response) {
			return true
		}
	}

	return false
}

// isBlockedResponse checks if content was blocked by safety filters
func (rv *ResponseValidator) isBlockedResponse(body []byte) bool {
	// Check for blockReason field (Gemini)
	if bytes.Contains(body, []byte("blockReason")) {
		return true
	}

	// Check for content_filter field (OpenAI)
	if bytes.Contains(body, []byte("content_filter")) {
		return true
	}

	// Check for safety-related blocking (Anthropic)
	if bytes.Contains(body, []byte(`"stop_reason":"stop_sequence"`)) &&
		bytes.Contains(body, []byte("safety")) {
		return true
	}

	return false
}

// getAbnormalFinishReason checks for abnormal finish reasons that should trigger retry
func (rv *ResponseValidator) getAbnormalFinishReason(body []byte) string {
	abnormalReasons := []string{
		"SAFETY",
		"RECITATION", 
		"OTHER",
		"ERROR",
		"PROHIBITED_CONTENT",
		"SPII",
		"MALFORMED_FUNCTION_CALL",
	}

	bodyStr := string(body)
	for _, reason := range abnormalReasons {
		// Check finishReason field
		if strings.Contains(bodyStr, `"finishReason":"`+reason+`"`) {
			return reason
		}
		// Check finish_reason field (different format)
		if strings.Contains(bodyStr, `"finish_reason":"`+strings.ToLower(reason)+`"`) {
			return reason
		}
		// Check stop_reason field (Anthropic)
		if strings.Contains(bodyStr, `"stop_reason":"`+strings.ToLower(reason)+`"`) {
			return reason
		}
	}

	return ""
}

// hasEmptyContent checks if the parsed response contains empty content
func (rv *ResponseValidator) hasEmptyContent(response map[string]interface{}) bool {
	// OpenAI format check
	if choices, ok := response["choices"].([]interface{}); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]interface{}); ok {
			if message, ok := choice["message"].(map[string]interface{}); ok {
				if content, ok := message["content"].(string); ok {
					return strings.TrimSpace(content) == ""
				}
			}
		}
	}

	// Gemini format check
	if candidates, ok := response["candidates"].([]interface{}); ok && len(candidates) > 0 {
		if candidate, ok := candidates[0].(map[string]interface{}); ok {
			if content, ok := candidate["content"].(map[string]interface{}); ok {
				if parts, ok := content["parts"].([]interface{}); ok && len(parts) > 0 {
					if part, ok := parts[0].(map[string]interface{}); ok {
						if text, ok := part["text"].(string); ok {
							return strings.TrimSpace(text) == ""
						}
					}
				}
			}
		}
	}

	// Anthropic format check
	if content, ok := response["content"].([]interface{}); ok && len(content) > 0 {
		if contentItem, ok := content[0].(map[string]interface{}); ok {
			if text, ok := contentItem["text"].(string); ok {
				return strings.TrimSpace(text) == ""
			}
		}
	}

	return false
}

// isIncompleteResponse checks if response appears incomplete based on completion token
func (rv *ResponseValidator) isIncompleteResponse(body []byte) bool {
	// This is a simple heuristic - if the response has meaningful content but no completion token,
	// it might be incomplete. We check for finish_reason "stop" without completion token.
	
	bodyStr := string(body)
	
	// Only apply this heuristic if we have a "stop" finish reason
	hasStopReason := strings.Contains(bodyStr, `"finish_reason":"stop"`) ||
		strings.Contains(bodyStr, `"finishReason":"STOP"`)
	
	if !hasStopReason {
		return false
	}
	
	// If we have stop reason but no completion token, it might be incomplete
	hasCompletionToken := bytes.Contains(body, []byte("[done]"))
	
	// Also check if the response has actual content (not just empty)
	hasContent := rv.hasNonEmptyContent(body)
	
	return hasContent && !hasCompletionToken
}

// hasNonEmptyContent checks if the response contains actual content
func (rv *ResponseValidator) hasNonEmptyContent(body []byte) bool {
	var response map[string]interface{}
	if err := json.Unmarshal(body, &response); err != nil {
		return false
	}

	// OpenAI format check
	if choices, ok := response["choices"].([]interface{}); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]interface{}); ok {
			if message, ok := choice["message"].(map[string]interface{}); ok {
				if content, ok := message["content"].(string); ok {
					return strings.TrimSpace(content) != ""
				}
			}
		}
	}

	// Gemini format check
	if candidates, ok := response["candidates"].([]interface{}); ok && len(candidates) > 0 {
		if candidate, ok := candidates[0].(map[string]interface{}); ok {
			if content, ok := candidate["content"].(map[string]interface{}); ok {
				if parts, ok := content["parts"].([]interface{}); ok && len(parts) > 0 {
					for _, part := range parts {
						if partMap, ok := part.(map[string]interface{}); ok {
							if text, ok := partMap["text"].(string); ok && strings.TrimSpace(text) != "" {
								return true
							}
						}
					}
				}
			}
		}
	}

	// Anthropic format check
	if content, ok := response["content"].([]interface{}); ok && len(content) > 0 {
		for _, item := range content {
			if itemMap, ok := item.(map[string]interface{}); ok {
				if text, ok := itemMap["text"].(string); ok && strings.TrimSpace(text) != "" {
					return true
				}
			}
		}
	}

	return false
}

// endsWithSentencePunctuation checks if text ends with sentence-ending punctuation
// Supports Chinese and English punctuation marks
func (rv *ResponseValidator) endsWithSentencePunctuation(text string) bool {
	trimmed := strings.TrimSpace(text)
	if len(trimmed) == 0 {
		return false
	}
	
	runes := []rune(trimmed)
	lastRune := runes[len(runes)-1]
	
	// Common sentence terminators for Chinese and English
	punctuations := []rune{'。', '？', '！', '.', '!', '?', '…'}
	for _, p := range punctuations {
		if lastRune == p {
			return true
		}
	}
	return false
}

// CheckPunctuationHeuristic evaluates if response should be considered complete based on punctuation pattern
// Returns true if we should treat the response as complete despite validation failures
func (rv *ResponseValidator) CheckPunctuationHeuristic(body []byte, provider string, enableHeuristic bool) bool {
	if !enableHeuristic {
		return false
	}
	
	// Extract text content from response
	text := rv.extractResponseText(body, provider)
	if text == "" {
		rv.punctuationEndCount = 0
		return false
	}
	
	// Check if current response ends with punctuation
	if rv.endsWithSentencePunctuation(text) {
		rv.punctuationEndCount++
		logrus.Debugf("Punctuation streak incremented to %d (response ends with sentence punctuation)", rv.punctuationEndCount)
		
		// If we have 3 consecutive punctuation endings, consider it complete
		if rv.punctuationEndCount >= 3 {
			logrus.Info("Punctuation heuristic: treating response as complete due to 3 consecutive punctuation endings")
			rv.punctuationEndCount = 0 // Reset counter
			return true
		}
	} else {
		logrus.Debug("Response does not end with sentence punctuation, resetting punctuation streak")
		rv.punctuationEndCount = 0
	}
	
	return false
}

// extractResponseText extracts text content from response for analysis
func (rv *ResponseValidator) extractResponseText(body []byte, provider string) string {
	var response map[string]interface{}
	if err := json.Unmarshal(body, &response); err != nil {
		return ""
	}

	switch provider {
	case "openai":
		return rv.extractOpenAIText(response)
	case "gemini":
		return rv.extractGeminiText(response)
	case "anthropic":
		return rv.extractAnthropicText(response)
	default:
		return rv.extractGenericText(response)
	}
}

// extractOpenAIText extracts text from OpenAI response format
func (rv *ResponseValidator) extractOpenAIText(response map[string]interface{}) string {
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
func (rv *ResponseValidator) extractGeminiText(response map[string]interface{}) string {
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
func (rv *ResponseValidator) extractAnthropicText(response map[string]interface{}) string {
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
func (rv *ResponseValidator) extractGenericText(response map[string]interface{}) string {
	// Try OpenAI format first
	if text := rv.extractOpenAIText(response); text != "" {
		return text
	}
	
	// Try Gemini format
	if text := rv.extractGeminiText(response); text != "" {
		return text
	}
	
	// Try Anthropic format
	if text := rv.extractAnthropicText(response); text != "" {
		return text
	}
	
	return ""
}