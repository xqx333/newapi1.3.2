package controller

import (
	"bufio"
	"bytes"
	"encoding/json"
	"github.com/gin-gonic/gin"
	"io"
	"net/http"
	"one-api/common"
	"strings"
	"sync"
	"time"
)

func openaiStreamHandler(c *gin.Context, resp *http.Response, relayMode int) (*OpenAIErrorWithStatusCode, string) {
	var responseTextBuilder strings.Builder
	scanner := bufio.NewScanner(resp.Body)
	scanner.Split(func(data []byte, atEOF bool) (advance int, token []byte, err error) {
		if atEOF && len(data) == 0 {
			return 0, nil, nil
		}
		if i := strings.Index(string(data), "\n"); i >= 0 {
			return i + 1, data[0:i], nil
		}
		if atEOF {
			return len(data), data, nil
		}
		return 0, nil, nil
	})
	dataChan := make(chan string, 5)
	stopChan := make(chan bool, 2)
	defer close(stopChan)
	defer close(dataChan)
	var wg sync.WaitGroup

	go func() {
		wg.Add(1)
		defer wg.Done()
		var streamItems []string
		for scanner.Scan() {
			data := scanner.Text()
			if len(data) < 6 { // ignore blank line or wrong format
				continue
			}
			if data[:6] != "data: " && data[:6] != "[DONE]" {
				continue
			}
			dataChan <- data
			data = data[6:]
			if !strings.HasPrefix(data, "[DONE]") {
				streamItems = append(streamItems, data)
			}
		}
		streamResp := "[" + strings.Join(streamItems, ",") + "]"
		switch relayMode {
		case RelayModeChatCompletions:
			var streamResponses []ChatCompletionsStreamResponseSimple
			err := json.Unmarshal(common.StringToByteSlice(streamResp), &streamResponses)
			if err != nil {
				common.SysError("error unmarshalling stream response: " + err.Error())
				return // just ignore the error
			}
			for _, streamResponse := range streamResponses {
				for _, choice := range streamResponse.Choices {
					responseTextBuilder.WriteString(choice.Delta.Content)
				}
			}
		case RelayModeCompletions:
			var streamResponses []CompletionsStreamResponse
			err := json.Unmarshal(common.StringToByteSlice(streamResp), &streamResponses)
			if err != nil {
				common.SysError("error unmarshalling stream response: " + err.Error())
				return // just ignore the error
			}
			for _, streamResponse := range streamResponses {
				for _, choice := range streamResponse.Choices {
					responseTextBuilder.WriteString(choice.Text)
				}
			}
		}
		if len(dataChan) > 0 {
			// wait data out
			time.Sleep(2 * time.Second)
		}
		common.SafeSend(stopChan, true)
	}()
	setEventStreamHeaders(c)
	c.Stream(func(w io.Writer) bool {
		select {
		case data := <-dataChan:
			if strings.HasPrefix(data, "data: [DONE]") {
				data = data[:12]
			}
			// some implementations may add \r at the end of data
			data = strings.TrimSuffix(data, "\r")
			c.Render(-1, common.CustomEvent{Data: data})
			return true
		case <-stopChan:
			return false
		}
	})
	err := resp.Body.Close()
	if err != nil {
		return errorWrapper(err, "close_response_body_failed", http.StatusInternalServerError), ""
	}
	wg.Wait()
	return nil, responseTextBuilder.String()
}

func openaiHandler(c *gin.Context, resp *http.Response, promptTokens int, model string) (*OpenAIErrorWithStatusCode, *Usage) {
	var textResponse TextResponse
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return errorWrapper(err, "read_response_body_failed", http.StatusInternalServerError), nil
	}
	err = resp.Body.Close()
	if err != nil {
		return errorWrapper(err, "close_response_body_failed", http.StatusInternalServerError), nil
	}
	err = json.Unmarshal(responseBody, &textResponse)
	if err != nil {
		return errorWrapper(err, "unmarshal_response_body_failed", http.StatusInternalServerError), nil
	}
	if textResponse.Error.Type != "" {
		return &OpenAIErrorWithStatusCode{
			OpenAIError: textResponse.Error,
			StatusCode:  resp.StatusCode,
		}, nil
	}
	// Reset response body
	resp.Body = io.NopCloser(bytes.NewBuffer(responseBody))
	// We shouldn't set the header before we parse the response body, because the parse part may fail.
	// And then we will have to send an error response, but in this case, the header has already been set.
	// So the httpClient will be confused by the response.
	// For example, Postman will report error, and we cannot check the response at all.
	for k, v := range resp.Header {
		c.Writer.Header().Set(k, v[0])
	}
	c.Writer.WriteHeader(resp.StatusCode)
	_, err = io.Copy(c.Writer, resp.Body)
	if err != nil {
		return errorWrapper(err, "copy_response_body_failed", http.StatusInternalServerError), nil
	}
	err = resp.Body.Close()
	if err != nil {
		return errorWrapper(err, "close_response_body_failed", http.StatusInternalServerError), nil
	}

	if textResponse.Usage.TotalTokens == 0 {
		completionTokens := 0
		for _, choice := range textResponse.Choices {
			completionTokens += countTokenText(string(choice.Message.Content), model)
		}
		textResponse.Usage = Usage{
			PromptTokens:     promptTokens,
			CompletionTokens: completionTokens,
			TotalTokens:      promptTokens + completionTokens,
		}
	}
	return nil, &textResponse.Usage
}
