package proxy

import (
	"bytes"
	"io"
	"net/http"

	"gpt-load/internal/models"
	"gpt-load/internal/types"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
)

func (ps *ProxyServer) handleStreamingResponse(c *gin.Context, resp *http.Response, group *models.Group, cfg *types.SystemSettings) (ValidationResult, []byte) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		logrus.Error("Streaming unsupported by the writer, falling back to normal response")
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			logUpstreamError("reading response body for fallback", err)
			return ValidationResult{IsValid: false, ErrorType: "STREAM_ERROR", ErrorMessage: "Fallback failed", ShouldRetry: true}, nil
		}
		if _, err := c.Writer.Write(body); err != nil {
			logUpstreamError("copying response body for fallback", err)
		}
		return ValidationResult{IsValid: true}, body
	}

	// Buffer the entire stream first for validation
	var accumulatedBody bytes.Buffer
	buf := make([]byte, 4*1024)
	
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			accumulatedBody.Write(buf[:n])
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			logUpstreamError("reading from upstream", err)
			return ValidationResult{
				IsValid:      false,
				ErrorType:    "STREAM_ERROR",
				ErrorMessage: err.Error(),
				ShouldRetry:  true,
			}, accumulatedBody.Bytes()
		}
	}

	finalBody := accumulatedBody.Bytes()
	
	// Validate the complete stream if enabled
	var validationResult ValidationResult
	if cfg.EnableAdvancedRetry {
		validationResult = ps.responseValidator.ValidateStreamResponseWithConfig(finalBody, group.ChannelType, cfg.EnableCompletionCheck)
	} else {
		validationResult = ValidationResult{IsValid: true}
	}
	
	// If validation passed, clean the response and send to client
	if validationResult.IsValid {
		cleanedBody := finalBody
		if cfg.EnableCompletionCheck {
			cleanedBody = ps.promptInjector.RemoveCompletionToken(finalBody, group.ChannelType)
		}
		
		// Write cleaned stream to client
		if _, err := c.Writer.Write(cleanedBody); err != nil {
			logUpstreamError("writing cleaned stream to client", err)
			return ValidationResult{
				IsValid:      false,
				ErrorType:    "STREAM_WRITE_ERROR",
				ErrorMessage: err.Error(),
				ShouldRetry:  false, // Don't retry write errors
			}, finalBody
		}
		flusher.Flush()
	}
	
	return validationResult, finalBody
}

func (ps *ProxyServer) handleNormalResponse(c *gin.Context, resp *http.Response, groupType string) {
	// Read response body for completion token removal
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		logUpstreamError("reading response body for token removal", err)
		return
	}
	resp.Body.Close()

	// Remove completion token if completion check is enabled
	cleanedBody := body
	cfg := ps.settingsManager.GetSettings()
	if cfg.EnableCompletionCheck {
		cleanedBody = ps.promptInjector.RemoveCompletionToken(body, groupType)
	}

	// Write cleaned response to client
	if _, writeErr := c.Writer.Write(cleanedBody); writeErr != nil {
		logUpstreamError("writing cleaned response to client", writeErr)
	}
}
