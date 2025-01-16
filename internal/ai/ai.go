package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/duaraghav8/dockershrink/internal/ai/promptcreator"
	"github.com/duaraghav8/dockershrink/internal/log"
	"github.com/openai/openai-go"
)

const (
	OpenAIPreferredModel = openai.ChatModelGPT4o2024_11_20
	MaxLLMCalls          = 5
)

const ToolReadFiles = "read_files"

type AIService struct {
	L      *log.Logger
	client *openai.Client
}

func NewAIService(logger *log.Logger, client *openai.Client) *AIService {
	return &AIService{
		L:      logger,
		client: client,
	}
}

// OptimizeDockerfile optimizes the given Dockerfile using OpenAI GPT-4o
// It returns the optimized Dockerfile along with the actions taken and
// recommendations for further optimization.
func (ai *AIService) OptimizeDockerfile(req *OptimizeRequest) (*OptimizeResponse, error) {
	systemInstructions, err := ai.constructSystemInstructions(req)
	if err != nil {
		return nil, fmt.Errorf("failed to construct system prompt: %w", err)
	}
	userQuery, err := ai.constructUserQuery(req)
	if err != nil {
		return nil, fmt.Errorf("failed to construct user prompt: %w", err)
	}

	ai.L.Debug("System instructions", map[string]interface{}{"content": systemInstructions})
	ai.L.Debug("User query", map[string]interface{}{"content": userQuery})

	messages := []openai.ChatCompletionMessageParamUnion{
		openai.SystemMessage(systemInstructions),
		openai.UserMessage(userQuery),
	}
	responseFormat := openai.ResponseFormatJSONSchemaJSONSchemaParam{
		Name:        openai.F("modifications"),
		Description: openai.F("Optimized assets for the project along with the actions taken and further recommendations"),
		Schema:      openai.F(optimizeResponseSchema),
		Strict:      openai.Bool(true),
	}
	availableTools := []openai.ChatCompletionToolParam{
		{
			Type: openai.F(openai.ChatCompletionToolTypeFunction),
			Function: openai.F(openai.FunctionDefinitionParam{
				Name:        openai.String(ToolReadFiles),
				Description: openai.String("Read the contents of specific files inside the project"),
				Parameters: openai.F(openai.FunctionParameters{
					"type": "object",
					"properties": map[string]interface{}{
						"filepaths": map[string]interface{}{
							"type":        "array",
							"items":       map[string]interface{}{"type": "string"},
							"description": "List of files to read. Each item in the array is a file path relative to the project root directory.",
						},
					},
					"required": []string{"filepaths"},
				}),
			}),
		},
	}
	// TODO: Enable "get_documentation" tool call

	params := openai.ChatCompletionNewParams{
		Messages: openai.F(messages),
		Tools:    openai.F(availableTools),
		ResponseFormat: openai.F[openai.ChatCompletionNewParamsResponseFormatUnion](
			openai.ResponseFormatJSONSchemaParam{
				Type:       openai.F(openai.ResponseFormatJSONSchemaTypeJSONSchema),
				JSONSchema: openai.F(responseFormat),
			},
		),
		Model: openai.F(OpenAIPreferredModel),
	}

	for i := 0; i < MaxLLMCalls; i++ {
		ai.L.Debug("Calling LLM for optimization", map[string]interface{}{"attempt": i + 1})

		response, err := ai.client.Chat.Completions.New(context.Background(), params)
		if err != nil {
			return nil, fmt.Errorf("failed to get chat completion: %w", err)
		}

		ai.L.Debug("Received response", map[string]interface{}{
			"content":   response.Choices[0].Message.Content,
			"toolCalls": response.Choices[0].Message.ToolCalls,
			"json":      response.Choices[0].Message.JSON,
		})

		toolCalls := response.Choices[0].Message.ToolCalls
		if len(toolCalls) == 0 {
			ai.L.Debug("Received final response", nil)
			// no tool calls, the optimized Dockerfile has been returned by the LLM
			optimizeResponse := OptimizeResponse{}
			err = json.Unmarshal([]byte(response.Choices[0].Message.Content), &optimizeResponse)
			if err != nil {
				return nil, fmt.Errorf("failed to parse final response from LLM: %w", err)
			}

			ai.L.Debug("Response", map[string]interface{}{
				"dockerfile":      optimizeResponse.Dockerfile,
				"actions":         optimizeResponse.ActionsTaken,
				"recommendations": optimizeResponse.Recommendations,
			})

			return &optimizeResponse, nil
		} else {

			ai.L.Debug("Tool call", map[string]interface{}{
				"message": response.Choices[0].Message.Content,
			})

			// add the tool call message back to the ongoing conversation with LLM
			params.Messages.Value = append(params.Messages.Value, response.Choices[0].Message)

			for _, toolCall := range toolCalls {
				if toolCall.Function.Name == ToolReadFiles {
					var extractedParams struct {
						FilePaths []string `json:"filepaths"`
					}
					if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &extractedParams); err != nil {
						return nil, fmt.Errorf("failed to parse function call arguments (%s) from LLM: %w", toolCall.Function.Arguments, err)
					}

					ai.L.Debug("read files", map[string]interface{}{
						"filepaths": extractedParams.FilePaths,
					})

					projectFiles, err := req.ProjectDirectory.ReadFiles(extractedParams.FilePaths)
					if err != nil {
						return nil, fmt.Errorf("failed to read files from the project requested by LLM: %w", err)
					}

					responsePrompt := "Here are the files you requested:\n"
					for path, content := range projectFiles {
						var filePrompt string

						if len(strings.TrimSpace(content)) == 0 {
							filePrompt = fmt.Sprintf("%s\n[File is empty]\n\n", path)
						} else {
							data := map[string]string{
								"TripleBackticks": "```",
								"Filepath":        path,
								"Content":         content,
							}
							filePrompt, _ = promptcreator.ConstructPrompt(ToolReadFilesResponseSingleFilePrompt, data)
						}

						responsePrompt += filePrompt
					}

					ai.L.Debug("Sending back the files", map[string]interface{}{
						"json": responsePrompt,
					})

					params.Messages.Value = append(params.Messages.Value, openai.ToolMessage(toolCall.ID, responsePrompt))
				} else {
					ai.L.Debug("Unknown tool", map[string]interface{}{
						"name": toolCall.Function.Name,
						"args": toolCall.Function.Arguments,
					})
				}
			}
		}
	}

	return nil, fmt.Errorf("Maximum number of LLM calls reached")
}

func (ai *AIService) constructSystemInstructions(req *OptimizeRequest) (string, error) {
	data := map[string]string{
		"Backtick":        "`",
		"TripleBackticks": "```",
	}

	multistageBuildsPrompt := ""
	if req.DockerfileStageCount == 1 {
		// Only add instructions for multistage builds if the Dockerfile is single-stage
		multistageBuildsPrompt, _ = promptcreator.ConstructPrompt(RuleMultistageBuildsPrompt, data)
	}

	data["RuleMultistageBuilds"] = multistageBuildsPrompt
	return promptcreator.ConstructPrompt(OptimizeRequestSystemPrompt, data)
}

func (ai *AIService) constructUserQuery(req *OptimizeRequest) (string, error) {
	data := map[string]string{
		"Backtick":        "`",
		"TripleBackticks": "```",
		"DirTree":         req.ProjectDirectory.DirTree(),
		"Dockerfile":      req.Dockerfile,
		"PackageJSON":     req.PackageJSON,
	}
	return promptcreator.ConstructPrompt(OptimizeRequestUserPrompt, data)
}
