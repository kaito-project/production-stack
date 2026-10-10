package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net"
	"strings"

	extprocpb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"google.golang.org/grpc"
)

type server struct {
	extprocv3.UnimplementedExternalProcessorServer
}

const (
	SECRET_PATTERN = "sk-123456789"
	REDACTED       = "[REDACTED]"

	// For this PoC detector, holding patternLen-1 bytes is sufficient
	// to detect a pattern split across logical content boundaries.
	HOLDBACK_SIZE = len(SECRET_PATTERN) - 1
)

type ChatCompletionChunk struct {
	Choices []Choice `json:"choices"`
}

type Choice struct {
	Delta Delta `json:"delta"`
}

type Delta struct {
	Content string `json:"content,omitempty"`
}

// sendBody sends a FULL_DUPLEX_STREAMED response body back to Envoy.
func sendBody(
	stream extprocv3.ExternalProcessor_ProcessServer,
	body []byte,
	endOfStream bool,
) error {
	return stream.Send(&extprocpb.ProcessingResponse{
		Response: &extprocpb.ProcessingResponse_ResponseBody{
			ResponseBody: &extprocpb.BodyResponse{
				Response: &extprocpb.CommonResponse{
					Status: extprocpb.CommonResponse_CONTINUE,
					BodyMutation: &extprocpb.BodyMutation{
						Mutation: &extprocpb.BodyMutation_StreamedResponse{
							StreamedResponse: &extprocpb.StreamedBodyResponse{
								Body:        body,
								EndOfStream: endOfStream,
							},
						},
					},
				},
			},
		},
	})
}

// splitSSEEvents extracts complete SSE events separated by "\n\n".
// Any incomplete event remains in the returned buffer.
func splitSSEEvents(buf []byte) (events [][]byte, remaining []byte) {
	for {
		idx := bytes.Index(buf, []byte("\n\n"))
		if idx < 0 {
			break
		}

		event := append([]byte(nil), buf[:idx]...)
		events = append(events, event)
		buf = buf[idx+2:]
	}

	remaining = append([]byte(nil), buf...)
	return
}

func isDoneEvent(event []byte) bool {
	return string(bytes.TrimSpace(event)) == "data: [DONE]"
}

// extractContent parses one OpenAI SSE event and returns delta.content.
func extractContent(event []byte) (string, bool, error) {
	event = bytes.TrimSpace(event)

	if !bytes.HasPrefix(event, []byte("data:")) {
		return "", false, nil
	}

	payload := bytes.TrimSpace(bytes.TrimPrefix(event, []byte("data:")))

	if bytes.Equal(payload, []byte("[DONE]")) {
		return "", false, nil
	}

	var chunk ChatCompletionChunk
	if err := json.Unmarshal(payload, &chunk); err != nil {
		return "", false, err
	}

	if len(chunk.Choices) == 0 {
		return "", false, nil
	}

	content := chunk.Choices[0].Delta.Content
	if content == "" {
		return "", false, nil
	}

	return content, true, nil
}

// buildSSE creates a minimal valid OpenAI-compatible SSE delta event.
func buildSSE(content string) ([]byte, error) {
	chunk := ChatCompletionChunk{
		Choices: []Choice{
			{
				Delta: Delta{
					Content: content,
				},
			},
		},
	}

	payload, err := json.Marshal(chunk)
	if err != nil {
		return nil, err
	}

	out := append([]byte("data: "), payload...)
	out = append(out, []byte("\n\n")...)

	return out, nil
}

// sanitizePending applies the PoC secret detector.
func sanitizePending(text string) string {
	if strings.Contains(text, SECRET_PATTERN) {
		log.Printf("[Go] DETECTED SECRET in logical text: %q", text)

		text = strings.ReplaceAll(
			text,
			SECRET_PATTERN,
			REDACTED,
		)

		log.Printf("[Go] REDACTED logical text to: %q", text)
	}

	return text
}

// releaseSafePrefix keeps HOLDBACK_SIZE bytes unconfirmed.
// The released prefix can no longer participate in SECRET_PATTERN.
func releaseSafePrefix(pending string) (safe string, held string) {
	if len(pending) <= HOLDBACK_SIZE {
		return "", pending
	}

	releaseLen := len(pending) - HOLDBACK_SIZE
	return pending[:releaseLen], pending[releaseLen:]
}

func (s *server) Process(
	stream extprocv3.ExternalProcessor_ProcessServer,
) error {
	log.Println("[Go] new ext_proc stream (OpenAI SSE semantic holdback)")

	// Raw transport-level buffer.
	var sseBuffer []byte

	// Logical LLM output buffer.
	var textPending string

	doneSeen := false

	for {
		req, err := stream.Recv()

		if err == io.EOF {
			log.Println("[Go] ext_proc stream EOF")
			return nil
		}

		if err != nil {
			// Envoy may cancel the gRPC stream after HTTP processing finishes.
			log.Printf("[Go] recv ended: %v", err)
			return nil
		}

		switch r := req.Request.(type) {

		case *extprocpb.ProcessingRequest_ResponseHeaders:
			log.Println("[Go] received response headers")

			err = stream.Send(&extprocpb.ProcessingResponse{
				Response: &extprocpb.ProcessingResponse_ResponseHeaders{
					ResponseHeaders: &extprocpb.HeadersResponse{
						Response: &extprocpb.CommonResponse{
							Status: extprocpb.CommonResponse_CONTINUE,
						},
					},
				},
			})

		case *extprocpb.ProcessingRequest_ResponseBody:
			body := r.ResponseBody

			log.Printf(
				"[Go] recv raw body chunk len=%d EndOfStream=%v",
				len(body.Body),
				body.EndOfStream,
			)

			sseBuffer = append(sseBuffer, body.Body...)

			events, remaining := splitSSEEvents(sseBuffer)
			sseBuffer = remaining

			var output []byte

			for _, event := range events {
				log.Printf(
					"[Go] complete SSE event: %q",
					string(event),
				)

				if isDoneEvent(event) {
					log.Println("[Go] received [DONE]")

					// Sanitize and flush all remaining logical text
					// before forwarding [DONE].
					textPending = sanitizePending(textPending)

					if textPending != "" {
						out, buildErr := buildSSE(textPending)
						if buildErr != nil {
							return buildErr
						}

						output = append(output, out...)

						log.Printf(
							"[Go] flushing final logical content: %q",
							textPending,
						)

						textPending = ""
					}

					output = append(
						output,
						[]byte("data: [DONE]\n\n")...,
					)

					doneSeen = true
					continue
				}

				content, ok, parseErr := extractContent(event)

				if parseErr != nil {
					log.Printf(
						"[Go] malformed/unsupported SSE event: %v",
						parseErr,
					)

					// PoC behavior: do not forward unparsed model text.
					continue
				}

				if !ok {
					log.Println(
						"[Go] SSE event has no supported delta.content",
					)
					continue
				}

				log.Printf(
					"[Go] extracted delta.content=%q",
					content,
				)

				// Logical concatenation across SSE events.
				textPending += content

				log.Printf(
					"[Go] logical pending before scan=%q",
					textPending,
				)

				// Scan the logical stream, not raw SSE bytes.
				textPending = sanitizePending(textPending)

				safe, held := releaseSafePrefix(textPending)
				textPending = held

				if safe != "" {
					out, buildErr := buildSSE(safe)
					if buildErr != nil {
						return buildErr
					}

					output = append(output, out...)

					log.Printf(
						"[Go] releasing safe logical prefix=%q holding=%q",
						safe,
						textPending,
					)
				} else {
					log.Printf(
						"[Go] holding logical text=%q",
						textPending,
					)
				}
			}

			if body.EndOfStream {
				log.Println("[Go] HTTP body EndOfStream=true")

				// Handle any incomplete raw SSE data conservatively.
				if len(sseBuffer) > 0 {
					log.Printf(
						"[Go] dropping incomplete SSE bytes at EOS: %q",
						string(sseBuffer),
					)
					sseBuffer = nil
				}

				// If backend ended without [DONE], flush remaining
				// sanitized logical content before HTTP EOS.
				if !doneSeen && textPending != "" {
					textPending = sanitizePending(textPending)

					out, buildErr := buildSSE(textPending)
					if buildErr != nil {
						return buildErr
					}

					output = append(output, out...)

					log.Printf(
						"[Go] EOS flushing logical content=%q",
						textPending,
					)

					textPending = ""
				}

				err = sendBody(stream, output, true)
			} else {
				// Even when we are holding everything, FULL_DUPLEX_STREAMED
				// still receives a corresponding streamed body response.
				err = sendBody(stream, output, false)
			}

		case *extprocpb.ProcessingRequest_ResponseTrailers:
			log.Println("[Go] received response trailers")

			err = stream.Send(&extprocpb.ProcessingResponse{
				Response: &extprocpb.ProcessingResponse_ResponseTrailers{
					ResponseTrailers: &extprocpb.TrailersResponse{
						HeaderMutation: &extprocpb.HeaderMutation{},
					},
				},
			})

		default:
			log.Printf(
				"[Go] ignoring request type %T",
				req.Request,
			)
			continue
		}

		if err != nil {
			log.Printf("[Go] send error: %v", err)
			return err
		}
	}
}

func main() {
	listener, err := net.Listen("tcp", ":9000")
	if err != nil {
		log.Fatal(err)
	}

	grpcServer := grpc.NewServer()

	extprocv3.RegisterExternalProcessorServer(
		grpcServer,
		&server{},
	)

	log.Println(
		"[Go] OpenAI SSE semantic holdback ext_proc listening on :9000",
	)

	log.Printf(
		"[Go] HOLDBACK_SIZE=%d SECRET_PATTERN=%q REDACTED=%q",
		HOLDBACK_SIZE,
		SECRET_PATTERN,
		REDACTED,
	)

	if err := grpcServer.Serve(listener); err != nil {
		log.Fatal(err)
	}
}
