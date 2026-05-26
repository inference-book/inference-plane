package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/spf13/cobra"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

// Flags scoped to `iplane deployment query`.
//
// The verb is a thin convenience: it Describes the deployment to read
// engine_endpoint, then POSTs /v1/chat/completions to the engine. iplane
// is NOT in the inference data path -- the operator could just as well
// curl the engine_endpoint themselves. The chapter beat is "you can see
// prompt-in / tokens-out without writing a curl invocation."
var (
	queryMaxTokens   int32
	queryTemperature float64
	queryTimeout     time.Duration
	querySystem      string
)

var deploymentQueryCmd = &cobra.Command{
	Use:   "query <id> <prompt>",
	Short: "Send a prompt to a deployed model and print the response",
	Args:  cobra.ExactArgs(2),
	Long: `Send a single-turn chat completion to a RUNNING deployment.

The verb is a convenience wrapper -- it Describes the deployment to
read engine_endpoint, then POSTs /v1/chat/completions to the engine.
iplane is NOT in the inference data path; this is what an operator
would otherwise script with curl.

v0.1 is single-turn (one user message in, one response out). Multi-
turn conversations and streaming responses land in v0.2.

Output:
  text (default): just the assistant's response text, plus a one-line
                  timing summary on stderr.
  json:           the full /v1/chat/completions response body.`,
	Example: `  # Single-turn query
  iplane deployment query my-llama "What is the capital of France?"

  # With a system prompt
  iplane deployment query my-llama "Explain transformers in one sentence" \
      --system "You are a concise ML tutor"

  # Constrain the response length
  iplane deployment query my-llama "Hello" --max-tokens 32

  # JSON output (full response body)
  iplane deployment query my-llama "Hello" -o json`,
	RunE: runDeploymentQuery,
}

func runDeploymentQuery(cmd *cobra.Command, args []string) error {
	id := args[0]
	prompt := args[1]

	client, err := buildDeploymentClient()
	if err != nil {
		return err
	}

	descCtx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
	defer cancel()
	descResp, err := client.DescribeDeployment(descCtx, &provisionerv1.DescribeDeploymentRequest{Id: id})
	if err != nil {
		return fmt.Errorf("describe %q: %w", id, err)
	}
	dep := descResp.GetDeployment()
	if dep.GetState() != provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING {
		return fmt.Errorf("deployment %q is %s, not RUNNING (cannot serve queries)",
			id, strings.TrimPrefix(dep.GetState().String(), "DEPLOYMENT_STATE_"))
	}
	endpoint := dep.GetEngineEndpoint()
	if endpoint == "" {
		return fmt.Errorf("deployment %q has no engine_endpoint (state is %s but endpoint missing)",
			id, dep.GetState())
	}
	modelID := dep.GetModel()

	qctx, qcancel := context.WithTimeout(cmd.Context(), queryTimeout)
	defer qcancel()

	body, elapsed, err := postChatCompletion(qctx, endpoint, modelID, querySystem, prompt, queryMaxTokens, queryTemperature)
	if err != nil {
		return fmt.Errorf("query %q: %w", id, err)
	}
	return renderQueryResponse(cmd, body, elapsed)
}

// chatCompletionRequest is the minimal OpenAI /v1/chat/completions
// shape v0.1 uses. Extensions (tools, response_format, logprobs) land
// when chapters need them.
type chatCompletionRequest struct {
	Model       string         `json:"model"`
	Messages    []chatMessage  `json:"messages"`
	MaxTokens   int32          `json:"max_tokens,omitempty"`
	Temperature float64        `json:"temperature,omitempty"`
	Stream      bool           `json:"stream,omitempty"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// chatCompletionResponse covers the fields the text-output path
// needs (the JSON path passes the raw body through, so missing
// fields don't matter there).
type chatCompletionResponse struct {
	Choices []struct {
		Message      chatMessage `json:"message"`
		FinishReason string      `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

// postChatCompletion sends a single-turn chat completion and returns
// the raw response body + wall-clock elapsed. Returns an error on
// non-2xx so the caller can surface the engine's failure verbatim.
func postChatCompletion(ctx context.Context, endpoint, modelID, system, user string, maxTokens int32, temperature float64) ([]byte, time.Duration, error) {
	msgs := []chatMessage{}
	if system != "" {
		msgs = append(msgs, chatMessage{Role: "system", Content: system})
	}
	msgs = append(msgs, chatMessage{Role: "user", Content: user})

	reqBody := chatCompletionRequest{
		Model:       modelID,
		Messages:    msgs,
		MaxTokens:   maxTokens,
		Temperature: temperature,
	}
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, 0, fmt.Errorf("encode request: %w", err)
	}

	url := strings.TrimRight(endpoint, "/") + "/v1/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, 0, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	started := time.Now()
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return nil, time.Since(started), fmt.Errorf("POST %s: %w", url, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	elapsed := time.Since(started)
	if resp.StatusCode/100 != 2 {
		return respBody, elapsed, fmt.Errorf("%s -> HTTP %d: %s", url, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return respBody, elapsed, nil
}

func renderQueryResponse(cmd *cobra.Command, body []byte, elapsed time.Duration) error {
	out := cmd.OutOrStdout()
	if deploymentOutput == outputJSON {
		// Pretty-print the raw response body so operators can pipe to jq.
		var pretty bytes.Buffer
		if err := json.Indent(&pretty, body, "", "  "); err != nil {
			_, _ = out.Write(body)
			return nil
		}
		_, _ = out.Write(pretty.Bytes())
		_, _ = io.WriteString(out, "\n")
		return nil
	}

	// Text mode: just the assistant's response on stdout, timing on stderr.
	var parsed chatCompletionResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return fmt.Errorf("decode response: %w (body: %s)", err, strings.TrimSpace(string(body)))
	}
	if len(parsed.Choices) == 0 {
		return fmt.Errorf("response had no choices (body: %s)", strings.TrimSpace(string(body)))
	}
	fmt.Fprintln(out, parsed.Choices[0].Message.Content)
	// Timing + token summary on stderr so `iplane deployment query ... > out.txt`
	// captures only the response text.
	fmt.Fprintf(cmd.ErrOrStderr(),
		"\n(%s · prompt %d tok · completion %d tok · finish %s)\n",
		elapsed.Round(time.Millisecond),
		parsed.Usage.PromptTokens,
		parsed.Usage.CompletionTokens,
		parsed.Choices[0].FinishReason)
	return nil
}

func init() {
	deploymentCmd.AddCommand(deploymentQueryCmd)
	f := deploymentQueryCmd.Flags()
	f.StringVar(&querySystem, "system", "",
		`optional system prompt prepended to the conversation`)
	f.Int32Var(&queryMaxTokens, "max-tokens", 256,
		`cap on completion tokens (vLLM honors this; some engines may ignore)`)
	f.Float64Var(&queryTemperature, "temperature", 0.7,
		`sampling temperature; 0 = deterministic, higher = more random`)
	f.DurationVar(&queryTimeout, "timeout", 60*time.Second,
		`max wall-clock time to wait for the engine's response`)
}
