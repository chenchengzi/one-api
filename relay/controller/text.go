package controller

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/logger"
	"github.com/songquanpeng/one-api/relay/channel/openai"
	"github.com/songquanpeng/one-api/relay/constant"
	"github.com/songquanpeng/one-api/relay/helper"
	"github.com/songquanpeng/one-api/relay/model"
	"github.com/songquanpeng/one-api/relay/util"
	"io"
	"net/http"
	"strings"
)

func RelayTextHelper(c *gin.Context) *model.ErrorWithStatusCode {
	ctx := c.Request.Context()
	meta := util.GetRelayMeta(c)
	// get & validate textRequest
	textRequest, err := getAndValidateTextRequest(c, meta.Mode)
	if err != nil {
		logger.Errorf(ctx, "getAndValidateTextRequest failed: %s", err.Error())
		return openai.ErrorWrapper(err, "invalid_text_request", http.StatusBadRequest)
	}
	meta.IsStream = textRequest.Stream

	// map model name
	var isModelMapped bool
	meta.OriginModelName = textRequest.Model
	textRequest.Model, isModelMapped = util.GetMappedModelName(textRequest.Model, meta.ModelMapping)
	meta.ActualModelName = textRequest.Model
	// get model ratio & group ratio
	modelRatio := common.GetModelRatio(textRequest.Model)
	groupRatio := common.GetGroupRatio(meta.Group)
	ratio := modelRatio * groupRatio
	// pre-consume quota
	promptTokens := getPromptTokens(textRequest, meta.Mode)
	meta.PromptTokens = promptTokens
	preConsumedQuota, bizErr := preConsumeQuota(ctx, textRequest, promptTokens, ratio, meta)
	if bizErr != nil {
		logger.Warnf(ctx, "preConsumeQuota failed: %+v", *bizErr)
		return bizErr
	}

	adaptor := helper.GetAdaptor(meta.APIType)
	if adaptor == nil {
		return openai.ErrorWrapper(fmt.Errorf("invalid api type: %d", meta.APIType), "invalid_api_type", http.StatusBadRequest)
	}

	//
	// 定义JSON数据结构
	type Message struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	type Data struct {
		Model     string    `json:"model"`
		Messages  []Message `json:"messages"`
		Stream    bool      `json:"stream"`
		Temperature float64 `json:"temperature"`
		TopP      float64   `json:"top_p"`
	}
	
	jsonStr, err := json.Marshal(textRequest)
	
	if err == nil {
		var data Data
		// 解析JSON数据
		if err := json.Unmarshal([]byte(jsonStr), &data); err != nil {
			fmt.Println("Error parsing JSON:", err)
		}
		var lastUserContent string
		for _, message := range data.Messages {
			if message.Role == "user" {
				lastUserContent = message.Content
			}
		}
		logger.Info(ctx,fmt.Sprintf("message:%s",lastUserContent))
	}



	// get request body
	var requestBody io.Reader
	if meta.APIType == constant.APITypeOpenAI {
		// no need to convert request for openai
		if isModelMapped {
			jsonStr, err := json.Marshal(textRequest)
			if err != nil {
				return openai.ErrorWrapper(err, "json_marshal_failed", http.StatusInternalServerError)
			}
			requestBody = bytes.NewBuffer(jsonStr)
		} else {
			requestBody = c.Request.Body
		}
	} else {
		convertedRequest, err := adaptor.ConvertRequest(c, meta.Mode, textRequest)
		if err != nil {
			return openai.ErrorWrapper(err, "convert_request_failed", http.StatusInternalServerError)
		}
		jsonData, err := json.Marshal(convertedRequest)
		if err != nil {
			return openai.ErrorWrapper(err, "json_marshal_failed", http.StatusInternalServerError)
		}
		requestBody = bytes.NewBuffer(jsonData)
	}

	// do request
	resp, err := adaptor.DoRequest(c, meta, requestBody)
	if err != nil {
		logger.Errorf(ctx, "DoRequest failed: %s", err.Error())
		return openai.ErrorWrapper(err, "do_request_failed", http.StatusInternalServerError)
	}





	
	respBody, err := io.ReadAll(resp.Body)
	
	logger.SysLog(ctx,fmt.Sprintf("response: \n%s",string(respBody)))

	// // 定义响应结构体以匹配OpenAI API的响应格式
	// type OpenAIResponse struct {
	// 	ID      string `json:"id"`
	// 	Object  string `json:"object"`
	// 	Created int64  `json:"created"`
	// 	Model   string `json:"model"`
	// 	Choices []struct {
	// 		Text         string      `json:"text"`
	// 		Index        int         `json:"index"`
	// 		Logprobs     interface{} `json:"logprobs"` // 根据需要，这里可以是更具体的类型或者留作interface{}
	// 		FinishReason string      `json:"finish_reason"`
	// 	} `json:"choices"`
	// }
	// // 读取响应体
	
	// defer resp.Body.Close() // 确保关闭resp.Body
	// // 解析响应体
    // var response OpenAIResponse
    // if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
    //     logger.Info(ctx,"解析 响应error")
    // }
	// if len(response.Choices) > 0 {
	// 	logger.Info(ctx,fmt.Sprintf("response:%s",response.Choices[0].Text))
    // } else {
	// 	logger.Info(ctx,"No response")
    // }

	meta.IsStream = meta.IsStream || strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream")
	if resp.StatusCode != http.StatusOK {
		util.ReturnPreConsumedQuota(ctx, preConsumedQuota, meta.TokenId)
		return util.RelayErrorHandler(resp)
	}

	// do response
	usage, respErr := adaptor.DoResponse(c, resp, meta)
	if respErr != nil {
		logger.Errorf(ctx, "respErr is not nil: %+v", respErr)
		util.ReturnPreConsumedQuota(ctx, preConsumedQuota, meta.TokenId)
		return respErr
	}
	// post-consume quota
	go postConsumeQuota(ctx, usage, meta, textRequest, ratio, preConsumedQuota, modelRatio, groupRatio)
	return nil
}
