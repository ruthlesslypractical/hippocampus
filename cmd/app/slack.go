// Copyright (c) 2026 Ruthlessly Practical LLC. All rights reserved.
// Use of this source code is governed by a BSD-3-Clause license
// that can be found in the LICENSE file.

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/slack-go/slack"

	"github.com/ruthlesslypractical/hippocampus/internal/config"
)

// SlackChannelInfo is returned to the frontend.
type SlackChannelInfo struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Mode     string   `json:"mode"`
	Tags     []string `json:"tags"`
	Backfill bool     `json:"backfill"`
}

// SlackWorkspaceChannel represents a channel available in the Slack workspace.
type SlackWorkspaceChannel struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Private bool   `json:"private"`
	Members int    `json:"members"`
}

// GetSlackChannels returns the configured Slack channels.
func (a *App) GetSlackChannels() []SlackChannelInfo {
	var channels []SlackChannelInfo
	for _, ch := range a.fullConfig.Slack.Channels {
		mode := ch.Mode
		if mode == "" {
			mode = "archive"
		}
		channels = append(channels, SlackChannelInfo{
			ID:       ch.ID,
			Name:     ch.Name,
			Mode:     mode,
			Tags:     ch.Tags,
			Backfill: ch.Backfill,
		})
	}
	return channels
}

// AddSlackChannel adds a channel to the Slack config and persists.
func (a *App) AddSlackChannel(id, name, mode string) error {
	if id == "" || name == "" {
		return fmt.Errorf("channel ID and name are required")
	}
	if mode == "" {
		mode = "archive"
	}
	// Check for duplicates
	for _, ch := range a.fullConfig.Slack.Channels {
		if ch.ID == id {
			return fmt.Errorf("channel %s already configured", id)
		}
	}
	a.fullConfig.Slack.Channels = append(a.fullConfig.Slack.Channels, config.SlackChannel{
		ID:   id,
		Name: name,
		Mode: mode,
	})
	a.saveConfig()
	return nil
}

// RemoveSlackChannel removes a channel from the Slack config and persists.
func (a *App) RemoveSlackChannel(id string) error {
	var filtered []config.SlackChannel
	found := false
	for _, ch := range a.fullConfig.Slack.Channels {
		if ch.ID == id {
			found = true
			continue
		}
		filtered = append(filtered, ch)
	}
	if !found {
		return fmt.Errorf("channel %s not found", id)
	}
	a.fullConfig.Slack.Channels = filtered
	a.saveConfig()
	return nil
}

// SetSlackChannelTags updates the autotags for a specific channel.
func (a *App) SetSlackChannelTags(channelID string, tags []string) error {
	found := false
	for i, ch := range a.fullConfig.Slack.Channels {
		if ch.ID == channelID {
			a.fullConfig.Slack.Channels[i].Tags = tags
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("channel %s not found", channelID)
	}

	// Ensure new tags are registered in Redis tags:all
	if a.redisClient != nil && len(tags) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		for _, tag := range tags {
			a.redisClient.SAdd(ctx, "tags:all", tag)
		}
	}

	a.saveConfig()
	return nil
}

// SetSlackChannelBackfill toggles the backfill setting for a channel.
func (a *App) SetSlackChannelBackfill(channelID string, enabled bool) error {
	found := false
	for i, ch := range a.fullConfig.Slack.Channels {
		if ch.ID == channelID {
			a.fullConfig.Slack.Channels[i].Backfill = enabled
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("channel %s not found", channelID)
	}
	a.saveConfig()
	return nil
}

// BackfillSlackChannel ingests all history from a Slack channel into memory.
// Respects rate limits and creates thread links between related messages.
func (a *App) BackfillSlackChannel(channelID string) (int, error) {
	if a.fullConfig.Slack.BotToken == "" {
		return 0, fmt.Errorf("bot token not configured")
	}
	if a.redisClient == nil {
		return 0, fmt.Errorf("not connected to Redis")
	}

	// Find channel config for autotags
	var channelCfg *config.SlackChannel
	for i, ch := range a.fullConfig.Slack.Channels {
		if ch.ID == channelID {
			channelCfg = &a.fullConfig.Slack.Channels[i]
			break
		}
	}
	if channelCfg == nil {
		return 0, fmt.Errorf("channel %s not configured", channelID)
	}

	api := slack.New(a.fullConfig.Slack.BotToken)
	ctx := context.Background()

	count := 0
	cursor := ""

	for {
		params := &slack.GetConversationHistoryParameters{
			ChannelID: channelID,
			Limit:     200,
			Cursor:    cursor,
		}
		history, err := api.GetConversationHistory(params)
		if err != nil {
			if count > 0 {
				return count, fmt.Errorf("partial backfill (%d messages): %w", count, err)
			}
			return 0, fmt.Errorf("fetching history: %w", err)
		}

		for _, msg := range history.Messages {
			if msg.SubType != "" || msg.Text == "" {
				continue
			}

			tags := []string{
				"content:slack",
				"source:slack",
				fmt.Sprintf("slack:channel:%s", channelID),
				fmt.Sprintf("slack:user:%s", msg.User),
			}

			// Apply channel autotags
			if len(channelCfg.Tags) > 0 {
				tags = append(tags, channelCfg.Tags...)
			}

			// Thread tagging + linking
			if msg.ThreadTimestamp != "" && msg.ThreadTimestamp != msg.Timestamp {
				tags = append(tags, fmt.Sprintf("slack:thread:%s", msg.ThreadTimestamp))
			}

			id := fmt.Sprintf("slack:%s:%s", channelID, msg.Timestamp)

			entry := map[string]interface{}{
				"id":        id,
				"content":   msg.Text,
				"tags":      strings.Join(tags, ","),
				"timestamp": time.Now().Unix(),
			}

			pipe := a.redisClient.Pipeline()
			pipe.HSet(ctx, "entry:"+id, entry)
			pipe.ZAdd(ctx, "timeline", redis.Z{Score: float64(time.Now().Unix()), Member: id})
			for _, tag := range tags {
				pipe.SAdd(ctx, "tag:"+tag, id)
				pipe.SAdd(ctx, "tags:all", tag)
			}
			if _, err := pipe.Exec(ctx); err != nil {
				continue
			}

			// Create thread links
			if msg.ThreadTimestamp != "" && msg.ThreadTimestamp != msg.Timestamp {
				parentID := fmt.Sprintf("slack:%s:%s", channelID, msg.ThreadTimestamp)
				a.redisClient.HSet(ctx, "links:"+parentID, id, "0.7000|thread")
				a.redisClient.HSet(ctx, "links:"+id, parentID, "0.7000|thread")
			}

			count++
		}

		if !history.HasMore {
			break
		}
		cursor = history.ResponseMetaData.NextCursor

		// Rate limit: Slack Tier 3 is ~50 req/min, sleep 1.5s between pages
		time.Sleep(1500 * time.Millisecond)
	}

	return count, nil
}

// GetSlackTokens returns whether tokens are configured (not the actual values).
func (a *App) GetSlackTokens() map[string]bool {
	return map[string]bool{
		"bot_token": a.fullConfig.Slack.BotToken != "",
		"app_token": a.fullConfig.Slack.AppToken != "",
	}
}

// SaveSlackTokens saves the Slack bot and app tokens.
func (a *App) SaveSlackTokens(botToken, appToken string) error {
	a.fullConfig.Slack.BotToken = botToken
	a.fullConfig.Slack.AppToken = appToken
	a.saveConfig()
	return nil
}

// ListSlackWorkspaceChannels fetches channels the bot has access to via Slack API.
// Requires bot_token to be configured.
func (a *App) ListSlackWorkspaceChannels() ([]SlackWorkspaceChannel, error) {
	if a.fullConfig.Slack.BotToken == "" {
		return nil, fmt.Errorf("bot token not configured — set tokens first")
	}

	// Already-configured channels (to exclude from list)
	configured := make(map[string]bool)
	for _, ch := range a.fullConfig.Slack.Channels {
		configured[ch.ID] = true
	}

	api := slack.New(a.fullConfig.Slack.BotToken)

	params := &slack.GetConversationsParameters{
		Types:           []string{"public_channel", "private_channel"},
		ExcludeArchived: true,
		Limit:           200,
	}
	channels, _, err := api.GetConversations(params)
	if err != nil {
		return nil, fmt.Errorf("Slack API error: %w", err)
	}

	var result []SlackWorkspaceChannel
	for _, ch := range channels {
		if configured[ch.ID] {
			continue
		}
		result = append(result, SlackWorkspaceChannel{
			ID:      ch.ID,
			Name:    "#" + ch.Name,
			Private: ch.IsPrivate,
			Members: ch.NumMembers,
		})
	}
	return result, nil
}

// OpenSlackAppSetup opens the Slack API app creation page in the user's browser.
func (a *App) OpenSlackAppSetup() {
	url := "https://api.slack.com/apps"
	exec.Command("open", url).Start()
}

// StartSlackBot starts the hippocampus-slack bot process.
func (a *App) StartSlackBot() error {
	if a.slackCmd != nil && a.slackCmd.Process != nil {
		// Already running
		return nil
	}

	if a.fullConfig.Slack.BotToken == "" || a.fullConfig.Slack.AppToken == "" {
		return fmt.Errorf("Slack tokens not configured")
	}
	if len(a.fullConfig.Slack.Channels) == 0 {
		return fmt.Errorf("no Slack channels configured")
	}

	slackPath := a.bundledBinaryPath("hippocampus-slack")
	if _, err := os.Stat(slackPath); err != nil {
		// Try bin/ in project dir as fallback
		slackPath = filepath.Join(filepath.Dir(os.Args[0]), "..", "Resources", "hippocampus-slack")
		if _, err := os.Stat(slackPath); err != nil {
			return fmt.Errorf("hippocampus-slack binary not found")
		}
	}

	cmd := exec.Command(slackPath, "--config", a.configFilePath(), "-v")
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting Slack bot: %w", err)
	}
	a.slackCmd = cmd

	// Persist enabled state
	a.config.SlackBotEnabled = true
	a.saveConfig()

	// Monitor process in background
	go func() {
		cmd.Wait()
		a.slackCmd = nil
	}()

	return nil
}

// StopSlackBot stops the hippocampus-slack bot process.
func (a *App) StopSlackBot() error {
	if a.slackCmd == nil || a.slackCmd.Process == nil {
		a.config.SlackBotEnabled = false
		a.saveConfig()
		return nil
	}
	if err := a.slackCmd.Process.Kill(); err != nil {
		return fmt.Errorf("stopping Slack bot: %w", err)
	}
	a.slackCmd = nil

	// Persist disabled state
	a.config.SlackBotEnabled = false
	a.saveConfig()

	return nil
}

// IsSlackBotRunning returns whether the Slack bot process is alive.
func (a *App) IsSlackBotRunning() bool {
	return a.slackCmd != nil && a.slackCmd.Process != nil
}
