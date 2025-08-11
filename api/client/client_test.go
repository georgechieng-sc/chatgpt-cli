package client_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/golang/mock/gomock"
	_ "github.com/golang/mock/mockgen/model"
	"github.com/kardolus/chatgpt-cli/api"
	"github.com/kardolus/chatgpt-cli/api/client"
	"github.com/kardolus/chatgpt-cli/api/http"
	"github.com/kardolus/chatgpt-cli/cmd/chatgpt/utils"
	config2 "github.com/kardolus/chatgpt-cli/config"
	"github.com/kardolus/chatgpt-cli/history"
	"github.com/kardolus/chatgpt-cli/internal"
	"github.com/kardolus/chatgpt-cli/test"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	"github.com/sclevine/spec"
	"github.com/sclevine/spec/report"
)

//go:generate mockgen -destination=callermocks_test.go -package=client_test github.com/kardolus/chatgpt-cli/api/http Caller
//go:generate mockgen -destination=historymocks_test.go -package=client_test github.com/kardolus/chatgpt-cli/history Store
//go:generate mockgen -destination=timermocks_test.go -package=client_test github.com/kardolus/chatgpt-cli/api/client Timer
//go:generate mockgen -destination=readermocks_test.go -package=client_test github.com/kardolus/chatgpt-cli/api/client FileReader
//go:generate mockgen -destination=writermocks_test.go -package=client_test github.com/kardolus/chatgpt-cli/api/client FileWriter

const (
	envApiKey       = "api-key"
	commandLineMode = false
	interactiveMode = true
)

var (
	mockCtrl         *gomock.Controller
	mockCaller       *MockCaller
	mockHistoryStore *MockStore
	mockTimer        *MockTimer
	mockReader       *MockFileReader
	mockWriter       *MockFileWriter
	factory          *clientFactory
	apiKeyEnvVar     string
	config           config2.Config
)

func TestUnitClient(t *testing.T) {
	spec.Run(t, "Testing the client package", testClient, spec.Report(report.Terminal{}))
}

func testClient(t *testing.T, when spec.G, it spec.S) {
	const query = "test query"

	it.Before(func() {
		RegisterTestingT(t)
		mockCtrl = gomock.NewController(t)
		mockCaller = NewMockCaller(mockCtrl)
		mockHistoryStore = NewMockStore(mockCtrl)
		mockTimer = NewMockTimer(mockCtrl)
		mockReader = NewMockFileReader(mockCtrl)
		mockWriter = NewMockFileWriter(mockCtrl)
		config = MockConfig()

		factory = newClientFactory(mockHistoryStore)

		apiKeyEnvVar = strings.ToUpper(config.Name) + "_API_KEY"
		Expect(os.Setenv(apiKeyEnvVar, envApiKey)).To(Succeed())
	})

	it.After(func() {
		mockCtrl.Finish()
	})

	when("New()", func() {
		it("should set a unique thread slug in interactive mode when AutoCreateNewThread is true", func() {
			var capturedThread string
			mockHistoryStore.EXPECT().SetThread(gomock.Any()).DoAndReturn(func(thread string) {
				capturedThread = thread
			}).Times(1)

			client.New(mockCallerFactory, mockHistoryStore, mockTimer, mockReader, mockWriter, MockConfig(), interactiveMode)

			Expect(capturedThread).To(HavePrefix(client.InteractiveThreadPrefix))
			Expect(len(capturedThread)).To(Equal(8)) // "int_" (4 chars) + 4 random characters
		})
		it("should not overwrite the thread in interactive mode when AutoCreateNewThread is false", func() {
			var capturedThread string
			mockHistoryStore.EXPECT().SetThread(gomock.Any()).DoAndReturn(func(thread string) {
				capturedThread = thread
			}).Times(1)

			cfg := MockConfig()
			cfg.AutoCreateNewThread = false

			client.New(mockCallerFactory, mockHistoryStore, mockTimer, mockReader, mockWriter, cfg, interactiveMode)

			Expect(capturedThread).To(Equal(config.Thread))
		})
		it("should never overwrite the thread in non-interactive mode", func() {
			var capturedThread string
			mockHistoryStore.EXPECT().SetThread(config.Thread).DoAndReturn(func(thread string) {
				capturedThread = thread
			}).Times(1)

			client.New(mockCallerFactory, mockHistoryStore, mockTimer, mockReader, mockWriter, MockConfig(), commandLineMode)

			Expect(capturedThread).To(Equal(config.Thread))
		})
	})
	when("Query()", func() {
		var (
			body     []byte
			messages []api.Message
			err      error
		)

		type TestCase struct {
			description     string
			setupPostReturn func() ([]byte, error)
			postError       error
			expectedError   string
		}

		tests := []TestCase{
			{
				description:     "throws an error when the http callout fails",
				setupPostReturn: func() ([]byte, error) { return nil, nil },
				postError:       errors.New("error message"),
				expectedError:   "error message",
			},
			{
				description:     "throws an error when the response is empty",
				setupPostReturn: func() ([]byte, error) { return nil, nil },
				postError:       nil,
				expectedError:   "empty response",
			},
			{
				description: "throws an error when the response is a malformed json",
				setupPostReturn: func() ([]byte, error) {
					malformed := `{"invalid":"json"` // missing closing brace
					return []byte(malformed), nil
				},
				postError:     nil,
				expectedError: "failed to decode response:",
			},
			{
				description: "throws an error when the response is missing Choices",
				setupPostReturn: func() ([]byte, error) {
					response := &api.CompletionsResponse{
						ID:      "id",
						Object:  "object",
						Created: 0,
						Model:   "model",
						Choices: []api.Choice{},
					}

					respBytes, err := json.Marshal(response)
					return respBytes, err
				},
				postError:     nil,
				expectedError: "no responses returned",
			},
			{
				description: "throws an error when the response cannot be casted to a string",
				setupPostReturn: func() ([]byte, error) {
					response := &api.CompletionsResponse{
						ID:      "id",
						Object:  "object",
						Created: 0,
						Model:   "model",
						Choices: []api.Choice{
							{
								Message: api.Message{
									Role:    client.AssistantRole,
									Content: 123, // cannot be converted to a string
								},
								FinishReason: "",
								Index:        0,
							},
						},
					}

					respBytes, err := json.Marshal(response)
					return respBytes, err
				},
				postError:     nil,
				expectedError: "response cannot be converted to a string",
			},
		}

		for _, tt := range tests {
			it(tt.description, func() {
				factory.withoutHistory()
				subject := factory.buildClientWithoutConfig()

				messages = createMessages(nil, query)
				body, err = createBody(messages, false)
				Expect(err).NotTo(HaveOccurred())

				respBytes, err := tt.setupPostReturn()
				Expect(err).NotTo(HaveOccurred())
				mockCaller.EXPECT().Post(subject.Config.URL+subject.Config.CompletionsPath, body, false).Return(respBytes, tt.postError)

				mockTimer.EXPECT().Now().Return(time.Time{}).Times(2)

				_, _, err = subject.Query(context.Background(), query)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring(tt.expectedError))
			})
		}

		when("a valid http response is received", func() {
			testValidHTTPResponse := func(subject *client.Client, expectedBody []byte, omitHistory bool) {
				const (
					answer = "content"
					tokens = 789
				)

				choice := api.Choice{
					Message: api.Message{
						Role:    client.AssistantRole,
						Content: answer,
					},
					FinishReason: "",
					Index:        0,
				}
				response := &api.CompletionsResponse{
					ID:      "id",
					Object:  "object",
					Created: 0,
					Model:   subject.Config.Model,
					Choices: []api.Choice{choice},
					Usage: api.Usage{
						PromptTokens:     123,
						CompletionTokens: 456,
						TotalTokens:      tokens,
					},
				}

				respBytes, err := json.Marshal(response)
				Expect(err).NotTo(HaveOccurred())
				mockCaller.EXPECT().Post(subject.Config.URL+subject.Config.CompletionsPath, expectedBody, false).Return(respBytes, nil)

				var request api.CompletionsRequest
				err = json.Unmarshal(expectedBody, &request)
				Expect(err).NotTo(HaveOccurred())

				mockTimer.EXPECT().Now().Return(time.Time{}).AnyTimes()

				var h []history.History
				if !omitHistory {
					for _, msg := range request.Messages {
						h = append(h, history.History{
							Message: msg,
						})
					}

					mockHistoryStore.EXPECT().Write(append(h, history.History{
						Message: api.Message{
							Role:    client.AssistantRole,
							Content: answer,
						},
					}))
				}

				result, usage, err := subject.Query(context.Background(), query)
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(Equal(answer))
				Expect(usage).To(Equal(tokens))
			}
			it("returns the expected result for a non-empty history", func() {
				h := []history.History{
					{
						Message: api.Message{
							Role:    client.SystemRole,
							Content: config.Role,
						},
					},
					{
						Message: api.Message{
							Role:    client.UserRole,
							Content: "question 1",
						},
					},
					{
						Message: api.Message{
							Role:    client.AssistantRole,
							Content: "answer 1",
						},
					},
				}

				messages = createMessages(h, query)
				factory.withHistory(h)
				subject := factory.buildClientWithoutConfig()

				body, err = createBody(messages, false)
				Expect(err).NotTo(HaveOccurred())

				testValidHTTPResponse(subject, body, false)
			})
			it("ignores history when configured to do so", func() {
				mockHistoryStore.EXPECT().SetThread(config.Thread).Times(1)

				config := MockConfig()
				config.OmitHistory = true

				subject := client.New(mockCallerFactory, mockHistoryStore, mockTimer, mockReader, mockWriter, config, commandLineMode)
				Expect(err).NotTo(HaveOccurred())

				// Read and Write are never called on the history store
				mockHistoryStore.EXPECT().Read().Times(0)
				mockHistoryStore.EXPECT().Write(gomock.Any()).Times(0)

				messages = createMessages(nil, query)

				body, err = createBody(messages, false)
				Expect(err).NotTo(HaveOccurred())

				testValidHTTPResponse(subject, body, true)
			})
			it("truncates the history as expected", func() {
				hs := []history.History{
					{
						Message: api.Message{
							Role:    client.SystemRole,
							Content: config.Role,
						},
						Timestamp: time.Time{},
					},
					{
						Message: api.Message{
							Role:    client.UserRole,
							Content: "question 1",
						},
						Timestamp: time.Time{},
					},
					{
						Message: api.Message{
							Role:    client.AssistantRole,
							Content: "answer 1",
						},
						Timestamp: time.Time{},
					},
					{
						Message: api.Message{
							Role:    client.UserRole,
							Content: "question 2",
						},
						Timestamp: time.Time{},
					},
					{
						Message: api.Message{
							Role:    client.AssistantRole,
							Content: "answer 2",
						},
						Timestamp: time.Time{},
					},
					{
						Message: api.Message{
							Role:    client.UserRole,
							Content: "question 3",
						},
						Timestamp: time.Time{},
					},
					{
						Message: api.Message{
							Role:    client.AssistantRole,
							Content: "answer 3",
						},
						Timestamp: time.Time{},
					},
				}

				messages = createMessages(hs, query)

				factory.withHistory(hs)
				subject := factory.buildClientWithoutConfig()

				// messages get truncated. Index 1+2 are cut out
				messages = append(messages[:1], messages[3:]...)

				body, err = createBody(messages, false)
				Expect(err).NotTo(HaveOccurred())

				testValidHTTPResponse(subject, body, false)
			})
			it("should skip the first message when the model starts with o1Prefix", func() {
				factory.withHistory([]history.History{
					{Message: api.Message{Role: client.SystemRole, Content: "First message"}},
					{Message: api.Message{Role: client.UserRole, Content: "Second message"}},
				})

				o1Model := "o1-example-model"
				config.Model = o1Model

				subject := factory.buildClientWithoutConfig()
				subject.Config.Model = o1Model

				expectedBody, err := createBody([]api.Message{
					{Role: client.UserRole, Content: "Second message"},
					{Role: client.UserRole, Content: "test query"},
				}, false)
				Expect(err).NotTo(HaveOccurred())

				mockTimer.EXPECT().Now().Return(time.Now()).AnyTimes()
				mockCaller.EXPECT().Post(subject.Config.URL+subject.Config.CompletionsPath, expectedBody, false).Return(nil, nil)

				_, _, _ = subject.Query(context.Background(), "test query")
			})
			it("should include all messages when the model does not start with o1Prefix", func() {
				const systemRole = "System role for this test"

				factory.withHistory([]history.History{
					{Message: api.Message{Role: client.SystemRole, Content: systemRole}},
					{Message: api.Message{Role: client.UserRole, Content: "Second message"}},
				})

				regularModel := "gpt-4o"
				config.Model = regularModel

				subject := factory.buildClientWithoutConfig()
				subject.Config.Model = regularModel
				subject.Config.Role = systemRole

				expectedBody, err := createBody([]api.Message{
					{Role: client.SystemRole, Content: systemRole},
					{Role: client.UserRole, Content: "Second message"},
					{Role: client.UserRole, Content: "test query"},
				}, false)
				Expect(err).NotTo(HaveOccurred())

				mockTimer.EXPECT().Now().Return(time.Now()).AnyTimes()
				mockCaller.EXPECT().Post(subject.Config.URL+subject.Config.CompletionsPath, expectedBody, false).Return(nil, nil)

				_, _, _ = subject.Query(context.Background(), "test query")
			})
			it("should omit Temperature and TopP when the model matches SearchModelPattern", func() {
				searchModel := "gpt-4o-search-preview"
				config.Model = searchModel
				config.Role = "role for search test"

				factory.withHistory([]history.History{
					{Message: api.Message{Role: client.SystemRole, Content: config.Role}},
				})

				subject := factory.buildClientWithoutConfig()
				subject.Config.Model = searchModel

				mockTimer.EXPECT().Now().Return(time.Now()).AnyTimes()

				mockCaller.EXPECT().
					Post(gomock.Any(), gomock.Any(), false).
					DoAndReturn(func(_ string, body []byte, _ bool) ([]byte, error) {
						var req map[string]interface{}
						Expect(json.Unmarshal(body, &req)).To(Succeed())

						// Should not include Temperature or TopP
						Expect(req).NotTo(HaveKey("temperature"))
						Expect(req).NotTo(HaveKey("top_p"))

						return nil, nil
					})

				_, _, _ = subject.Query(context.Background(), "test query")
			})
			it("should include Temperature and TopP when the model does not match SearchModelPattern", func() {
				regularModel := "gpt-4o"
				config.Model = regularModel
				config.Role = "regular model test"

				factory.withHistory([]history.History{
					{Message: api.Message{Role: client.SystemRole, Content: config.Role}},
				})

				subject := factory.buildClientWithoutConfig()
				subject.Config.Model = regularModel

				mockTimer.EXPECT().Now().Return(time.Now()).AnyTimes()

				mockCaller.EXPECT().
					Post(gomock.Any(), gomock.Any(), false).
					DoAndReturn(func(_ string, body []byte, _ bool) ([]byte, error) {
						var req map[string]interface{}
						Expect(json.Unmarshal(body, &req)).To(Succeed())

						Expect(req).To(HaveKeyWithValue("temperature", BeNumerically("==", config.Temperature)))
						Expect(req).To(HaveKeyWithValue("top_p", BeNumerically("==", config.TopP)))

						return nil, nil
					})

				_, _, _ = subject.Query(context.Background(), "test query")
			})
		})

		when("an image is provided", func() {
			const (
				query        = "test query"
				systemRole   = "System role for this test"
				errorMessage = "error message"
				image        = "path/to/image.wrong"
				website      = "https://website.com"
			)

			it.Before(func() {
				factory.withoutHistory()
			})

			it("should update a callout as expected when a valid image URL is provided", func() {
				subject := factory.buildClientWithoutConfig()

				subject.Config.Role = systemRole

				ctx := context.Background()
				ctx = context.WithValue(ctx, internal.ImagePathKey, website)

				expectedBody, err := createBody([]api.Message{
					{Role: client.SystemRole, Content: systemRole},
					{Role: client.UserRole, Content: query},
					{Role: client.UserRole, Content: []api.ImageContent{{
						Type: "image_url",
						ImageURL: struct {
							URL string `json:"url"`
						}{
							URL: website,
						},
					}}},
				}, false)
				Expect(err).NotTo(HaveOccurred())

				mockTimer.EXPECT().Now().Return(time.Now()).Times(2)
				mockCaller.EXPECT().Post(subject.Config.URL+subject.Config.CompletionsPath, expectedBody, false).Return(nil, nil)

				_, _, _ = subject.Query(ctx, query)
			})
			it("throws an error when the image mime type cannot be obtained due to an open-error", func() {
				subject := factory.buildClientWithoutConfig()
				subject.Config.Role = systemRole

				ctx := context.Background()
				ctx = context.WithValue(ctx, internal.ImagePathKey, image)

				mockTimer.EXPECT().Now().Return(time.Now()).Times(2)
				mockReader.EXPECT().Open(image).Return(nil, errors.New(errorMessage))

				_, _, err := subject.Query(ctx, query)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(Equal(errorMessage))
			})
			it("throws an error when the image mime type cannot be obtained due to a read-error", func() {
				imageFile := &os.File{}

				subject := factory.buildClientWithoutConfig()
				subject.Config.Role = systemRole

				ctx := context.Background()
				ctx = context.WithValue(ctx, internal.ImagePathKey, image)

				mockTimer.EXPECT().Now().Return(time.Now()).Times(2)
				mockReader.EXPECT().Open(image).Return(imageFile, nil)
				mockReader.EXPECT().ReadBufferFromFile(imageFile).Return(nil, errors.New(errorMessage))

				_, _, err := subject.Query(ctx, query)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(Equal(errorMessage))
			})
			it("throws an error when the image base64 encoded content cannot be obtained due to a read-error", func() {
				imageFile := &os.File{}

				subject := factory.buildClientWithoutConfig()
				subject.Config.Role = systemRole

				ctx := context.Background()
				ctx = context.WithValue(ctx, internal.ImagePathKey, image)

				mockTimer.EXPECT().Now().Return(time.Now()).Times(2)
				mockReader.EXPECT().Open(image).Return(imageFile, nil)
				mockReader.EXPECT().ReadBufferFromFile(imageFile).Return(nil, nil)
				mockReader.EXPECT().ReadFile(image).Return(nil, errors.New(errorMessage))

				_, _, err := subject.Query(ctx, query)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(Equal(errorMessage))
			})
			it("should update a callout as expected when a valid local image is provided", func() {
				imageFile := &os.File{}

				subject := factory.buildClientWithoutConfig()
				subject.Config.Role = systemRole

				ctx := context.Background()
				ctx = context.WithValue(ctx, internal.ImagePathKey, image)

				mockReader.EXPECT().Open(image).Return(imageFile, nil)
				mockReader.EXPECT().ReadBufferFromFile(imageFile).Return(nil, nil)
				mockReader.EXPECT().ReadFile(image).Return(nil, nil)

				expectedBody, err := createBody([]api.Message{
					{Role: client.SystemRole, Content: systemRole},
					{Role: client.UserRole, Content: query},
					{Role: client.UserRole, Content: []api.ImageContent{{
						Type: "image_url",
						ImageURL: struct {
							URL string `json:"url"`
						}{
							URL: "data:text/plain; charset=utf-8;base64,",
						},
					}}},
				}, false)
				Expect(err).NotTo(HaveOccurred())

				mockTimer.EXPECT().Now().Return(time.Now()).Times(2)
				mockCaller.EXPECT().Post(subject.Config.URL+subject.Config.CompletionsPath, expectedBody, false).Return(nil, nil)

				_, _, _ = subject.Query(ctx, query)
			})
		})

		when("an audio file is provided", func() {
			const (
				query        = "transcribe this"
				systemRole   = "System role for audio test"
				errorMessage = "error opening audio file"
				audio        = "path/to/audio.wav"
			)

			it.Before(func() {
				factory.withoutHistory()
			})

			it("throws an error when the audio file cannot be opened", func() {
				subject := factory.buildClientWithoutConfig()
				subject.Config.Role = systemRole

				ctx := context.Background()
				ctx = context.WithValue(ctx, internal.AudioPathKey, audio)

				mockTimer.EXPECT().Now().Return(time.Now()).Times(2)
				mockReader.EXPECT().Open(audio).Return(nil, errors.New(errorMessage))

				_, _, err := subject.Query(ctx, query)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(Equal(errorMessage))
			})

			it("throws an error when the audio data cannot be read", func() {
				audioFile := &os.File{}
				subject := factory.buildClientWithoutConfig()
				subject.Config.Role = systemRole

				ctx := context.Background()
				ctx = context.WithValue(ctx, internal.AudioPathKey, audio)

				mockTimer.EXPECT().Now().Return(time.Now()).Times(2)
				mockReader.EXPECT().Open(audio).Return(audioFile, nil)
				mockReader.EXPECT().ReadBufferFromFile(audioFile).Return([]byte("RIFFxxxxWAVE..."), nil)
				mockReader.EXPECT().ReadFile(audio).Return(nil, errors.New(errorMessage))

				_, _, err := subject.Query(ctx, query)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(Equal(errorMessage))
			})

			it("adds audio as input_audio type content when valid", func() {
				audioFile := &os.File{}
				subject := factory.buildClientWithoutConfig()
				subject.Config.Role = systemRole

				ctx := context.Background()
				ctx = context.WithValue(ctx, internal.AudioPathKey, audio)

				mockReader.EXPECT().Open(audio).Return(audioFile, nil)
				mockReader.EXPECT().ReadBufferFromFile(audioFile).Return([]byte("RIFFxxxxWAVE..."), nil)
				mockReader.EXPECT().ReadFile(audio).Return([]byte("audio-bytes"), nil)

				expectedBody, err := createBody([]api.Message{
					{Role: client.SystemRole, Content: systemRole},
					{Role: client.UserRole, Content: query},
					{Role: client.UserRole, Content: []api.AudioContent{{
						Type: "input_audio",
						InputAudio: struct {
							Data   string `json:"data"`
							Format string `json:"format"`
						}{
							Data:   "YXVkaW8tYnl0ZXM=", // base64 of "audio-bytes"
							Format: "wav",
						},
					}}},
				}, false)
				Expect(err).NotTo(HaveOccurred())

				mockTimer.EXPECT().Now().Return(time.Now()).Times(2)
				mockCaller.EXPECT().Post(subject.Config.URL+subject.Config.CompletionsPath, expectedBody, false).Return(nil, nil)

				_, _, _ = subject.Query(ctx, query)
			})
		})

		when("the model is o1-pro or gpt-5", func() {
			models := []string{"o1-pro", "gpt-5"}

			for _, m := range models {
				m := m // capture
				when(fmt.Sprintf("the model is %s", m), func() {
					const (
						query       = "what's the weather"
						systemRole  = "you are helpful"
						totalTokens = 777
					)

					it.Before(func() {
						config.Model = m
						config.Role = systemRole
						factory.withoutHistory()
					})

					it("returns the output_text when present", func() {
						subject := factory.buildClientWithoutConfig()
						subject.Config.Model = m
						subject.Config.Role = systemRole

						answer := "yes, it does"
						messages := []api.Message{
							{Role: client.SystemRole, Content: systemRole},
							{Role: client.UserRole, Content: query},
						}

						body, err := json.Marshal(api.ResponsesRequest{
							Model:           subject.Config.Model,
							Input:           messages,
							MaxOutputTokens: subject.Config.MaxTokens,
							Reasoning:       api.Reasoning{Effort: "low"},
							Stream:          false,
							Temperature:     subject.Config.Temperature,
							TopP:            subject.Config.TopP,
						})
						Expect(err).NotTo(HaveOccurred())

						mockTimer.EXPECT().Now().Times(3)
						mockHistoryStore.EXPECT().Write(gomock.Any())

						response := api.ResponsesResponse{
							Output: []api.Output{{
								Type:    "message",
								Content: []api.Content{{Type: "output_text", Text: answer}},
							}},
							Usage: api.TokenUsage{TotalTokens: 42},
						}
						raw, _ := json.Marshal(response)

						mockCaller.EXPECT().
							Post(subject.Config.URL+"/v1/responses", body, false).
							Return(raw, nil)

						text, tokens, err := subject.Query(context.Background(), query)
						Expect(err).NotTo(HaveOccurred())
						Expect(text).To(Equal(answer))
						Expect(tokens).To(Equal(42))
					})

					it("errors when no output blocks are present", func() {
						subject := factory.buildClientWithoutConfig()
						subject.Config.Model = m
						subject.Config.Role = systemRole

						messages := []api.Message{
							{Role: client.SystemRole, Content: systemRole},
							{Role: client.UserRole, Content: query},
						}

						body, _ := json.Marshal(api.ResponsesRequest{
							Model:           subject.Config.Model,
							Input:           messages,
							MaxOutputTokens: subject.Config.MaxTokens,
							Reasoning:       api.Reasoning{Effort: "low"},
							Stream:          false,
							Temperature:     subject.Config.Temperature,
							TopP:            subject.Config.TopP,
						})

						mockTimer.EXPECT().Now().Times(2)

						response := api.ResponsesResponse{
							Output: []api.Output{},
							Usage:  api.TokenUsage{TotalTokens: totalTokens},
						}
						raw, _ := json.Marshal(response)

						mockCaller.EXPECT().
							Post(subject.Config.URL+"/v1/responses", body, false).
							Return(raw, nil)

						_, _, err := subject.Query(context.Background(), query)
						Expect(err).To(HaveOccurred())
						Expect(err.Error()).To(Equal("no response returned"))
					})

					it("errors when message has no output_text", func() {
						subject := factory.buildClientWithoutConfig()
						subject.Config.Model = m
						subject.Config.Role = systemRole

						messages := []api.Message{
							{Role: client.SystemRole, Content: systemRole},
							{Role: client.UserRole, Content: query},
						}

						body, _ := json.Marshal(api.ResponsesRequest{
							Model:           subject.Config.Model,
							Input:           messages,
							MaxOutputTokens: subject.Config.MaxTokens,
							Reasoning:       api.Reasoning{Effort: "low"},
							Stream:          false,
							Temperature:     subject.Config.Temperature,
							TopP:            subject.Config.TopP,
						})

						mockTimer.EXPECT().Now().Times(2)

						response := api.ResponsesResponse{
							Output: []api.Output{{
								Type:    "message",
								Content: []api.Content{{Type: "refusal", Text: "nope"}},
							}},
							Usage: api.TokenUsage{TotalTokens: totalTokens},
						}
						raw, _ := json.Marshal(response)

						mockCaller.EXPECT().
							Post(subject.Config.URL+"/v1/responses", body, false).
							Return(raw, nil)

						_, _, err := subject.Query(context.Background(), query)
						Expect(err).To(HaveOccurred())
						Expect(err.Error()).To(Equal("no response returned"))
					})
				})
			}
		})
	})
	when("Stream()", func() {
		var (
			body     []byte
			messages []api.Message
			err      error
		)

		it("throws an error when the http callout fails", func() {
			factory.withoutHistory()
			subject := factory.buildClientWithoutConfig()

			messages = createMessages(nil, query)
			body, err = createBody(messages, true)
			Expect(err).NotTo(HaveOccurred())

			errorMsg := "error message"
			mockCaller.EXPECT().Post(subject.Config.URL+subject.Config.CompletionsPath, body, true).Return(nil, errors.New(errorMsg))

			mockTimer.EXPECT().Now().Return(time.Time{}).Times(2)

			err := subject.Stream(context.Background(), query)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(Equal(errorMsg))
		})
		when("a valid http response is received", func() {
			const answer = "answer"

			testValidHTTPResponse := func(subject *client.Client, hs []history.History, expectedBody []byte) {
				messages = createMessages(nil, query)
				body, err = createBody(messages, true)
				Expect(err).NotTo(HaveOccurred())

				mockCaller.EXPECT().Post(subject.Config.URL+subject.Config.CompletionsPath, expectedBody, true).Return([]byte(answer), nil)

				mockTimer.EXPECT().Now().Return(time.Time{}).AnyTimes()

				messages = createMessages(hs, query)

				hs = []history.History{}

				for _, message := range messages {
					hs = append(hs, history.History{
						Message: message,
					})
				}

				mockHistoryStore.EXPECT().Write(append(hs, history.History{
					Message: api.Message{
						Role:    client.AssistantRole,
						Content: answer,
					},
				}))

				err := subject.Stream(context.Background(), query)
				Expect(err).NotTo(HaveOccurred())
			}

			it("returns the expected result for an empty history", func() {
				factory.withHistory(nil)
				subject := factory.buildClientWithoutConfig()

				messages = createMessages(nil, query)
				body, err = createBody(messages, true)
				Expect(err).NotTo(HaveOccurred())

				testValidHTTPResponse(subject, nil, body)
			})
			it("returns the expected result for a non-empty history", func() {
				h := []history.History{
					{
						Message: api.Message{
							Role:    client.SystemRole,
							Content: config.Role,
						},
					},
					{
						Message: api.Message{
							Role:    client.UserRole,
							Content: "question x",
						},
					},
					{
						Message: api.Message{
							Role:    client.AssistantRole,
							Content: "answer x",
						},
					},
				}
				factory.withHistory(h)
				subject := factory.buildClientWithoutConfig()

				messages = createMessages(h, query)
				body, err = createBody(messages, true)
				Expect(err).NotTo(HaveOccurred())

				testValidHTTPResponse(subject, h, body)
			})
		})
	})
	when("SynthesizeSpeech()", func() {
		const (
			inputText      = "mock-input"
			outputFile     = "mock-output"
			outputFileType = "mp3"
			errorText      = "mock error occurred"
		)

		var (
			subject  *client.Client
			fileName = outputFile + "." + outputFileType
			body     []byte
			response []byte
		)
		it.Before(func() {
			subject = factory.buildClientWithoutConfig()
			request := api.Speech{
				Model:          subject.Config.Model,
				Voice:          subject.Config.Voice,
				Input:          inputText,
				ResponseFormat: outputFileType,
			}
			var err error
			body, err = json.Marshal(request)
			Expect(err).NotTo(HaveOccurred())

			response = []byte("mock response")
		})
		it("throws an error when the http call fails", func() {
			mockCaller.EXPECT().Post(subject.Config.URL+subject.Config.SpeechPath, body, false).Return(nil, errors.New(errorText))

			err := subject.SynthesizeSpeech(inputText, fileName)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(errorText))
		})
		it("throws an error when a file cannot be created", func() {
			mockCaller.EXPECT().Post(subject.Config.URL+subject.Config.SpeechPath, body, false).Return(response, nil)
			mockWriter.EXPECT().Create(fileName).Return(nil, errors.New(errorText))

			err := subject.SynthesizeSpeech(inputText, fileName)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(errorText))
		})
		it("throws an error when bytes cannot be written to the output file", func() {
			file, err := os.Open(os.DevNull)
			Expect(err).NotTo(HaveOccurred())
			defer file.Close()

			mockCaller.EXPECT().Post(subject.Config.URL+subject.Config.SpeechPath, body, false).Return(response, nil)
			mockWriter.EXPECT().Create(fileName).Return(file, nil)
			mockWriter.EXPECT().Write(file, response).Return(errors.New(errorText))

			err = subject.SynthesizeSpeech(inputText, fileName)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(errorText))
		})
		it("succeeds when no errors occurred", func() {
			file, err := os.Open(os.DevNull)
			Expect(err).NotTo(HaveOccurred())
			defer file.Close()

			mockCaller.EXPECT().Post(subject.Config.URL+subject.Config.SpeechPath, body, false).Return(response, nil)
			mockWriter.EXPECT().Create(fileName).Return(file, nil)
			mockWriter.EXPECT().Write(file, response).Return(nil)

			err = subject.SynthesizeSpeech(inputText, fileName)
			Expect(err).NotTo(HaveOccurred())
		})
	})
	when("GenerateImage()", func() {
		const (
			inputText  = "draw a happy dog"
			outputFile = "dog.png"
			errorText  = "mock error occurred"
		)

		var (
			subject *client.Client
			body    []byte
		)

		it.Before(func() {
			subject = factory.buildClientWithoutConfig()
			request := api.Draw{
				Model:  subject.Config.Model,
				Prompt: inputText,
			}
			var err error
			body, err = json.Marshal(request)
			Expect(err).NotTo(HaveOccurred())
		})
		it("throws an error when the http call fails", func() {
			mockCaller.EXPECT().
				Post(subject.Config.URL+subject.Config.ImageGenerationsPath, body, false).
				Return(nil, errors.New(errorText))

			err := subject.GenerateImage(inputText, outputFile)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(errorText))
		})
		it("throws an error when no image data is returned", func() {
			mockCaller.EXPECT().
				Post(subject.Config.URL+subject.Config.ImageGenerationsPath, body, false).
				Return([]byte(`{"data":[]}`), nil)

			err := subject.GenerateImage(inputText, outputFile)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("no image data returned"))
		})
		it("throws an error when base64 is invalid", func() {
			mockCaller.EXPECT().
				Post(subject.Config.URL+subject.Config.ImageGenerationsPath, body, false).
				Return([]byte(`{"data":[{"b64_json":"!!notbase64!!"}]}`), nil)

			err := subject.GenerateImage(inputText, outputFile)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to decode base64 image"))
		})
		it("throws an error when a file cannot be created", func() {
			valid := base64.StdEncoding.EncodeToString([]byte("image-bytes"))

			mockCaller.EXPECT().
				Post(subject.Config.URL+subject.Config.ImageGenerationsPath, body, false).
				Return([]byte(fmt.Sprintf(`{"data":[{"b64_json":"%s"}]}`, valid)), nil)

			mockWriter.EXPECT().Create(outputFile).Return(nil, errors.New(errorText))

			err := subject.GenerateImage(inputText, outputFile)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(errorText))
		})
		it("throws an error when bytes cannot be written to the file", func() {
			valid := base64.StdEncoding.EncodeToString([]byte("image-bytes"))
			file, err := os.Open(os.DevNull)
			Expect(err).NotTo(HaveOccurred())
			defer file.Close()

			mockCaller.EXPECT().
				Post(subject.Config.URL+subject.Config.ImageGenerationsPath, body, false).
				Return([]byte(fmt.Sprintf(`{"data":[{"b64_json":"%s"}]}`, valid)), nil)

			mockWriter.EXPECT().Create(outputFile).Return(file, nil)
			mockWriter.EXPECT().Write(file, []byte("image-bytes")).Return(errors.New(errorText))

			err = subject.GenerateImage(inputText, outputFile)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(errorText))
		})
		it("succeeds when all steps complete", func() {
			valid := base64.StdEncoding.EncodeToString([]byte("image-bytes"))
			file, err := os.Open(os.DevNull)
			Expect(err).NotTo(HaveOccurred())
			defer file.Close()

			mockCaller.EXPECT().
				Post(subject.Config.URL+subject.Config.ImageGenerationsPath, body, false).
				Return([]byte(fmt.Sprintf(`{"data":[{"b64_json":"%s"}]}`, valid)), nil)

			mockWriter.EXPECT().Create(outputFile).Return(file, nil)
			mockWriter.EXPECT().Write(file, []byte("image-bytes")).Return(nil)

			err = subject.GenerateImage(inputText, outputFile)
			Expect(err).NotTo(HaveOccurred())
		})
	})
	when("EditImage()", func() {
		const (
			inputText  = "give the dog sunglasses"
			inputFile  = "dog.png"
			outputFile = "dog_cool.png"
			errorText  = "mock error occurred"
		)

		var (
			subject    *client.Client
			validB64   string
			imageBytes = []byte("image-bytes")
			respBytes  []byte
		)

		it.Before(func() {
			subject = factory.buildClientWithoutConfig()
			validB64 = base64.StdEncoding.EncodeToString(imageBytes)
			respBytes = []byte(fmt.Sprintf(`{"data":[{"b64_json":"%s"}]}`, validB64))
		})

		it("returns error when input file can't be opened", func() {
			mockReader.EXPECT().Open(inputFile).Return(nil, errors.New(errorText))

			err := subject.EditImage(inputText, inputFile, outputFile)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to open input image"))
		})
		it("returns error on invalid mime type", func() {
			file := openDummy()
			mockReader.EXPECT().Open(inputFile).Return(file, nil).Times(2)
			mockReader.EXPECT().ReadBufferFromFile(file).Return([]byte("not an image"), nil)

			err := subject.EditImage(inputText, inputFile, outputFile)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("unsupported MIME type"))
		})
		it("returns error when HTTP call fails", func() {
			mockReader.EXPECT().Open(inputFile).DoAndReturn(func(string) (*os.File, error) {
				return openDummy(), nil
			}).Times(2)

			mockReader.EXPECT().
				ReadBufferFromFile(gomock.AssignableToTypeOf(&os.File{})).
				Return([]byte("\x89PNG\r\n\x1a\n"), nil)

			mockCaller.EXPECT().
				PostWithHeaders(gomock.Any(), gomock.Any(), gomock.Any()).
				Return(nil, errors.New(errorText))

			err := subject.EditImage(inputText, inputFile, outputFile)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to edit image"))
		})
		it("returns error when base64 is invalid", func() {
			invalidResp := []byte(`{"data":[{"b64_json":"!notbase64"}]}`)

			mockReader.EXPECT().Open(inputFile).DoAndReturn(func(string) (*os.File, error) {
				return openDummy(), nil
			}).Times(2)

			mockReader.EXPECT().
				ReadBufferFromFile(gomock.AssignableToTypeOf(&os.File{})).
				Return([]byte("\x89PNG\r\n\x1a\n"), nil)

			mockCaller.EXPECT().
				PostWithHeaders(gomock.Any(), gomock.Any(), gomock.Any()).
				Return(invalidResp, nil)

			err := subject.EditImage(inputText, inputFile, outputFile)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to decode base64 image"))
		})
		it("writes image when all steps succeed", func() {
			file := openDummy()
			mockReader.EXPECT().Open(inputFile).DoAndReturn(func(string) (*os.File, error) {
				return openDummy(), nil
			}).Times(2)

			mockReader.EXPECT().
				ReadBufferFromFile(gomock.AssignableToTypeOf(&os.File{})).
				Return([]byte("\x89PNG\r\n\x1a\n"), nil)

			mockCaller.EXPECT().
				PostWithHeaders(gomock.Any(), gomock.Any(), gomock.Any()).
				Return(respBytes, nil)

			mockWriter.EXPECT().Create(outputFile).Return(file, nil)
			mockWriter.EXPECT().Write(file, imageBytes).Return(nil)

			err := subject.EditImage(inputText, inputFile, outputFile)
			Expect(err).NotTo(HaveOccurred())
		})
	})
	when("Transcribe()", func() {
		const audioPath = "path/to/audio.wav"
		const transcribedText = "Hello, this is a test."

		it("returns an error if the audio file cannot be opened", func() {
			subject := factory.buildClientWithoutConfig()

			mockHistoryStore.EXPECT().Read().Return(nil, nil)
			mockTimer.EXPECT().Now().Times(1)

			mockReader.EXPECT().Open(audioPath).Return(nil, errors.New("cannot open"))

			_, err := subject.Transcribe(audioPath)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("cannot open"))
		})

		it("returns an error if copying audio content fails", func() {
			subject := factory.buildClientWithoutConfig()

			mockHistoryStore.EXPECT().Read().Return(nil, nil)
			mockTimer.EXPECT().Now().Times(1)

			reader, writer, err := os.Pipe()
			Expect(err).NotTo(HaveOccurred())

			// Immediately close writer so reader will return EOF
			_ = writer.Close()

			mockReader.EXPECT().Open(audioPath).Return(reader, nil)

			mockCaller.EXPECT().
				PostWithHeaders(subject.Config.URL+subject.Config.TranscriptionsPath, gomock.Any(), gomock.Any())

			_, err = subject.Transcribe(audioPath)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed"))
		})

		it("returns an error if the API call fails", func() {
			subject := factory.buildClientWithoutConfig()

			mockHistoryStore.EXPECT().Read().Return(nil, nil)
			mockTimer.EXPECT().Now().Times(1)

			file, err := os.Open(os.DevNull)
			Expect(err).NotTo(HaveOccurred())
			defer file.Close()

			mockReader.EXPECT().Open(audioPath).Return(file, nil)

			mockCaller.EXPECT().
				PostWithHeaders(subject.Config.URL+subject.Config.TranscriptionsPath, gomock.Any(), gomock.Any()).
				Return(nil, errors.New("network error"))

			_, err = subject.Transcribe(audioPath)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("network error"))
		})

		it("returns the transcribed text when successful", func() {
			subject := factory.buildClientWithoutConfig()

			mockHistoryStore.EXPECT().Read().Return(nil, nil)

			now := time.Now()
			mockTimer.EXPECT().Now().Return(now).Times(3)

			file, err := os.Open(os.DevNull)
			Expect(err).NotTo(HaveOccurred())
			defer file.Close()

			mockReader.EXPECT().Open(audioPath).Return(file, nil)

			resp := []byte(`{"text": "Hello, this is a test."}`)
			mockCaller.EXPECT().
				PostWithHeaders(subject.Config.URL+subject.Config.TranscriptionsPath, gomock.Any(), gomock.Any()).
				Return(resp, nil)

			expectedHistory := []history.History{
				{
					Message: api.Message{
						Role:    client.SystemRole,
						Content: subject.Config.Role,
					},
					Timestamp: now,
				},
				{
					Message: api.Message{
						Role:    client.UserRole,
						Content: "[transcribe] audio.wav",
					},
					Timestamp: now,
				},
				{
					Message: api.Message{
						Role:    client.AssistantRole,
						Content: transcribedText,
					},
					Timestamp: now,
				},
			}

			mockHistoryStore.EXPECT().Write(expectedHistory)

			text, err := subject.Transcribe(audioPath)
			Expect(err).NotTo(HaveOccurred())
			Expect(text).To(Equal(transcribedText))
		})
	})
	when("ListModels()", func() {
		it("throws an error when the http callout fails", func() {
			subject := factory.buildClientWithoutConfig()

			errorMsg := "error message"
			mockCaller.EXPECT().Get(subject.Config.URL+subject.Config.ModelsPath).Return(nil, errors.New(errorMsg))

			_, err := subject.ListModels()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(Equal(errorMsg))
		})
		it("throws an error when the response is empty", func() {
			subject := factory.buildClientWithoutConfig()

			mockCaller.EXPECT().Get(subject.Config.URL+subject.Config.ModelsPath).Return(nil, nil)

			_, err := subject.ListModels()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(Equal("empty response"))
		})
		it("throws an error when the response is a malformed json", func() {
			subject := factory.buildClientWithoutConfig()

			malformed := `{"invalid":"json"` // missing closing brace
			mockCaller.EXPECT().Get(subject.Config.URL+subject.Config.ModelsPath).Return([]byte(malformed), nil)

			_, err := subject.ListModels()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).Should(HavePrefix("failed to decode response:"))
		})
		it("filters gpt and o1 models as expected and puts them in alphabetical order", func() {
			subject := factory.buildClientWithoutConfig()

			response, err := test.FileToBytes("models.json")
			Expect(err).NotTo(HaveOccurred())

			mockCaller.EXPECT().Get(subject.Config.URL+subject.Config.ModelsPath).Return(response, nil)

			result, err := subject.ListModels()
			Expect(err).NotTo(HaveOccurred())
			Expect(result).NotTo(BeEmpty())
			Expect(result).To(HaveLen(5))
			Expect(result[0]).To(Equal("- gpt-3.5-env-model"))
			Expect(result[1]).To(Equal("* gpt-3.5-turbo (current)"))
			Expect(result[2]).To(Equal("- gpt-3.5-turbo-0301"))
			Expect(result[3]).To(Equal("- gpt-4o"))
			Expect(result[4]).To(Equal("- o1-mini"))
		})
	})
	when("ProvideContext()", func() {
		it("updates the history with the provided context", func() {
			subject := factory.buildClientWithoutConfig()

			chatContext := "This is a story about a dog named Kya. Kya loves to play fetch and swim in the lake."
			mockHistoryStore.EXPECT().Read().Return(nil, nil).Times(1)

			mockTimer.EXPECT().Now().Return(time.Time{}).AnyTimes()

			subject.ProvideContext(chatContext)

			Expect(len(subject.History)).To(Equal(2)) // The system message and the provided context

			systemMessage := subject.History[0]
			Expect(systemMessage.Role).To(Equal(client.SystemRole))
			Expect(systemMessage.Content).To(Equal(config.Role))

			contextMessage := subject.History[1]
			Expect(contextMessage.Role).To(Equal(client.UserRole))
			Expect(contextMessage.Content).To(Equal(chatContext))
		})
		it("behaves as expected with a non empty initial history", func() {
			subject := factory.buildClientWithoutConfig()

			subject.History = []history.History{
				{
					Message: api.Message{
						Role:    client.SystemRole,
						Content: "system message",
					},
				},
				{
					Message: api.Message{
						Role: client.UserRole,
					},
				},
			}

			mockTimer.EXPECT().Now().Return(time.Time{}).AnyTimes()

			chatContext := "test context"
			subject.ProvideContext(chatContext)

			Expect(len(subject.History)).To(Equal(3))

			contextMessage := subject.History[2]
			Expect(contextMessage.Role).To(Equal(client.UserRole))
			Expect(contextMessage.Content).To(Equal(chatContext))
		})
	})
	when("InjectMCPContext()", func() {
		var subject *client.Client

		const (
			function = "mock-function"
			version  = "mock-version"
			param    = "mock-param"
			value    = "mock-value"
			apifyKey = "mock-key"
			endpoint = client.ApifyURL + function + client.ApifyPath
		)

		req := api.MCPRequest{
			Provider: utils.ApifyProvider,
			Function: function,
			Version:  version,
			Params: map[string]interface{}{
				param: value,
			},
		}

		it.Before(func() {
			subject = factory.buildClientWithoutConfig()
			subject.Config.ApifyAPIKey = apifyKey
		})

		it("throws an error when the apify API key is missing and the apify provider is used", func() {
			subject.Config.ApifyAPIKey = ""

			err := subject.InjectMCPContext(req)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(utils.ApifyProvider))
		})
		it("is not reliant on specific provider casing", func() {
			subject.Config.ApifyAPIKey = ""

			req.Provider = "ApIfY"

			err := subject.InjectMCPContext(req)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(utils.ApifyProvider))
		})
		it("throws an error when history tracking is disabled", func() {
			subject.Config.OmitHistory = true

			err := subject.InjectMCPContext(req)
			Expect(err).To(HaveOccurred())
			Expect(err).To(MatchError(client.ErrHistoryTracking))
		})
		it("throws an error when the provider is not supported", func() {
			req.Provider = "not-supported"

			err := subject.InjectMCPContext(req)
			Expect(err).To(HaveOccurred())
			Expect(err).To(MatchError(client.ErrUnsupportedProvider))
		})
		it("throws an error when the http call fails", func() {
			msg := "error message"

			mockCaller.EXPECT().
				PostWithHeaders(endpoint, gomock.Any(), gomock.Any()).Return(nil, errors.New(msg))

			err := subject.InjectMCPContext(req)
			Expect(err).To(HaveOccurred())
			Expect(err).To(MatchError(msg))
		})
		it("throws an error when history writing fails", func() {
			mockCaller.EXPECT().
				PostWithHeaders(endpoint, gomock.Any(), gomock.Any()).Return([]byte(`{"key":"value"}`), nil)

			mockHistoryStore.EXPECT().Read().Times(1)
			mockTimer.EXPECT().Now().Times(2)

			msg := "error message"
			mockHistoryStore.EXPECT().Write(gomock.Any()).Return(errors.New(msg))

			err := subject.InjectMCPContext(req)
			Expect(err).To(HaveOccurred())
			Expect(err).To(MatchError(msg))
		})
		it("adds the formatted MCP response to history (array data)", func() {
			mockCaller.EXPECT().
				PostWithHeaders(endpoint, gomock.Any(), gomock.Any()).
				Return([]byte(`[{"temperature":"15C","condition":"Sunny"}]`), nil)

			mockHistoryStore.EXPECT().Read().Times(1)
			mockTimer.EXPECT().Now().Times(2)

			mockHistoryStore.EXPECT().Write(gomock.Any()).
				DoAndReturn(func(h []history.History) error {
					Expect(len(h)).To(Equal(2))
					last := h[len(h)-1]
					Expect(last.Message.Role).To(Equal("function"))
					Expect(last.Message.Name).To(Equal("mock-function"))
					Expect(last.Message.Content).To(ContainSubstring("Temperature: 15C"))
					Expect(last.Message.Content).To(ContainSubstring("Condition: Sunny"))
					Expect(last.Message.Content).To(ContainSubstring("[MCP: mock-function]"))
					return nil
				})

			err := subject.InjectMCPContext(req)
			Expect(err).NotTo(HaveOccurred())
		})
		it("adds the formatted MCP response to history (single object)", func() {
			mockCaller.EXPECT().
				PostWithHeaders(endpoint, gomock.Any(), gomock.Any()).
				Return([]byte(`{"foo":"bar","baz":"qux"}`), nil)

			mockHistoryStore.EXPECT().Read().Times(1)
			mockTimer.EXPECT().Now().Times(2)

			mockHistoryStore.EXPECT().Write(gomock.Any()).
				DoAndReturn(func(h []history.History) error {
					Expect(len(h)).To(Equal(2))
					Expect(h[len(h)-1].Message.Content).To(ContainSubstring("Foo: bar"))
					Expect(h[len(h)-1].Message.Content).To(ContainSubstring("Baz: qux"))
					Expect(h[len(h)-1].Message.Role).To(Equal("function"))
					Expect(h[len(h)-1].Message.Name).To(Equal("mock-function"))
					return nil
				})

			err := subject.InjectMCPContext(req)
			Expect(err).NotTo(HaveOccurred())
		})
		it("adds fallback message when array response is empty", func() {
			mockCaller.EXPECT().
				PostWithHeaders(endpoint, gomock.Any(), gomock.Any()).
				Return([]byte(`[]`), nil)

			mockHistoryStore.EXPECT().Read().Times(1)
			mockTimer.EXPECT().Now().Times(2)

			mockHistoryStore.EXPECT().Write(gomock.Any()).
				DoAndReturn(func(h []history.History) error {
					Expect(h[len(h)-1].Message.Content).To(ContainSubstring("no data returned"))
					return nil
				})

			err := subject.InjectMCPContext(req)
			Expect(err).NotTo(HaveOccurred())
		})
		it("adds fallback message when array contains non-object items", func() {
			mockCaller.EXPECT().
				PostWithHeaders(endpoint, gomock.Any(), gomock.Any()).
				Return([]byte(`[42, true, "string"]`), nil)

			mockHistoryStore.EXPECT().Read().Times(1)
			mockTimer.EXPECT().Now().Times(2)

			mockHistoryStore.EXPECT().Write(gomock.Any()).
				DoAndReturn(func(h []history.History) error {
					Expect(h[len(h)-1].Message.Content).To(ContainSubstring("unexpected response format"))
					return nil
				})

			err := subject.InjectMCPContext(req)
			Expect(err).NotTo(HaveOccurred())
		})
		it("adds fallback message when response is invalid JSON", func() {
			mockCaller.EXPECT().
				PostWithHeaders(endpoint, gomock.Any(), gomock.Any()).
				Return([]byte(`{invalid json}`), nil)

			mockHistoryStore.EXPECT().Read().Times(1)
			mockTimer.EXPECT().Now().Times(2)

			mockHistoryStore.EXPECT().Write(gomock.Any()).
				DoAndReturn(func(h []history.History) error {
					Expect(h[len(h)-1].Message.Content).To(ContainSubstring("failed to decode response"))
					return nil
				})

			err := subject.InjectMCPContext(req)
			Expect(err).NotTo(HaveOccurred())
		})
		it("adds fallback message when top-level JSON is a string", func() {
			mockCaller.EXPECT().
				PostWithHeaders(endpoint, gomock.Any(), gomock.Any()).
				Return([]byte(`"hello world"`), nil)

			mockHistoryStore.EXPECT().Read().Times(1)
			mockTimer.EXPECT().Now().Times(2)

			mockHistoryStore.EXPECT().Write(gomock.Any()).
				DoAndReturn(func(h []history.History) error {
					Expect(h[len(h)-1].Message.Content).To(ContainSubstring("unexpected response format"))
					return nil
				})

			err := subject.InjectMCPContext(req)
			Expect(err).NotTo(HaveOccurred())
		})
	})
}

func openDummy() *os.File {
	// Use os.Pipe to get an *os.File without needing a real disk file.
	r, w, _ := os.Pipe()
	go func() {
		_, _ = io.Copy(w, bytes.NewBuffer([]byte("\x89PNG\r\n\x1a\n")))
		_ = w.Close()
	}()
	return r
}

func createBody(messages []api.Message, stream bool) ([]byte, error) {
	req := api.CompletionsRequest{
		Model:            config.Model,
		Messages:         messages,
		Stream:           stream,
		Temperature:      config.Temperature,
		TopP:             config.TopP,
		FrequencyPenalty: config.FrequencyPenalty,
		MaxTokens:        config.MaxTokens,
		PresencePenalty:  config.PresencePenalty,
		Seed:             config.Seed,
	}

	return json.Marshal(req)
}

func createMessages(historyEntries []history.History, query string) []api.Message {
	var messages []api.Message

	if len(historyEntries) == 0 {
		messages = append(messages, api.Message{
			Role:    client.SystemRole,
			Content: config.Role,
		})
	} else {
		for _, entry := range historyEntries {
			messages = append(messages, entry.Message)
		}
	}

	messages = append(messages, api.Message{
		Role:    client.UserRole,
		Content: query,
	})

	return messages
}

type clientFactory struct {
	mockHistoryStore *MockStore
}

func newClientFactory(mhs *MockStore) *clientFactory {
	return &clientFactory{
		mockHistoryStore: mhs,
	}
}

func (f *clientFactory) buildClientWithoutConfig() *client.Client {
	f.mockHistoryStore.EXPECT().SetThread(config.Thread).Times(1)

	c := client.New(mockCallerFactory, f.mockHistoryStore, mockTimer, mockReader, mockWriter, MockConfig(), commandLineMode)

	return c.WithContextWindow(config.ContextWindow)
}

func (f *clientFactory) withoutHistory() {
	f.mockHistoryStore.EXPECT().Read().Return(nil, nil).Times(1)
}

func (f *clientFactory) withHistory(history []history.History) {
	f.mockHistoryStore.EXPECT().Read().Return(history, nil).Times(1)
}

func mockCallerFactory(_ config2.Config) http.Caller {
	return mockCaller
}

func MockConfig() config2.Config {
	return config2.Config{
		Name:                "mock-openai",
		APIKey:              "mock-api-key",
		Model:               "gpt-3.5-turbo",
		MaxTokens:           100,
		ContextWindow:       50,
		Role:                "You are a test assistant.",
		Temperature:         0.7,
		TopP:                0.9,
		FrequencyPenalty:    0.1,
		PresencePenalty:     0.2,
		Thread:              "mock-thread",
		OmitHistory:         false,
		URL:                 "https://api.mock-openai.com",
		CompletionsPath:     "/v1/test/completions",
		ModelsPath:          "/v1/test/models",
		AuthHeader:          "MockAuthorization",
		AuthTokenPrefix:     "MockBearer ",
		CommandPrompt:       "[mock-datetime] [Q%counter] [%usage]",
		OutputPrompt:        "[mock-output]",
		AutoCreateNewThread: true,
		TrackTokenUsage:     true,
		SkipTLSVerify:       false,
		Seed:                1,
		Effort:              "low",
		ResponsesPath:       "/v1/responses",
		Voice:               "mock-voice",
		TranscriptionsPath:  "/v1/test/transcriptions",
		SpeechPath:          "/v1/test/speech",
	}
}
