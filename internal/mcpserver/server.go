// Package mcpserver registers the service's MCP tools: speak and stop, the
// voicing path Miranda's orchestrator uses for replies that did not arrive
// through Home Assistant's Assist pipeline. The pipeline path itself does not
// go through MCP at all — Home Assistant talks to the Wyoming server.
package mcpserver

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/archer-developer/miranda-tts/internal/speaker"
)

const (
	serverName    = "miranda-tts"
	serverVersion = "0.1.0"
)

// Speaker is what the tools need from the playback layer.
type Speaker interface {
	Speak(ctx context.Context, entityID, text, voice string) (speaker.Result, error)
	Stop(ctx context.Context, entityID string) (bool, error)
}

// New builds the MCP server with all tools registered. A nil logger falls
// back to slog.Default().
func New(logger *slog.Logger, sp Speaker) *mcp.Server {
	if logger == nil {
		logger = slog.Default()
	}

	server := mcp.NewServer(&mcp.Implementation{Name: serverName, Version: serverVersion}, nil)

	mcp.AddTool(server, &mcp.Tool{
		Name: "speak",
		Description: "Speak text aloud on a Home Assistant speaker. entity_id selects the device and the mechanism: " +
			"an assist_satellite.* entity announces the text through its voice pipeline, a media_player.* entity " +
			"plays a synthesized clip. Blocks until playback finishes or is interrupted.",
	}, speakHandler(logger, sp))

	mcp.AddTool(server, &mcp.Tool{
		Name: "stop",
		Description: "Interrupt speech started by the speak tool on a Home Assistant entity. Works for media_player.* " +
			"entities; assist_satellite.* announcements cannot be interrupted and report stopped=false.",
	}, stopHandler(logger, sp))

	return server
}

type SpeakInput struct {
	Text     string `json:"text" jsonschema:"Text to synthesize and play aloud."`
	EntityID string `json:"entity_id" jsonschema:"Home Assistant entity to play on, e.g. media_player.yandex_station_x or assist_satellite.voice_pe_kitchen."`
	Voice    string `json:"voice,omitempty" jsonschema:"Optional Gemini voice name overriding the configured default (media_player only)."`
}

type SpeakOutput struct {
	Played      bool  `json:"played"`
	Interrupted bool  `json:"interrupted,omitempty"`
	DurationMS  int64 `json:"duration_ms,omitempty"`
}

func speakHandler(logger *slog.Logger, sp Speaker) mcp.ToolHandlerFor[SpeakInput, SpeakOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in SpeakInput) (*mcp.CallToolResult, SpeakOutput, error) {
		if strings.TrimSpace(in.Text) == "" {
			return nil, SpeakOutput{}, fmt.Errorf("speak: text must not be empty")
		}
		if strings.TrimSpace(in.EntityID) == "" {
			return nil, SpeakOutput{}, fmt.Errorf("speak: entity_id must not be empty")
		}

		res, err := sp.Speak(ctx, strings.TrimSpace(in.EntityID), in.Text, strings.TrimSpace(in.Voice))
		if err != nil {
			logger.Error("speak failed", "entity_id", in.EntityID, "error", err)
			return nil, SpeakOutput{}, fmt.Errorf("speak: %w", err)
		}
		logger.Info("speak", "entity_id", in.EntityID, "chars", len([]rune(in.Text)), "played", res.Played, "interrupted", res.Interrupted)

		out := SpeakOutput{Played: res.Played, Interrupted: res.Interrupted, DurationMS: res.DurationMS}
		summary := "played"
		if res.Interrupted {
			summary = "interrupted"
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: summary}}}, out, nil
	}
}

type StopInput struct {
	EntityID string `json:"entity_id" jsonschema:"The entity a previous speak call targeted."`
}

type StopOutput struct {
	Stopped bool `json:"stopped"`
}

func stopHandler(logger *slog.Logger, sp Speaker) mcp.ToolHandlerFor[StopInput, StopOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in StopInput) (*mcp.CallToolResult, StopOutput, error) {
		if strings.TrimSpace(in.EntityID) == "" {
			return nil, StopOutput{}, fmt.Errorf("stop: entity_id must not be empty")
		}

		stopped, err := sp.Stop(ctx, strings.TrimSpace(in.EntityID))
		if err != nil {
			logger.Error("stop failed", "entity_id", in.EntityID, "error", err)
			return nil, StopOutput{}, fmt.Errorf("stop: %w", err)
		}
		logger.Info("stop", "entity_id", in.EntityID, "stopped", stopped)

		text := "stopped"
		if !stopped {
			text = "nothing stopped (assist_satellite announcements cannot be interrupted)"
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, StopOutput{Stopped: stopped}, nil
	}
}
