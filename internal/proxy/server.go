// Package proxy provides high-performance OpenAI multi-key proxy server
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"gpt-load/internal/channel"
	"gpt-load/internal/config"
	app_errors "gpt-load/internal/errors"
	"gpt-load/internal/keypool"
	"gpt-load/internal/models"
	"gpt-load/internal/response"
	"gpt-load/internal/services"
	"gpt-load/internal/types"
	"gpt-load/internal/utils"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
)

// ProxyServer represents the proxy server
type ProxyServer struct {
	keyProvider       *keypool.KeyProvider
	groupManager      *services.GroupManager
	settingsManager   *config.SystemSettingsManager
	channelFactory    *channel.Factory
	requestLogService *services.RequestLogService
	responseValidator *ResponseValidator
	contextManager    *ContextManager
	promptInjector    *PromptInjector
}

// NewProxyServer creates a new proxy server
func NewProxyServer(
	keyProvider *keypool.KeyProvider,
	groupManager *services.GroupManager,
	settingsManager *config.SystemSettingsManager,
	channelFactory *channel.Factory,
	requestLogService *services.RequestLogService,
) (*ProxyServer, error) {
	return &ProxyServer{
		keyProvider:       keyProvider,
		groupManager:      groupManager,
		settingsManager:   settingsManager,
		channelFactory:    channelFactory,
		requestLogService: requestLogService,
		responseValidator: NewResponseValidator(),
		contextManager:    NewContextManager(),
		promptInjector:    NewPromptInjector(),
	}, nil
}

// HandleProxy is the main entry point for proxy requests, refactored based on the stable .bak logic.
func (ps *ProxyServer) HandleProxy(c *gin.Context) {
	startTime := time.Now()
	groupName := c.Param("group_name")

	group, err := ps.groupManager.GetGroupByName(groupName)
	if err != nil {
		response.Error(c, app_errors.ParseDBError(err))
		return
	}

	channelHandler, err := ps.channelFactory.GetChannel(group)
	if err != nil {
		response.Error(c, app_errors.NewAPIError(app_errors.ErrInternalServer, fmt.Sprintf("Failed to get channel for group '%s': %v", groupName, err)))
		return
	}

	bodyBytes, err := io.ReadAll(c.Request.Body)
	if err != nil {
		logrus.Errorf("Failed to read request body: %v", err)
		response.Error(c, app_errors.NewAPIError(app_errors.ErrBadRequest, "Failed to read request body"))
		return
	}
	c.Request.Body.Close()

	finalBodyBytes, err := ps.applyParamOverrides(bodyBytes, group)
	if err != nil {
		response.Error(c, app_errors.NewAPIError(app_errors.ErrInternalServer, fmt.Sprintf("Failed to apply parameter overrides: %v", err)))
		return
	}

	// Inject completion verification prompt (if enabled)
	cfg := ps.settingsManager.GetSettings()
	if cfg.EnableCompletionCheck {
		enhancedBodyBytes, err := ps.promptInjector.InjectCompletionPrompt(finalBodyBytes, group.ChannelType)
		if err != nil {
			logrus.Errorf("Failed to inject completion prompt: %v", err)
			enhancedBodyBytes = finalBodyBytes // Fallback to original request
		} else {
			logrus.Debug("Successfully injected completion verification prompt")
		}
		finalBodyBytes = enhancedBodyBytes
	}

	isStream := channelHandler.IsStreamRequest(c, bodyBytes)

	ps.executeRequestWithRetry(c, channelHandler, group, finalBodyBytes, isStream, startTime, 0, nil, "")
}

// executeRequestWithRetry is the core recursive function for handling requests and retries.
func (ps *ProxyServer) executeRequestWithRetry(
	c *gin.Context,
	channelHandler channel.ChannelProxy,
	group *models.Group,
	bodyBytes []byte,
	isStream bool,
	startTime time.Time,
	retryCount int,
	retryErrors []types.RetryError,
	accumulatedText string,
) {
	cfg := group.EffectiveConfig
	if retryCount > cfg.MaxRetries {
		if len(retryErrors) > 0 {
			lastError := retryErrors[len(retryErrors)-1]
			var errorJSON map[string]any
			if err := json.Unmarshal([]byte(lastError.ErrorMessage), &errorJSON); err == nil {
				c.JSON(lastError.StatusCode, errorJSON)
			} else {
				response.Error(c, app_errors.NewAPIErrorWithUpstream(lastError.StatusCode, "UPSTREAM_ERROR", lastError.ErrorMessage))
			}
			logMessage := lastError.ParsedErrorMessage
			if logMessage == "" {
				logMessage = lastError.ErrorMessage
			}
			logrus.Debugf("Max retries exceeded for group %s after %d attempts. Parsed Error: %s", group.Name, retryCount, logMessage)

			ps.logRequest(c, group, &models.APIKey{KeyValue: lastError.KeyValue}, startTime, lastError.StatusCode, retryCount, errors.New(logMessage), isStream, lastError.UpstreamAddr, channelHandler, bodyBytes)
		} else {
			response.Error(c, app_errors.ErrMaxRetriesExceeded)
			logrus.Debugf("Max retries exceeded for group %s after %d attempts.", group.Name, retryCount)
			ps.logRequest(c, group, nil, startTime, http.StatusServiceUnavailable, retryCount, app_errors.ErrMaxRetriesExceeded, isStream, "", channelHandler, bodyBytes)
		}
		return
	}

	apiKey, err := ps.keyProvider.SelectKey(group.ID)
	if err != nil {
		logrus.Errorf("Failed to select a key for group %s on attempt %d: %v", group.Name, retryCount+1, err)
		response.Error(c, app_errors.NewAPIError(app_errors.ErrNoKeysAvailable, err.Error()))
		ps.logRequest(c, group, nil, startTime, http.StatusServiceUnavailable, retryCount, err, isStream, "", channelHandler, bodyBytes)
		return
	}

	upstreamURL, err := channelHandler.BuildUpstreamURL(c.Request.URL, group)
	if err != nil {
		response.Error(c, app_errors.NewAPIError(app_errors.ErrInternalServer, fmt.Sprintf("Failed to build upstream URL: %v", err)))
		return
	}

	var ctx context.Context
	var cancel context.CancelFunc
	if isStream {
		ctx, cancel = context.WithCancel(c.Request.Context())
	} else {
		timeout := time.Duration(cfg.RequestTimeout) * time.Second
		ctx, cancel = context.WithTimeout(c.Request.Context(), timeout)
	}
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, c.Request.Method, upstreamURL, bytes.NewReader(bodyBytes))
	if err != nil {
		logrus.Errorf("Failed to create upstream request: %v", err)
		response.Error(c, app_errors.ErrInternalServer)
		return
	}
	req.ContentLength = int64(len(bodyBytes))

	req.Header = c.Request.Header.Clone()

	// Clean up client auth key
	req.Header.Del("Authorization")
	req.Header.Del("X-Api-Key")
	req.Header.Del("X-Goog-Api-Key")
	q := req.URL.Query()
	q.Del("key")
	req.URL.RawQuery = q.Encode()

	// Apply custom header rules
	if len(group.HeaderRuleList) > 0 {
		headerCtx := utils.NewHeaderVariableContextFromGin(c, group, apiKey)
		utils.ApplyHeaderRules(req, group.HeaderRuleList, headerCtx)
	}

	channelHandler.ModifyRequest(req, apiKey, group)

	var client *http.Client
	if isStream {
		client = channelHandler.GetStreamClient()
		req.Header.Set("X-Accel-Buffering", "no")
	} else {
		client = channelHandler.GetHTTPClient()
	}

	resp, err := client.Do(req)
	if resp != nil {
		defer resp.Body.Close()
	}

	// Unified error handling for retries.
	// Exclude 404 from being a retryable error.
	if err != nil || (resp != nil && resp.StatusCode >= 400 && resp.StatusCode != http.StatusNotFound) {
		if err != nil && app_errors.IsIgnorableError(err) {
			logrus.Debugf("Client-side ignorable error for key %s, aborting retries: %v", utils.MaskAPIKey(apiKey.KeyValue), err)
			ps.logRequest(c, group, apiKey, startTime, 499, retryCount+1, err, isStream, upstreamURL, channelHandler, bodyBytes)
			return
		}

		ps.keyProvider.UpdateStatus(apiKey, group, false)

		var statusCode int
		var errorMessage string
		var parsedError string

		if err != nil {
			statusCode = 500
			errorMessage = err.Error()
			logrus.Debugf("Request failed (attempt %d/%d) for key %s: %v", retryCount+1, cfg.MaxRetries, utils.MaskAPIKey(apiKey.KeyValue), err)
		} else {
			// HTTP-level error (status >= 400)
			statusCode = resp.StatusCode
			errorBody, readErr := io.ReadAll(resp.Body)
			if readErr != nil {
				logrus.Errorf("Failed to read error body: %v", readErr)
				errorBody = []byte("Failed to read error body")
			}

			errorBody = handleGzipCompression(resp, errorBody)
			errorMessage = string(errorBody)
			parsedError = app_errors.ParseUpstreamError(errorBody)
			logrus.Debugf("Request failed with status %d (attempt %d/%d) for key %s. Parsed Error: %s", statusCode, retryCount+1, cfg.MaxRetries, utils.MaskAPIKey(apiKey.KeyValue), parsedError)
		}

		newRetryErrors := append(retryErrors, types.RetryError{
			StatusCode:         statusCode,
			ErrorMessage:       errorMessage,
			ParsedErrorMessage: parsedError,
			KeyValue:           apiKey.KeyValue,
			Attempt:            retryCount + 1,
			UpstreamAddr:       upstreamURL,
		})
		ps.executeRequestWithRetry(c, channelHandler, group, bodyBytes, isStream, startTime, retryCount+1, newRetryErrors, accumulatedText)
		return
	}

	// Validate response before considering it successful
	if !isStream {
		bodyBytes, err := io.ReadAll(resp.Body)
		if err != nil {
			logrus.Errorf("Failed to read response body for validation: %v", err)
		} else {
			resp.Body.Close()
			resp.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			
			// Use ResponseValidator for comprehensive validation
			cfg := ps.settingsManager.GetSettings()
			
			if cfg.EnableAdvancedRetry {
				validationResult := ps.responseValidator.ValidateResponse(bodyBytes, group.ChannelType, isStream)
				
				if !validationResult.IsValid && validationResult.ShouldRetry {
					// Check punctuation heuristic before retrying
					if ps.responseValidator.CheckPunctuationHeuristic(bodyBytes, group.ChannelType, cfg.EnablePunctuationHeuristic) {
						logrus.Debug("Punctuation heuristic override: treating response as complete despite validation failure")
					} else {
						logrus.Debugf("%s detected for key %s on attempt %d/%d. Triggering retry.", 
							validationResult.ErrorType, utils.MaskAPIKey(apiKey.KeyValue), retryCount+1, cfg.MaxRetries)
						ps.keyProvider.UpdateStatus(apiKey, group, false)
						
						// Extract accumulated text from current response (if context retry enabled)
						var newAccumulatedText string
						var retryBodyBytes []byte
						
						if cfg.EnableContextRetry {
							currentText := ps.contextManager.ExtractAccumulatedText(bodyBytes, group.ChannelType)
							newAccumulatedText = accumulatedText + currentText
							
							// Check accumulated text length limit
							if cfg.MaxAccumulatedChars > 0 && len(newAccumulatedText) > cfg.MaxAccumulatedChars {
								logrus.Warnf("Accumulated text length (%d) exceeds limit (%d), truncating", len(newAccumulatedText), cfg.MaxAccumulatedChars)
								runes := []rune(newAccumulatedText)
								if len(runes) > cfg.MaxAccumulatedChars {
									newAccumulatedText = string(runes[:cfg.MaxAccumulatedChars])
								}
							}
							
							// Build retry request with accumulated context
							if strings.TrimSpace(newAccumulatedText) != "" {
								var err error
								retryBodyBytes, err = ps.contextManager.BuildRetryRequest(bodyBytes, newAccumulatedText, group.ChannelType)
								if err != nil {
									logrus.Errorf("Failed to build retry request with context: %v", err)
									retryBodyBytes = bodyBytes // Fallback to original request
								} else {
									logrus.Debugf("Built retry request with accumulated text length: %d", len(newAccumulatedText))
								}
							} else {
								retryBodyBytes = bodyBytes // No accumulated text, use original request
							}
						} else {
							newAccumulatedText = accumulatedText
							retryBodyBytes = bodyBytes
						}
						
						newRetryErrors := append(retryErrors, types.RetryError{
							StatusCode:         500,
							ErrorMessage:       validationResult.ErrorMessage,
							ParsedErrorMessage: validationResult.ErrorType + ": " + validationResult.ErrorMessage,
							KeyValue:           apiKey.KeyValue,
							Attempt:            retryCount + 1,
							UpstreamAddr:       upstreamURL,
						})
						ps.executeRequestWithRetry(c, channelHandler, group, retryBodyBytes, isStream, startTime, retryCount+1, newRetryErrors, newAccumulatedText)
						return
					}
				}
			}
		}
	}

	// ps.keyProvider.UpdateStatus(apiKey, group, true) // 请求成功不再重置成功次数，减少IO消耗
	logrus.Debugf("Request for group %s succeeded on attempt %d with key %s", group.Name, retryCount+1, utils.MaskAPIKey(apiKey.KeyValue))
	ps.logRequest(c, group, apiKey, startTime, resp.StatusCode, retryCount+1, nil, isStream, upstreamURL, channelHandler, bodyBytes)

	for key, values := range resp.Header {
		for _, value := range values {
			c.Header(key, value)
		}
	}
	c.Status(resp.StatusCode)

	if isStream {
		cfg := ps.settingsManager.GetSettings()
		validationResult, responseBody := ps.handleStreamingResponse(c, resp, group, &cfg)

		if cfg.EnableAdvancedRetry && !validationResult.IsValid && validationResult.ShouldRetry {
			logrus.Debugf("Stream validation failed for key %s on attempt %d/%d. Error: %s. Triggering retry.",
				utils.MaskAPIKey(apiKey.KeyValue), retryCount+1, group.EffectiveConfig.MaxRetries, validationResult.ErrorMessage)

			ps.keyProvider.UpdateStatus(apiKey, group, false)

			newRetryErrors := append(retryErrors, types.RetryError{
				StatusCode:         500, // Internal error for validation failure
				ErrorMessage:       validationResult.ErrorMessage,
				ParsedErrorMessage: validationResult.ErrorType + ": " + validationResult.ErrorMessage,
				KeyValue:           apiKey.KeyValue,
				Attempt:            retryCount + 1,
				UpstreamAddr:       upstreamURL,
			})

			// Retry with the original request body, context accumulation for streams is not yet supported.
			ps.executeRequestWithRetry(c, channelHandler, group, bodyBytes, isStream, startTime, retryCount+1, newRetryErrors, "")
			return
		}
		// If validation is successful, write the buffered response to the client.
		if _, err := c.Writer.Write(responseBody); err != nil {
			logrus.Errorf("Error writing buffered stream to client: %v", err)
		}
	} else {
		ps.handleNormalResponse(c, resp, group.ChannelType)
	}
}

// logRequest is a helper function to create and record a request log.
func (ps *ProxyServer) logRequest(
	c *gin.Context,
	group *models.Group,
	apiKey *models.APIKey,
	startTime time.Time,
	statusCode int,
	retries int,
	finalError error,
	isStream bool,
	upstreamAddr string,
	channelHandler channel.ChannelProxy,
	bodyBytes []byte,
) {
	if ps.requestLogService == nil {
		return
	}

	duration := time.Since(startTime).Milliseconds()

	logEntry := &models.RequestLog{
		GroupID:      group.ID,
		GroupName:    group.Name,
		IsSuccess:    finalError == nil && statusCode < 400,
		SourceIP:     c.ClientIP(),
		StatusCode:   statusCode,
		RequestPath:  utils.TruncateString(c.Request.URL.String(), 500),
		Duration:     duration,
		UserAgent:    c.Request.UserAgent(),
		Retries:      retries,
		IsStream:     isStream,
		UpstreamAddr: utils.TruncateString(upstreamAddr, 500),
	}

	if channelHandler != nil && bodyBytes != nil {
		logEntry.Model = channelHandler.ExtractModel(c, bodyBytes)
	}

	if apiKey != nil {
		logEntry.KeyValue = apiKey.KeyValue
	}

	if finalError != nil {
		logEntry.ErrorMessage = finalError.Error()
	}

	if err := ps.requestLogService.Record(logEntry); err != nil {
		logrus.Errorf("Failed to record request log: %v", err)
	}
}
