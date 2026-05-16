// Package executor — Kiro streaming state machine.
//
// This file factors the Kiro streaming pipeline out of kiro_executor.go's
// streamToChannel into a state-machine object with explicit methods for each
// stage. The previous implementation was a single ~200-line function with 10
// closures sharing 14 local variables; nothing was independently testable
// and any bug required reading the full body to understand state flow.
//
// kiroStreamState's methods are roughly:
//
//   - emit / ensureMessageStart / closeText / closeThinking — translator side
//     effects (unchanged behaviour, just moved off the stack).
//   - processEvent — handle a single parsed upstream event: merge usage,
//     route content/thinking/toolUses to the right block emitter.
//   - emitToolUseBlock — dedupe + emit a tool_use content block.
//   - finalize — the defer logic: close open blocks, infer stop_reason,
//     run the input/output token estimator, publish the usage record.
//   - handleReadError / handleParseError — bail-out paths.
//
// The driver loop in streamToChannel then reads from the reader and calls
// these methods in sequence; the loop itself is small enough to reason
// about at a glance.
package executor

import (
	"bufio"
	"context"
	"encoding/json"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/runtime/executor/helps"
	kiroclaude "github.com/router-for-me/CLIProxyAPI/v6/internal/translator/kiro/claude"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	log "github.com/sirupsen/logrus"
)

// kiroStreamState encapsulates the mutable state of one in-flight Kiro
// streaming response. One instance per request; not safe for concurrent use.
type kiroStreamState struct {
	ctx context.Context

	// Wiring supplied by the executor.
	executor        *KiroExecutor
	out             chan<- cliproxyexecutor.StreamChunk
	targetFormat    sdktranslator.Format
	model           string
	originalReq     []byte
	kiroReq         []byte
	reporter        *helps.UsageReporter
	promptCachePlan *kiroPromptCachePlan

	// Translator param carried across chunks (stateful translator SDK).
	translatorParam any

	// Block bookkeeping — which content_block index is currently open and
	// what type it is. Kiro's protocol requires a `content_block_stop` for
	// every `content_block_start`, so we track open blocks explicitly
	// rather than inferring from event type.
	messageStartSent  bool
	textBlockOpen     bool
	thinkingBlockOpen bool
	contentBlockIndex int
	hasToolUses       bool

	// Usage accounting.
	totalUsage         usage.Detail
	accumulatedContent strings.Builder
	emittedToolUses    []kiroclaude.KiroToolUse

	// Tool-use dedupe set; Kiro sometimes resends the same tool_use ID
	// across multiple events if the response is retried upstream.
	processedToolIDs map[string]bool
	currentToolUse   *kiroclaude.ToolUseState

	// Stop reason captured from an upstream event or inferred at finalize.
	stopReason string

	// Set to true on read/parse error so finalize() knows not to emit the
	// message_delta/message_stop pair that would normally close the stream.
	streamFailed bool

	// Latched the first time a ctx-aware send observes ctx.Done. After that
	// all further emits are no-ops so the pipeline drains quickly without
	// per-call ctx checks piling up.
	cancelled bool
}

// newKiroStreamState constructs the state. contentBlockIndex starts at -1 so
// the first `++` lands on 0.
func newKiroStreamState(
	ctx context.Context,
	executor *KiroExecutor,
	out chan<- cliproxyexecutor.StreamChunk,
	targetFormat sdktranslator.Format,
	model string,
	originalReq, kiroReq []byte,
	reporter *helps.UsageReporter,
) *kiroStreamState {
	return &kiroStreamState{
		ctx:               ctx,
		executor:          executor,
		out:               out,
		targetFormat:      targetFormat,
		model:             model,
		originalReq:       originalReq,
		kiroReq:           kiroReq,
		reporter:          reporter,
		contentBlockIndex: -1,
		processedToolIDs:  make(map[string]bool),
	}
}

// emit runs a raw Kiro-shaped event through the translator and ships the
// resulting chunk(s) to the output channel. Sends are ctx-aware: when the
// client disconnects (ctx.Done fires) we stop attempting to send so a slow
// / dead reader cannot pin this goroutine and its upstream HTTP connection
// forever. cancelled is latched the first time we observe ctx cancellation
// so the rest of the pipeline can drain quickly without re-checking ctx on
// every emit.
func (s *kiroStreamState) emit(raw []byte) {
	if s.cancelled {
		return
	}
	chunks := sdktranslator.TranslateStream(s.ctx, sdktranslator.FromString("kiro"), s.targetFormat, s.model, s.originalReq, s.kiroReq, raw, &s.translatorParam)
	for _, chunk := range chunks {
		if !s.sendChunk(cliproxyexecutor.StreamChunk{Payload: chunk}) {
			return
		}
	}
}

// sendChunk performs a ctx-aware channel send. Returns false when the send
// was abandoned because ctx was cancelled; callers should treat that as
// "stream over, stop doing work". The method is the single place where we
// touch `out <-`; every other path goes through here so the cancel-aware
// invariant only needs to be upheld in one place.
func (s *kiroStreamState) sendChunk(chunk cliproxyexecutor.StreamChunk) bool {
	if s.cancelled {
		return false
	}
	select {
	case <-s.ctx.Done():
		s.cancelled = true
		return false
	case s.out <- chunk:
		return true
	}
}

// ensureMessageStart emits the Claude `message_start` event exactly once.
// Required for clients that parse the SSE protocol strictly (including
// Claude Code itself).
func (s *kiroStreamState) ensureMessageStart() {
	if s.messageStartSent {
		return
	}
	s.emit(kiroclaude.BuildClaudeMessageStartEvent(s.model, s.totalUsage))
	s.messageStartSent = true
}

// closeText emits a content_block_stop for the currently-open text block.
// No-op if no text block is open.
func (s *kiroStreamState) closeText() {
	if !s.textBlockOpen {
		return
	}
	s.emit(kiroclaude.BuildClaudeContentBlockStopEvent(s.contentBlockIndex))
	s.textBlockOpen = false
}

// closeThinking closes an open thinking block.
func (s *kiroStreamState) closeThinking() {
	if !s.thinkingBlockOpen {
		return
	}
	s.emit(kiroclaude.BuildClaudeThinkingBlockStopEvent(s.contentBlockIndex))
	s.thinkingBlockOpen = false
}

// emitToolUseBlock streams a tool_use content block (start/input_json/stop)
// and records the tool for later token estimation. Dedupes on toolUseID
// when `mark` is true — the caller passes false only for already-marked
// tools (e.g. those emitted by the accumulator when a streamed tool
// completes).
func (s *kiroStreamState) emitToolUseBlock(toolUse kiroclaude.KiroToolUse, mark bool) {
	if toolUse.IsTruncated {
		log.Warnf("kiro: streamToChannel skipping truncated tool: %s (ID: %s)", toolUse.Name, toolUse.ToolUseID)
		return
	}
	if mark && toolUse.ToolUseID != "" {
		if s.processedToolIDs[toolUse.ToolUseID] {
			return
		}
		s.processedToolIDs[toolUse.ToolUseID] = true
	}
	s.closeThinking()
	s.closeText()
	s.contentBlockIndex++
	s.emit(kiroclaude.BuildClaudeContentBlockStartEvent(s.contentBlockIndex, "tool_use", toolUse.ToolUseID, toolUse.Name))
	inputBytes, _ := json.Marshal(toolUse.Input)
	s.emit(kiroclaude.BuildClaudeInputJsonDeltaEvent(string(inputBytes), s.contentBlockIndex))
	s.emit(kiroclaude.BuildClaudeContentBlockStopEvent(s.contentBlockIndex))
	s.hasToolUses = true
	// Only successfully emitted (non-truncated, non-duplicate) tools reach
	// this point; the emittedToolUses slice therefore matches what the
	// client actually sees, which is what the token estimator cares about.
	s.emittedToolUses = append(s.emittedToolUses, toolUse)
}

// processEvent ingests one parsed Kiro event and emits the corresponding
// Claude-shaped blocks. Must be called with ensureMessageStart already run
// (the driver loop does this).
func (s *kiroStreamState) processEvent(event parsedKiroEvent) {
	if event.stopReason != "" {
		s.stopReason = event.stopReason
	}
	mergeKiroUsage(&s.totalUsage, event.usage)

	// Some upstreams embed tool_use JSON inside a plain content string.
	// Split those back out so downstream sees them as structured tool_use.
	if event.content != "" {
		cleanedContent, embeddedToolUses := kiroclaude.ParseEmbeddedToolCalls(event.content, s.processedToolIDs)
		event.content = cleanedContent
		event.toolUses = append(event.toolUses, embeddedToolUses...)
	}

	if event.thinking != "" {
		s.closeText()
		if !s.thinkingBlockOpen {
			s.contentBlockIndex++
			s.emit(kiroclaude.BuildClaudeContentBlockStartEvent(s.contentBlockIndex, "thinking", "", ""))
			s.thinkingBlockOpen = true
		}
		s.emit(kiroclaude.BuildClaudeThinkingDeltaEvent(event.thinking, s.contentBlockIndex))
	}
	if event.content != "" {
		s.closeThinking()
		if !s.textBlockOpen {
			s.contentBlockIndex++
			s.emit(kiroclaude.BuildClaudeContentBlockStartEvent(s.contentBlockIndex, "text", "", ""))
			s.textBlockOpen = true
		}
		s.accumulatedContent.WriteString(event.content)
		s.emit(kiroclaude.BuildClaudeStreamEvent(event.content, s.contentBlockIndex))
	}
	for _, toolUse := range event.toolUses {
		s.emitToolUseBlock(toolUse, true)
	}
	if event.toolUseEvent != nil {
		completed, newState := kiroclaude.ProcessToolUseEvent(event.toolUseEvent, s.currentToolUse, s.processedToolIDs)
		s.currentToolUse = newState
		for _, toolUse := range completed {
			s.emitToolUseBlock(toolUse, false)
		}
	}
}

// flushTrailingToolUse emits any in-progress tool_use accumulated across
// multiple toolUseEvent fragments when the stream ends. Kiro sometimes
// omits the final `stop: true` marker, so we need an explicit flush.
func (s *kiroStreamState) flushTrailingToolUse() {
	if flushed, ok := flushKiroToolUseState(s.currentToolUse, s.processedToolIDs); ok {
		s.emitToolUseBlock(flushed, false)
		s.currentToolUse = nil
	}
}

// handleReadError is the bail-out path for a frame-reader error. It records
// ordinary errors for log persistence, notifies the usage reporter of a
// failure, and forwards the error to the client via the StreamChunk channel.
// Uses the ctx-aware sender so we don't deadlock when the client has
// disconnected (which is often exactly why the read failed in the first place).
func (s *kiroStreamState) handleReadError(err error) {
	transientCapacity := isKiroTransientModelCapacity429Error(err)
	if !transientCapacity {
		helps.RecordAPIResponseError(s.ctx, s.executor.cfg, err)
	}
	if s.reporter != nil && !transientCapacity {
		s.reporter.PublishFailureWithError(s.ctx, err)
	}
	s.streamFailed = true
	s.sendChunk(cliproxyexecutor.StreamChunk{Err: err})
}

// handleParseError mirrors handleReadError but for per-event parse errors
// returned inside the parsedKiroEvent. Same semantics, same ctx-aware send.
func (s *kiroStreamState) handleParseError(err error) {
	transientCapacity := isKiroTransientModelCapacity429Error(err)
	if !transientCapacity {
		helps.RecordAPIResponseError(s.ctx, s.executor.cfg, err)
		log.Warnf("kiro: upstream event stream failed: %v", err)
	}
	if s.reporter != nil && !transientCapacity {
		s.reporter.PublishFailureWithError(s.ctx, err)
	}
	s.streamFailed = true
	s.sendChunk(cliproxyexecutor.StreamChunk{Err: err})
}

// finalize runs the defer logic: close any open blocks, infer a missing
// stop_reason, estimate usage if the upstream omitted it, emit the terminal
// message_delta / message_stop pair, and publish the usage record. Called
// unconditionally from streamToChannel's defer; checks streamFailed to skip
// the success-path emits when the stream errored out.
func (s *kiroStreamState) finalize() {
	if s.streamFailed {
		return
	}
	s.closeThinking()
	s.closeText()
	if !s.messageStartSent {
		s.ensureMessageStart()
	}
	if s.stopReason == "" {
		// Infer a sensible default based on what we emitted. Claude clients
		// use stop_reason to decide whether to pass the response back to the
		// model (tool_use) or return it to the user (end_turn).
		if s.hasToolUses {
			s.stopReason = "tool_use"
		} else {
			s.stopReason = "end_turn"
		}
	}
	s.estimateMissingUsage()
	if applyKiroPromptCachePlan(&s.totalUsage, s.promptCachePlan) {
		log.WithFields(kiroPromptCacheUsageLogKV(s.totalUsage, s.promptCachePlan)).Debug("kiro: usage augmented from prompt cache tracker (stream)")
	}
	markKiroPromptCachePlanSuccess(s.promptCachePlan)
	s.emit(kiroclaude.BuildClaudeMessageDeltaEvent(s.stopReason, s.totalUsage))
	s.emit(kiroclaude.BuildClaudeMessageStopOnlyEvent())
	if s.reporter != nil {
		s.reporter.Publish(s.ctx, s.totalUsage)
		s.reporter.EnsurePublished(s.ctx)
	}
}

// estimateMissingUsage fills totalUsage.InputTokens and OutputTokens when
// the upstream didn't supply them. Output uses a tiktoken estimate over the
// accumulated text + emitted tool_use JSON; input uses the Kiro request
// payload. Emits a debug log when any estimate ran so operators can tell
// "upstream-reported" from "locally estimated" numbers.
func (s *kiroStreamState) estimateMissingUsage() {
	outputEstimated := false
	if s.totalUsage.OutputTokens == 0 && (s.accumulatedContent.Len() > 0 || len(s.emittedToolUses) > 0) {
		if est := kiroEstimateOutputTokens(s.accumulatedContent.String(), s.emittedToolUses); est > 0 {
			s.totalUsage.OutputTokens = est
			outputEstimated = true
		} else {
			// Fall back to the historical length/4 heuristic only when the
			// tokenizer came back with zero (e.g. content consisted solely
			// of whitespace the tokenizer collapses).
			s.totalUsage.OutputTokens = int64(s.accumulatedContent.Len() / 4)
			if s.totalUsage.OutputTokens == 0 {
				s.totalUsage.OutputTokens = 1
			}
			outputEstimated = true
		}
	}
	inputEstimated := false
	if s.totalUsage.InputTokens == 0 {
		if est := kiroEstimateInputTokensFromKiroRequest(s.kiroReq); est > 0 {
			s.totalUsage.InputTokens = est
			inputEstimated = true
		}
	}
	componentTotal := s.totalUsage.InputTokens + s.totalUsage.OutputTokens + s.totalUsage.ReasoningTokens
	if componentTotal > 0 && s.totalUsage.TotalTokens < componentTotal {
		s.totalUsage.TotalTokens = componentTotal
	}
	if inputEstimated || outputEstimated {
		log.WithFields(kiroUsageEstimateLogKV(s.totalUsage, kiroUsageEstimateReport{
			FilledInput:  inputEstimated,
			FilledOutput: outputEstimated,
		})).Debug("kiro: usage filled from local estimator (stream)")
	}
}

// readNextFrame is a thin convenience wrapper so the driver loop can share
// the same read-logic with existing helpers.
func (s *kiroStreamState) readNextFrame(reader *bufio.Reader) (*kiroEventStreamMessage, error) {
	return s.executor.readEventStreamMessage(reader)
}
