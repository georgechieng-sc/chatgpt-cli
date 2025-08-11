package client

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/kardolus/chatgpt-cli/api"
	"github.com/kardolus/chatgpt-cli/api/http"
	"github.com/kardolus/chatgpt-cli/cmd/chatgpt/utils"
	"github.com/kardolus/chatgpt-cli/config"
	"github.com/kardolus/chatgpt-cli/internal"
	"go.uber.org/zap"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"
	"io"
	"mime/multipart"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kardolus/chatgpt-cli/history"
	stdhttp "net/http"
)

const (
	AssistantRole            = "assistant"
	ErrEmptyResponse         = "empty response"
	ErrMissingMCPAPIKey      = "the %s api key is not configured"
	ErrUnsupportedProvider   = "unsupported MCP provider"
	ErrHistoryTracking       = "history tracking needs to be enabled to use this feature"
	MaxTokenBufferPercentage = 20
	SystemRole               = "system"
	UserRole                 = "user"
	FunctionRole             = "function"
	InteractiveThreadPrefix  = "int_"
	SearchModelPattern       = "-search"
	ApifyURL                 = "https://api.apify.com/v2/acts/"
	ApifyPath                = "/run-sync-get-dataset-items"
	ApifyProxyConfig         = "proxyConfiguration"
	gptPrefix                = "gpt"
	o1Prefix                 = "o1"
	o1ProPattern             = "o1-pro"
	gpt5Pattern              = "gpt-5"
	audioType                = "input_audio"
	imageURLType             = "image_url"
	messageType              = "message"
	outputTextType           = "output_text"
	imageContent             = "data:%s;base64,%s"
	httpScheme               = "http"
	httpsScheme              = "https"
	bufferSize               = 512
)

type Timer interface {
	Now() time.Time
}

type RealTime struct {
}

func (r *RealTime) Now() time.Time {
	return time.Now()
}

type FileReader interface {
	ReadFile(name string) ([]byte, error)
	ReadBufferFromFile(file *os.File) ([]byte, error)
	Open(name string) (*os.File, error)
}

type RealFileReader struct{}

func (r *RealFileReader) Open(name string) (*os.File, error) {
	return os.Open(name)
}

func (r *RealFileReader) ReadFile(name string) ([]byte, error) {
	return os.ReadFile(name)
}

func (r *RealFileReader) ReadBufferFromFile(file *os.File) ([]byte, error) {
	buffer := make([]byte, bufferSize)
	_, err := file.Read(buffer)

	return buffer, err
}

type FileWriter interface {
	Write(file *os.File, buf []byte) error
	Create(name string) (*os.File, error)
}

type RealFileWriter struct{}

func (w *RealFileWriter) Create(name string) (*os.File, error) {
	return os.Create(name)
}

func (r *RealFileWriter) Write(file *os.File, buf []byte) error {
	_, err := file.Write(buf)
	return err
}

type Client struct {
	Config       config.Config
	History      []history.History
	caller       http.Caller
	historyStore history.Store
	timer        Timer
	reader       FileReader
	writer       FileWriter
}

func New(callerFactory http.CallerFactory, hs history.Store, t Timer, r FileReader, w FileWriter, cfg config.Config, interactiveMode bool) *Client {
	caller := callerFactory(cfg)

	if interactiveMode && cfg.AutoCreateNewThread {
		hs.SetThread(internal.GenerateUniqueSlug(InteractiveThreadPrefix))
	} else {
		hs.SetThread(cfg.Thread)
	}

	return &Client{
		Config:       cfg,
		caller:       caller,
		historyStore: hs,
		timer:        t,
		reader:       r,
		writer:       w,
	}
}

func (c *Client) WithContextWindow(window int) *Client {
	c.Config.ContextWindow = window
	return c
}

func (c *Client) WithServiceURL(url string) *Client {
	c.Config.URL = url
	return c
}

// InjectMCPContext calls an MCP plugin (e.g. Apify) with the given parameters,
// retrieves the result, and adds it to the chat history as a function message.
// The result is formatted as a string and tagged with the function name.
func (c *Client) InjectMCPContext(mcp api.MCPRequest) error {
	if c.Config.OmitHistory {
		return errors.New(ErrHistoryTracking)
	}

	endpoint, headers, body, err := c.buildMCPRequest(mcp)
	if err != nil {
		return err
	}

	c.printRequestDebugInfo(endpoint, body, headers)

	raw, err := c.caller.PostWithHeaders(endpoint, body, headers)
	if err != nil {
		return err
	}

	c.printResponseDebugInfo(raw)

	formatted := formatMCPResponse(raw, mcp.Function)

	c.initHistory()
	c.History = append(c.History, history.History{
		Message: api.Message{
			Role:    FunctionRole,
			Name:    strings.ReplaceAll(mcp.Function, "~", "-"),
			Content: formatted,
		},
		Timestamp: c.timer.Now(),
	})
	c.truncateHistory()

	return c.historyStore.Write(c.History)
}

// ListModels retrieves a list of all available models from the OpenAI API.
// The models are returned as a slice of strings, each entry representing a model ID.
// Models that have an ID starting with 'gpt' are included.
// The currently active model is marked with an asterisk (*) in the list.
// In case of an error during the retrieval or processing of the models,
// the method returns an error. If the API response is empty, an error is returned as well.
func (c *Client) ListModels() ([]string, error) {
	var result []string

	endpoint := c.getEndpoint(c.Config.ModelsPath)

	c.printRequestDebugInfo(endpoint, nil, nil)

	raw, err := c.caller.Get(c.getEndpoint(c.Config.ModelsPath))
	c.printResponseDebugInfo(raw)

	if err != nil {
		return nil, err
	}

	var response api.ListModelsResponse
	if err := c.processResponse(raw, &response); err != nil {
		return nil, err
	}

	sort.Slice(response.Data, func(i, j int) bool {
		return response.Data[i].Id < response.Data[j].Id
	})

	for _, model := range response.Data {
		if strings.HasPrefix(model.Id, gptPrefix) || strings.HasPrefix(model.Id, o1Prefix) {
			if model.Id != c.Config.Model {
				result = append(result, fmt.Sprintf("- %s", model.Id))
				continue
			}
			result = append(result, fmt.Sprintf("* %s (current)", model.Id))
		}
	}

	return result, nil
}

// ProvideContext adds custom context to the client's history by converting the
// provided string into a series of messages. This allows the ChatGPT API to have
// prior knowledge of the provided context when generating responses.
//
// The context string should contain the text you want to provide as context,
// and the method will split it into messages, preserving punctuation and special
// characters.
func (c *Client) ProvideContext(context string) {
	c.initHistory()
	historyEntries := c.createHistoryEntriesFromString(context)
	c.History = append(c.History, historyEntries...)
}

// Query sends a query to the API, returning the response as a string along with the token usage.
//
// It takes a context `ctx` and an input string, constructs a request body, and makes a POST API call.
// The context allows for request scoping, timeouts, and cancellation handling.
//
// Returns the API response string, the number of tokens used, and an error if any issues occur.
// If the response contains choices, it decodes the JSON and returns the content of the first choice.
//
// Parameters:
//   - ctx: A context.Context that controls request cancellation and deadlines.
//   - input: The query string to send to the API.
//
// Returns:
//   - string: The content of the first response choice from the API.
//   - int: The total number of tokens used in the request.
//   - error: An error if the request fails or the response is invalid.
func (c *Client) Query(ctx context.Context, input string) (string, int, error) {
	c.prepareQuery(input)

	body, err := c.createBody(ctx, false)
	if err != nil {
		return "", 0, err
	}

	endpoint := c.getChatEndpoint()

	c.printRequestDebugInfo(endpoint, body, nil)

	raw, err := c.caller.Post(endpoint, body, false)
	c.printResponseDebugInfo(raw)

	if err != nil {
		return "", 0, err
	}

	var (
		response   string
		tokensUsed int
	)

	caps := GetCapabilities(c.Config.Model)

	if caps.UsesResponsesAPI {
		var res api.ResponsesResponse
		if err := c.processResponse(raw, &res); err != nil {
			return "", 0, err
		}
		tokensUsed = res.Usage.TotalTokens

		for _, output := range res.Output {
			if output.Type != messageType {
				continue
			}
			for _, content := range output.Content {
				if content.Type == outputTextType {
					response = content.Text
					break
				}
			}
		}

		if response == "" {
			return "", tokensUsed, errors.New("no response returned")
		}
	} else {
		var res api.CompletionsResponse
		if err := c.processResponse(raw, &res); err != nil {
			return "", 0, err
		}
		tokensUsed = res.Usage.TotalTokens

		if len(res.Choices) == 0 {
			return "", tokensUsed, errors.New("no responses returned")
		}

		var ok bool
		response, ok = res.Choices[0].Message.Content.(string)
		if !ok {
			return "", tokensUsed, errors.New("response cannot be converted to a string")
		}
	}

	c.updateHistory(response)

	return response, tokensUsed, nil
}

// Stream sends a query to the API and processes the response as a stream.
//
// It takes a context `ctx` and an input string, constructs a request body, and makes a POST API call.
// The context allows for request scoping, timeouts, and cancellation handling.
//
// The method creates a request body with the input and calls the API using the `Post` method.
// The actual processing of the streamed response is handled inside the `Post` method.
//
// Parameters:
//   - ctx: A context.Context that controls request cancellation and deadlines.
//   - input: The query string to send to the API.
//
// Returns:
//   - error: An error if the request fails or the response is invalid.
func (c *Client) Stream(ctx context.Context, input string) error {
	c.prepareQuery(input)

	body, err := c.createBody(ctx, true)
	if err != nil {
		return err
	}

	endpoint := c.getChatEndpoint()

	c.printRequestDebugInfo(endpoint, body, nil)

	result, err := c.caller.Post(endpoint, body, true)
	if err != nil {
		return err
	}

	c.updateHistory(string(result))

	return nil
}

// SynthesizeSpeech converts the given input text into speech using the configured TTS model,
// and writes the resulting audio to the specified output file.
//
// The audio format is inferred from the output file's extension (e.g., "mp3", "wav") and sent
// as the "response_format" in the request to the OpenAI speech synthesis endpoint.
//
// Parameters:
//   - inputText: The text to synthesize into speech.
//   - outputPath: The path to the output audio file. The file extension determines the response format.
//
// Returns an error if the request fails, the response cannot be written, or the file cannot be created.
func (c *Client) SynthesizeSpeech(inputText, outputPath string) error {
	req := api.Speech{
		Model:          c.Config.Model,
		Voice:          c.Config.Voice,
		Input:          inputText,
		ResponseFormat: getExtension(outputPath),
	}
	return c.postAndWriteBinaryOutput(c.getEndpoint(c.Config.SpeechPath), req, outputPath, "binary", nil)
}

// GenerateImage sends a prompt to the configured image generation model (e.g., gpt-image-1)
// and writes the resulting image to the specified output path.
//
// The method performs the following steps:
//  1. Sends a POST request to the image generation endpoint with the provided prompt.
//  2. Parses the response and extracts the base64-encoded image data.
//  3. Decodes the image bytes and writes them to the given outputPath.
//  4. Logs the number of bytes written using debug output.
//
// Parameters:
//   - inputText: The prompt describing the image to be generated.
//   - outputPath: The file path where the generated image (e.g., .png) will be saved.
//
// Returns:
//   - An error if any part of the request, decoding, or file writing fails.
func (c *Client) GenerateImage(inputText, outputPath string) error {
	req := api.Draw{
		Model:  c.Config.Model,
		Prompt: inputText,
	}

	return c.postAndWriteBinaryOutput(
		c.getEndpoint(c.Config.ImageGenerationsPath),
		req,
		outputPath,
		"image",
		func(respBytes []byte) ([]byte, error) {
			var response struct {
				Data []struct {
					B64 string `json:"b64_json"`
				} `json:"data"`
			}
			if err := json.Unmarshal(respBytes, &response); err != nil {
				return nil, fmt.Errorf("failed to decode response: %w", err)
			}
			if len(response.Data) == 0 {
				return nil, fmt.Errorf("no image data returned")
			}
			decoded, err := base64.StdEncoding.DecodeString(response.Data[0].B64)
			if err != nil {
				return nil, fmt.Errorf("failed to decode base64 image: %w", err)
			}
			return decoded, nil
		},
	)
}

// EditImage edits an input image using a text prompt and writes the modified image to the specified output path.
//
// This method sends a multipart/form-data POST request to the image editing endpoint
// (typically OpenAI's /v1/images/edits). The request includes:
//   - The image file to edit.
//   - A text prompt describing how the image should be modified.
//   - The model ID (e.g., gpt-image-1).
//
// The response is expected to contain a base64-encoded image, which is decoded and written to the outputPath.
//
// Parameters:
//   - inputText: A text prompt describing the desired modifications to the image.
//   - inputPath: The file path to the source image (must be a supported format: PNG, JPEG, or WebP).
//   - outputPath: The file path where the edited image will be saved.
//
// Returns:
//   - An error if any step of the process fails: reading the file, building the request, sending it,
//     decoding the response, or writing the output image.
//
// Example:
//
//	err := client.EditImage("Add a rainbow in the sky", "input.png", "output.png")
//	if err != nil {
//	    log.Fatal(err)
//	}
func (c *Client) EditImage(inputText, inputPath, outputPath string) error {
	endpoint := c.getEndpoint(c.Config.ImageEditsPath)

	file, err := c.reader.Open(inputPath)
	if err != nil {
		return fmt.Errorf("failed to open input image: %w", err)
	}
	defer file.Close()

	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	mimeType, err := c.getMimeTypeFromFileContent(inputPath)
	if err != nil {
		return fmt.Errorf("failed to detect MIME type: %w", err)
	}
	if !strings.HasPrefix(mimeType, "image/") {
		return fmt.Errorf("unsupported MIME type: %s", mimeType)
	}

	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="image"; filename="%s"`, filepath.Base(inputPath)))
	header.Set("Content-Type", mimeType)

	part, err := writer.CreatePart(header)
	if err != nil {
		return fmt.Errorf("failed to create image part: %w", err)
	}
	if _, err := io.Copy(part, file); err != nil {
		return fmt.Errorf("failed to copy image data: %w", err)
	}

	if err := writer.WriteField("prompt", inputText); err != nil {
		return fmt.Errorf("failed to add prompt: %w", err)
	}
	if err := writer.WriteField("model", c.Config.Model); err != nil {
		return fmt.Errorf("failed to add model: %w", err)
	}

	if err := writer.Close(); err != nil {
		return fmt.Errorf("failed to close multipart writer: %w", err)
	}

	c.printRequestDebugInfo(endpoint, buf.Bytes(), map[string]string{
		"Content-Type": writer.FormDataContentType(),
	})

	respBytes, err := c.caller.PostWithHeaders(endpoint, buf.Bytes(), map[string]string{
		c.Config.AuthHeader: fmt.Sprintf("%s %s", c.Config.AuthTokenPrefix, c.Config.APIKey),
		"Content-Type":      writer.FormDataContentType(),
	})
	if err != nil {
		return fmt.Errorf("failed to edit image: %w", err)
	}

	// Parse the JSON and extract b64_json
	var response struct {
		Data []struct {
			B64 string `json:"b64_json"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBytes, &response); err != nil {
		return fmt.Errorf("failed to decode response: %w", err)
	}
	if len(response.Data) == 0 {
		return fmt.Errorf("no image data returned")
	}

	imgBytes, err := base64.StdEncoding.DecodeString(response.Data[0].B64)
	if err != nil {
		return fmt.Errorf("failed to decode base64 image: %w", err)
	}

	outFile, err := c.writer.Create(outputPath)
	if err != nil {
		return fmt.Errorf("failed to create output file: %w", err)
	}
	defer outFile.Close()

	if err := c.writer.Write(outFile, imgBytes); err != nil {
		return fmt.Errorf("failed to write image: %w", err)
	}

	c.printResponseDebugInfo([]byte(fmt.Sprintf("[image] %d bytes written to %s", len(imgBytes), outputPath)))
	return nil
}

// Transcribe uploads an audio file to the OpenAI transcription endpoint and returns the transcribed text.
//
// It reads the audio file from the provided `audioPath`, creates a multipart/form-data request with the model name
// and the audio file, and sends it to the endpoint defined by the `TranscriptionsPath` in the client config.
// The method expects a JSON response containing a "text" field with the transcription result.
//
// Parameters:
//   - audioPath: The local file path to the audio file to be transcribed.
//
// Returns:
//   - string: The transcribed text from the audio file.
//   - error: An error if the file can't be read, the request fails, or the response is invalid.
//
// This method supports formats like mp3, mp4, mpeg, mpga, m4a, wav, and webm, depending on API compatibility.
func (c *Client) Transcribe(audioPath string) (string, error) {
	c.initHistory()

	file, err := c.reader.Open(audioPath)
	if err != nil {
		return "", fmt.Errorf("failed to open audio file: %w", err)
	}
	defer file.Close()

	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	_ = writer.WriteField("model", c.Config.Model)

	part, err := writer.CreateFormFile("file", filepath.Base(audioPath))
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(part, file); err != nil {
		return "", err
	}

	if err := writer.Close(); err != nil {
		return "", err
	}

	endpoint := c.getEndpoint(c.Config.TranscriptionsPath)
	headers := map[string]string{
		"Content-Type":      writer.FormDataContentType(),
		c.Config.AuthHeader: fmt.Sprintf("%s %s", c.Config.AuthTokenPrefix, c.Config.APIKey),
	}

	c.printRequestDebugInfo(endpoint, buf.Bytes(), headers)

	raw, err := c.caller.PostWithHeaders(endpoint, buf.Bytes(), headers)
	if err != nil {
		return "", err
	}

	c.printResponseDebugInfo(raw)

	var res struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", fmt.Errorf("failed to parse transcription: %w", err)
	}

	c.History = append(c.History, history.History{
		Message: api.Message{
			Role:    UserRole,
			Content: fmt.Sprintf("[transcribe] %s", filepath.Base(audioPath)),
		},
		Timestamp: c.timer.Now(),
	})

	c.History = append(c.History, history.History{
		Message: api.Message{
			Role:    AssistantRole,
			Content: res.Text,
		},
		Timestamp: c.timer.Now(),
	})

	c.truncateHistory()

	if !c.Config.OmitHistory {
		_ = c.historyStore.Write(c.History)
	}

	return res.Text, nil
}

func (c *Client) appendMediaMessages(ctx context.Context, messages []api.Message) ([]api.Message, error) {
	if data, ok := ctx.Value(internal.BinaryDataKey).([]byte); ok {
		content, err := c.createImageContentFromBinary(data)
		if err != nil {
			return nil, err
		}
		messages = append(messages, api.Message{
			Role:    UserRole,
			Content: []api.ImageContent{content},
		})
	} else if path, ok := ctx.Value(internal.ImagePathKey).(string); ok {
		content, err := c.createImageContentFromURLOrFile(path)
		if err != nil {
			return nil, err
		}
		messages = append(messages, api.Message{
			Role:    UserRole,
			Content: []api.ImageContent{content},
		})
	} else if path, ok := ctx.Value(internal.AudioPathKey).(string); ok {
		content, err := c.createAudioContentFromFile(path)
		if err != nil {
			return nil, err
		}
		messages = append(messages, api.Message{
			Role:    UserRole,
			Content: []api.AudioContent{content},
		})
	}
	return messages, nil
}

func (c *Client) createBody(ctx context.Context, stream bool) ([]byte, error) {
	caps := GetCapabilities(c.Config.Model)

	if caps.UsesResponsesAPI {
		req, err := c.createResponsesRequest(ctx, stream)
		if err != nil {
			return nil, err
		}
		return json.Marshal(req)
	}

	req, err := c.createCompletionsRequest(ctx, stream)
	if err != nil {
		return nil, err
	}
	return json.Marshal(req)
}

func (c *Client) createCompletionsRequest(ctx context.Context, stream bool) (*api.CompletionsRequest, error) {
	var messages []api.Message
	caps := GetCapabilities(c.Config.Model)

	for index, item := range c.History {
		if caps.OmitFirstSystemMsg && index == 0 {
			continue
		}
		messages = append(messages, item.Message)
	}

	messages, err := c.appendMediaMessages(ctx, messages)
	if err != nil {
		return nil, err
	}

	req := &api.CompletionsRequest{
		Messages:         messages,
		Model:            c.Config.Model,
		MaxTokens:        c.Config.MaxTokens,
		FrequencyPenalty: c.Config.FrequencyPenalty,
		PresencePenalty:  c.Config.PresencePenalty,
		Seed:             c.Config.Seed,
		Stream:           stream,
	}

	if caps.SupportsTemperature {
		req.Temperature = c.Config.Temperature
		req.TopP = c.Config.TopP
	}

	return req, nil
}

func (c *Client) createResponsesRequest(ctx context.Context, stream bool) (*api.ResponsesRequest, error) {
	var messages []api.Message
	caps := GetCapabilities(c.Config.Model)

	for index, item := range c.History {
		if caps.OmitFirstSystemMsg && index == 0 {
			continue
		}
		messages = append(messages, item.Message)
	}

	messages, err := c.appendMediaMessages(ctx, messages)
	if err != nil {
		return nil, err
	}

	req := &api.ResponsesRequest{
		Model:           c.Config.Model,
		Input:           messages,
		MaxOutputTokens: c.Config.MaxTokens,
		Reasoning: api.Reasoning{
			Effort: c.Config.Effort,
		},
		Stream:      stream,
		Temperature: c.Config.Temperature,
		TopP:        c.Config.TopP,
	}

	return req, nil
}

func (c *Client) createImageContentFromBinary(binary []byte) (api.ImageContent, error) {
	mime, err := getMimeTypeFromBytes(binary)
	if err != nil {
		return api.ImageContent{}, err
	}

	encoded := base64.StdEncoding.EncodeToString(binary)
	content := api.ImageContent{
		Type: imageURLType,
		ImageURL: struct {
			URL string `json:"url"`
		}{
			URL: fmt.Sprintf(imageContent, mime, encoded),
		},
	}

	return content, nil
}

func (c *Client) createAudioContentFromFile(audio string) (api.AudioContent, error) {

	format, err := c.detectAudioFormat(audio)
	if err != nil {
		return api.AudioContent{}, err
	}

	encodedAudio, err := c.base64Encode(audio)
	if err != nil {
		return api.AudioContent{}, err
	}

	return api.AudioContent{
		Type: audioType,
		InputAudio: api.InputAudio{
			Data:   encodedAudio,
			Format: format,
		},
	}, nil
}

func (c *Client) createImageContentFromURLOrFile(image string) (api.ImageContent, error) {
	var content api.ImageContent

	if isValidURL(image) {
		content = api.ImageContent{
			Type: imageURLType,
			ImageURL: struct {
				URL string `json:"url"`
			}{
				URL: image,
			},
		}
	} else {
		mime, err := c.getMimeTypeFromFileContent(image)
		if err != nil {
			return content, err
		}

		encodedImage, err := c.base64Encode(image)
		if err != nil {
			return content, err
		}

		content = api.ImageContent{
			Type: imageURLType,
			ImageURL: struct {
				URL string `json:"url"`
			}{
				URL: fmt.Sprintf(imageContent, mime, encodedImage),
			},
		}
	}

	return content, nil
}

func (c *Client) initHistory() {
	if len(c.History) != 0 {
		return
	}

	if !c.Config.OmitHistory {
		c.History, _ = c.historyStore.Read()
	}

	if len(c.History) == 0 {
		c.History = []history.History{{
			Message: api.Message{
				Role: SystemRole,
			},
			Timestamp: c.timer.Now(),
		}}
	}

	c.History[0].Content = c.Config.Role
}

func (c *Client) addQuery(query string) {
	message := api.Message{
		Role:    UserRole,
		Content: query,
	}

	c.History = append(c.History, history.History{
		Message:   message,
		Timestamp: c.timer.Now(),
	})
	c.truncateHistory()
}

func (c *Client) getChatEndpoint() string {
	caps := GetCapabilities(c.Config.Model)

	var endpoint string
	if caps.UsesResponsesAPI {
		endpoint = c.getEndpoint(c.Config.ResponsesPath)
	} else {
		endpoint = c.getEndpoint(c.Config.CompletionsPath)
	}
	return endpoint
}

func (c *Client) getEndpoint(path string) string {
	return c.Config.URL + path
}

func (c *Client) prepareQuery(input string) {
	c.initHistory()
	c.addQuery(input)
}

func (c *Client) processResponse(raw []byte, v interface{}) error {
	if raw == nil {
		return errors.New(ErrEmptyResponse)
	}

	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("failed to decode response: %w", err)
	}

	return nil
}

func (c *Client) truncateHistory() {
	tokens, rolling := countTokens(c.History)
	effectiveTokenSize := calculateEffectiveContextWindow(c.Config.ContextWindow, MaxTokenBufferPercentage)

	if tokens <= effectiveTokenSize {
		return
	}

	var index int
	var total int
	diff := tokens - effectiveTokenSize

	for i := 1; i < len(rolling); i++ {
		total += rolling[i]
		if total > diff {
			index = i
			break
		}
	}

	c.History = append(c.History[:1], c.History[index+1:]...)
}

func (c *Client) updateHistory(response string) {
	c.History = append(c.History, history.History{
		Message: api.Message{
			Role:    AssistantRole,
			Content: response,
		},
		Timestamp: c.timer.Now(),
	})

	if !c.Config.OmitHistory {
		_ = c.historyStore.Write(c.History)
	}
}

func (c *Client) base64Encode(path string) (string, error) {
	imageData, err := c.reader.ReadFile(path)
	if err != nil {
		return "", err
	}

	return base64.StdEncoding.EncodeToString(imageData), nil
}

func (c *Client) createHistoryEntriesFromString(input string) []history.History {
	var result []history.History

	words := strings.Fields(input)

	for i := 0; i < len(words); i += 100 {
		end := i + 100
		if end > len(words) {
			end = len(words)
		}

		content := strings.Join(words[i:end], " ")

		item := history.History{
			Message: api.Message{
				Role:    UserRole,
				Content: content,
			},
			Timestamp: c.timer.Now(),
		}
		result = append(result, item)
	}

	return result
}

func (c *Client) detectAudioFormat(path string) (string, error) {
	file, err := c.reader.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()

	buf, err := c.reader.ReadBufferFromFile(file)
	if err != nil {
		return "", err
	}

	// WAV
	if string(buf[0:4]) == "RIFF" && string(buf[8:12]) == "WAVE" {
		return "wav", nil
	}

	// MP3 (ID3 or sync bits)
	if string(buf[0:3]) == "ID3" || (buf[0] == 0xFF && (buf[1]&0xE0) == 0xE0) {
		return "mp3", nil
	}

	// FLAC
	if string(buf[0:4]) == "fLaC" {
		return "flac", nil
	}

	// OGG
	if string(buf[0:4]) == "OggS" {
		return "ogg", nil
	}

	// M4A / MP4
	if string(buf[4:8]) == "ftyp" {
		if string(buf[8:12]) == "M4A " || string(buf[8:12]) == "isom" || string(buf[8:12]) == "mp42" {
			return "m4a", nil
		}
		return "mp4", nil
	}

	return "unknown", nil
}

func (c *Client) getMimeTypeFromFileContent(path string) (string, error) {
	file, err := c.reader.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()

	buffer, err := c.reader.ReadBufferFromFile(file)
	if err != nil {
		return "", err
	}

	mimeType := stdhttp.DetectContentType(buffer)

	return mimeType, nil
}

func (c *Client) printRequestDebugInfo(endpoint string, body []byte, headers map[string]string) {
	sugar := zap.S()
	sugar.Debugf("\nGenerated cURL command:\n")

	method := "POST"
	if body == nil {
		method = "GET"
	}
	sugar.Debugf("curl --location --insecure --request %s '%s' \\", method, endpoint)

	if len(headers) > 0 {
		for k, v := range headers {
			sugar.Debugf("  --header '%s: %s' \\", k, v)
		}
	} else {
		sugar.Debugf("  --header \"Authorization: Bearer ${%s_API_KEY}\" \\", strings.ToUpper(c.Config.Name))
		sugar.Debugf("  --header 'Content-Type: application/json' \\")
	}

	if body != nil {
		bodyString := strings.ReplaceAll(string(body), "'", "'\"'\"'")
		sugar.Debugf("  --data-raw '%s'", bodyString)
	}
}

func (c *Client) printResponseDebugInfo(raw []byte) {
	sugar := zap.S()
	sugar.Debugf("\nResponse\n")
	sugar.Debugf("%s\n", raw)
}

func (c *Client) postAndWriteBinaryOutput(endpoint string, requestBody interface{}, outputPath, debugLabel string, transform func([]byte) ([]byte, error)) error {
	body, err := json.Marshal(requestBody)
	if err != nil {
		return fmt.Errorf("failed to marshal request: %w", err)
	}

	c.printRequestDebugInfo(endpoint, body, nil)

	respBytes, err := c.caller.Post(endpoint, body, false)
	if err != nil {
		return fmt.Errorf("API request failed: %w", err)
	}

	if transform != nil {
		respBytes, err = transform(respBytes)
		if err != nil {
			return err
		}
	}

	outFile, err := c.writer.Create(outputPath)
	if err != nil {
		return fmt.Errorf("failed to create output file: %w", err)
	}
	defer outFile.Close()

	if err := c.writer.Write(outFile, respBytes); err != nil {
		return fmt.Errorf("failed to write %s: %w", debugLabel, err)
	}

	c.printResponseDebugInfo([]byte(fmt.Sprintf("[%s] %d bytes written to %s", debugLabel, len(respBytes), outputPath)))
	return nil
}

func (c *Client) buildMCPRequest(mcp api.MCPRequest) (string, map[string]string, []byte, error) {
	mcp.Provider = strings.ToLower(mcp.Provider)
	params := mcp.Params

	if mcp.Provider != utils.ApifyProvider {
		return "", nil, nil, errors.New(ErrUnsupportedProvider)
	}

	apiKey := c.Config.ApifyAPIKey
	if apiKey == "" {
		return "", nil, nil, fmt.Errorf(ErrMissingMCPAPIKey, mcp.Provider)
	}

	params[ApifyProxyConfig] = api.ProxyConfiguration{UseApifyProxy: true}
	endpoint := ApifyURL + mcp.Function + ApifyPath

	headers := map[string]string{
		"Content-Type":  "application/json",
		"Authorization": fmt.Sprintf("Bearer %s", apiKey),
	}

	body, err := json.Marshal(params)
	if err != nil {
		return "", nil, nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	return endpoint, headers, body, nil
}

type ModelCapabilities struct {
	SupportsTemperature bool
	SupportsStreaming   bool
	UsesResponsesAPI    bool
	OmitFirstSystemMsg  bool
}

func GetCapabilities(model string) ModelCapabilities {
	return ModelCapabilities{
		SupportsTemperature: !strings.Contains(model, SearchModelPattern),
		SupportsStreaming:   !strings.Contains(model, o1ProPattern),
		UsesResponsesAPI:    strings.Contains(model, o1ProPattern) || strings.Contains(model, gpt5Pattern),
		OmitFirstSystemMsg:  strings.HasPrefix(model, o1Prefix) && !strings.Contains(model, o1ProPattern),
	}
}

func formatMCPResponse(raw []byte, function string) string {
	var result interface{}
	if err := json.Unmarshal(raw, &result); err != nil {
		return fmt.Sprintf("[MCP: %s] (failed to decode response)", function)
	}

	var lines []string

	switch v := result.(type) {
	case []interface{}:
		if len(v) == 0 {
			return fmt.Sprintf("[MCP: %s] (no data returned)", function)
		}
		if obj, ok := v[0].(map[string]interface{}); ok {
			lines = formatKeyValues(obj)
		} else {
			return fmt.Sprintf("[MCP: %s] (unexpected response format)", function)
		}
	case map[string]interface{}:
		lines = formatKeyValues(v)
	default:
		return fmt.Sprintf("[MCP: %s] (unexpected response format)", function)
	}

	sort.Strings(lines)
	return fmt.Sprintf("[MCP: %s]\n%s", function, strings.Join(lines, "\n"))
}

func formatKeyValues(obj map[string]interface{}) []string {
	var lines []string
	caser := cases.Title(language.English)
	for k, val := range obj {
		label := caser.String(strings.ReplaceAll(k, "_", " "))
		lines = append(lines, fmt.Sprintf("%s: %v", label, val))
	}
	return lines
}

func calculateEffectiveContextWindow(window int, bufferPercentage int) int {
	adjustedPercentage := 100 - bufferPercentage
	effectiveContextWindow := (window * adjustedPercentage) / 100
	return effectiveContextWindow
}

func countTokens(entries []history.History) (int, []int) {
	var result int
	var rolling []int

	for _, entry := range entries {
		charCount, wordCount := 0, 0
		words := strings.Fields(entry.Content.(string))
		wordCount += len(words)

		for _, word := range words {
			charCount += utf8.RuneCountInString(word)
		}

		// This is a simple approximation; actual token count may differ.
		// You can adjust this based on your language and the specific tokenizer used by the model.
		tokenCountForMessage := (charCount + wordCount) / 2
		result += tokenCountForMessage
		rolling = append(rolling, tokenCountForMessage)
	}

	return result, rolling
}

func getExtension(path string) string {
	ext := filepath.Ext(path) // e.g. ".mp4"
	if ext != "" {
		return strings.TrimPrefix(ext, ".") // "mp4"
	}
	return ""
}

func getMimeTypeFromBytes(data []byte) (string, error) {
	mimeType := stdhttp.DetectContentType(data)

	return mimeType, nil
}

func isValidURL(input string) bool {
	parsedURL, err := url.ParseRequestURI(input)
	if err != nil {
		return false
	}

	// Ensure that the URL has a valid scheme
	schemes := []string{httpScheme, httpsScheme}
	for _, scheme := range schemes {
		if strings.HasPrefix(parsedURL.Scheme, scheme) {
			return true
		}
	}

	return false
}
