// Copyright (c) 2026 Ruthlessly Practical LLC. All rights reserved.
// Use of this source code is governed by a BSD-3-Clause license
// that can be found in the LICENSE file.

// Command hippocampus-slack runs a Slack bot in Socket Mode that archives
// channel messages to Redis and responds to /hippo slash commands.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
	"github.com/ruthlesslypractical/hippocampus/internal/config"
	"github.com/ruthlesslypractical/hippocampus/internal/logging"
	"github.com/ruthlesslypractical/hippocampus/internal/memory"
	"github.com/ruthlesslypractical/hippocampus/internal/util"
	"github.com/ruthlesslypractical/hippocampus/pkg/ingest"
	"github.com/ruthlesslypractical/hippocampus/pkg/safeguard"
)

func main() {
	configPath := flag.String("config", "", "path to config.json")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("hippocampus-slack v%s\n", config.Version)
		os.Exit(0)
	}

	// Find config: flag > env > standard paths
	path := *configPath
	if path == "" {
		path = config.FindConfigPath()
	}

	cfg, err := config.Load(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config from %s: %v\n", path, err)
		os.Exit(1)
	}

	// Set up structured logging (writes to ~/Library/Logs/Hippocampus/slack.log or configured dir)
	cleanupLog := logging.Setup(cfg, "slack")
	defer cleanupLog()

	slog.Info("slack bot starting",
		"version", config.Version,
		"config", path,
	)

	if cfg.Slack.BotToken == "" || cfg.Slack.AppToken == "" {
		slog.Error("missing required Slack tokens",
			"bot_token_set", cfg.Slack.BotToken != "",
			"app_token_set", cfg.Slack.AppToken != "",
		)
		os.Exit(1)
	}

	// Connect to Redis
	rdb := cfg.Redis.NewRedisClient()
	defer rdb.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := rdb.Ping(ctx).Err(); err != nil {
		slog.Error("redis connection failed", "error", err,
			"addr", cfg.Redis.Addr,
		)
		os.Exit(1)
	}
	slog.Info("redis connected", "addr", cfg.Redis.Addr)

	store, err := memory.NewRedisStore(cfg.Redis, nil)
	if err != nil {
		slog.Error("creating memory store", "error", err)
		os.Exit(1)
	}

	// Build channel lookup set for filtering
	monitoredChannels := make(map[string]config.SlackChannel)
	for _, ch := range cfg.Slack.Channels {
		monitoredChannels[ch.ID] = ch
	}

	if len(monitoredChannels) == 0 {
		slog.Error("no channels configured in slack.channels")
		os.Exit(1)
	}

	// Create Slack client
	api := slack.New(
		cfg.Slack.BotToken,
		slack.OptionAppLevelToken(cfg.Slack.AppToken),
	)

	client := socketmode.New(api,
		socketmode.OptionLog(log.New(os.Stderr, "[slack-socket] ", log.LstdFlags)),
	)

	slog.Info("monitoring channels", "count", len(monitoredChannels))
	for _, ch := range cfg.Slack.Channels {
		slog.Info("channel configured",
			"name", ch.Name,
			"id", ch.ID,
			"mode", ch.Mode,
			"backfill", ch.Backfill,
			"tags", ch.Tags,
		)
	}

	// User name cache — resolves Slack user IDs to display names lazily
	userNames := &userNameCache{
		api:   api,
		cache: make(map[string]string),
	}

	// Handle events
	go func() {
		for evt := range client.Events {
			switch evt.Type {
			case socketmode.EventTypeEventsAPI:
				eventsAPIEvent, ok := evt.Data.(slackevents.EventsAPIEvent)
				if !ok {
					continue
				}
				client.Ack(*evt.Request)
				handleEvent(ctx, store, eventsAPIEvent, monitoredChannels, userNames)

			case socketmode.EventTypeSlashCommand:
				cmd, ok := evt.Data.(slack.SlashCommand)
				if !ok {
					continue
				}
				handleSlashCommand(ctx, store, cfg, client, evt, cmd)

			case socketmode.EventTypeConnecting:
				slog.Info("connecting to Slack...")
			case socketmode.EventTypeConnected:
				slog.Info("connected to Slack (Socket Mode)")
			case socketmode.EventTypeConnectionError:
				slog.Warn("Slack connection error, will retry")
			}
		}
	}()

	// Graceful shutdown
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		s := <-sig
		slog.Info("received signal, shutting down", "signal", s.String())
		cancel()
		os.Exit(0)
	}()

	// Start backfill goroutines for channels that have it enabled
	for _, ch := range cfg.Slack.Channels {
		if ch.Backfill {
			slog.Info("starting backfill", "channel", ch.Name, "id", ch.ID)
			go runBackfill(ctx, rdb, store, slack.New(cfg.Slack.BotToken), ch, userNames)
		}
	}

	slog.Info("entering Socket Mode event loop")
	if err := client.Run(); err != nil {
		slog.Error("socket mode fatal", "error", err)
		os.Exit(1)
	}
}

func handleEvent(ctx context.Context, store memory.Store, event slackevents.EventsAPIEvent, channels map[string]config.SlackChannel, users *userNameCache) {
	switch event.Type {
	case slackevents.CallbackEvent:
		switch ev := event.InnerEvent.Data.(type) {
		case *slackevents.MessageEvent:
			// Only archive from monitored channels
			ch, monitored := channels[ev.Channel]
			if !monitored {
				return
			}

			// Skip bot messages and non-message subtypes (except edits)
			if ev.BotID != "" {
				return
			}
			if ev.SubType != "" && ev.SubType != "message_changed" {
				return
			}

			// For edits, use the edited message content
			text := strings.TrimSpace(ev.Text)
			msgTs := ev.TimeStamp
			if ev.SubType == "message_changed" && ev.Message != nil {
				text = strings.TrimSpace(ev.Message.Text)
				msgTs = ev.Message.Timestamp
				slog.Debug("message edit detected",
					"channel", ch.Name,
					"ts", msgTs,
				)
			}

			if text == "" {
				return
			}

			// Resolve user display name
			userName := users.Resolve(ev.User)

			slog.Debug("message received",
				"channel", ch.Name,
				"user", userName,
				"user_id", ev.User,
				"len", len(text),
				"preview", util.Truncate(text, 80),
			)

			// Run safeguard scan
			result := safeguard.Scan(text)

			// Build entry
			tags := []string{
				"content:slack",
				"source:slack",
				fmt.Sprintf("slack:channel:%s", ev.Channel),
				fmt.Sprintf("slack:channel_name:%s", ch.Name),
				fmt.Sprintf("slack:user:%s", ev.User),
				fmt.Sprintf("slack:user_name:%s", userName),
				fmt.Sprintf("date:%s", time.Now().Format("2006-01-02")),
			}

			// Apply channel-configured autotags
			if len(ch.Tags) > 0 {
				tags = append(tags, ch.Tags...)
			}

			// Thread coalescing: if this is a threaded reply, tag with thread
			if ev.ThreadTimeStamp != "" && ev.ThreadTimeStamp != ev.TimeStamp {
				tags = append(tags, fmt.Sprintf("slack:thread:%s", ev.ThreadTimeStamp))
			}

			// Add safety flag if risky
			if result.RiskScore >= 0.5 {
				tags = append(tags, "safety:flagged")
				slog.Warn("message flagged by safeguard",
					"channel", ch.Name,
					"user", ev.User,
					"risk_score", result.RiskScore,
					"flags", len(result.Flags),
				)
			}

			id := fmt.Sprintf("slack:%s:%s", ev.Channel, msgTs)

			entry := memory.Entry{
				ID:        id,
				Timestamp: parseSlackTimestamp(msgTs),
				Content:   text,
				Tags:      tags,
			}

			if err := store.Put(ctx, entry); err != nil {
				slog.Error("failed to store message",
					"channel", ch.Name,
					"id", id,
					"error", err,
				)
			} else {
				slog.Info("archived message",
					"channel", ch.Name,
					"id", id,
					"tags", len(tags),
				)
			}
		}
	}
}

// lastResults tracks the most recent search/list results per user for short ID references.
var lastResults = make(map[string][]string) // userID → []entryID

func handleSlashCommand(ctx context.Context, store memory.Store, cfg config.Config, client *socketmode.Client, evt socketmode.Event, cmd slack.SlashCommand) {
	parts := strings.SplitN(strings.TrimSpace(cmd.Text), " ", 2)
	action := ""
	arg := ""
	if len(parts) > 0 {
		action = strings.ToLower(parts[0])
	}
	if len(parts) > 1 {
		arg = parts[1]
	}

	var responseText string

	switch action {
	case "search":
		if arg == "" {
			responseText = "Usage: `/hippo search <query>`"
			break
		}
		results, err := store.Search(ctx, arg, 5)
		if err != nil {
			responseText = fmt.Sprintf("❌ Search error: %s", err)
			break
		}
		if len(results) == 0 {
			responseText = fmt.Sprintf("No results for \"%s\"", arg)
			break
		}
		// Store IDs for short reference
		ids := make([]string, len(results))
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("🔍 *Results for \"%s\"* (%d found):\n\n", arg, len(results)))
		for i, r := range results {
			ids[i] = r.Entry.ID
			content := util.Truncate(r.Entry.Content, 200)
			sb.WriteString(fmt.Sprintf("`[%d]` %s\n   _Tags: %s_\n\n", i+1, content, strings.Join(r.Entry.Tags, ", ")))
		}
		lastResults[cmd.UserID] = ids
		sb.WriteString("_Use `/hippo forget <#>` to delete an entry by number._")
		responseText = sb.String()

	case "recent":
		limit := 5
		if arg != "" {
			fmt.Sscanf(arg, "%d", &limit)
			if limit < 1 {
				limit = 1
			}
			if limit > 20 {
				limit = 20
			}
		}
		now := time.Now()
		start := now.Add(-7 * 24 * time.Hour)
		entries, err := store.EntriesByTimeRange(ctx, start.Unix(), now.Unix(), nil, limit)
		if err != nil {
			responseText = fmt.Sprintf("❌ Error: %s", err)
			break
		}
		if len(entries) == 0 {
			responseText = "No recent entries."
			break
		}
		ids := make([]string, len(entries))
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("🕐 *Last %d entries:*\n\n", len(entries)))
		for i, e := range entries {
			ids[i] = e.ID
			content := util.Truncate(e.Content, 150)
			ts := e.Timestamp.Format("Jan 2 15:04")
			sb.WriteString(fmt.Sprintf("`[%d]` _%s_ — %s\n", i+1, ts, content))
		}
		lastResults[cmd.UserID] = ids
		sb.WriteString("\n_Use `/hippo forget <#>` to delete by number._")
		responseText = sb.String()

	case "tags":
		infos, err := store.ListTags(ctx)
		if err != nil {
			responseText = fmt.Sprintf("❌ Error: %s", err)
			break
		}
		// Filter by prefix if arg provided
		var filtered []memory.TagInfo
		for _, t := range infos {
			if arg == "" || strings.Contains(t.Name, arg) {
				filtered = append(filtered, t)
			}
		}
		if len(filtered) == 0 {
			responseText = "No tags found."
			break
		}
		// Show top 20
		max := 20
		if len(filtered) < max {
			max = len(filtered)
		}
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("🏷️ *Tags* (%d total", len(infos)))
		if arg != "" {
			sb.WriteString(fmt.Sprintf(", filtered by \"%s\"", arg))
		}
		sb.WriteString("):\n\n")
		for i := 0; i < max; i++ {
			sb.WriteString(fmt.Sprintf("• `%s` (%d)\n", filtered[i].Name, filtered[i].Count))
		}
		if len(filtered) > max {
			sb.WriteString(fmt.Sprintf("\n_...and %d more._", len(filtered)-max))
		}
		responseText = sb.String()

	case "store":
		if arg == "" {
			responseText = "Usage: `/hippo store <content to remember>`"
			break
		}
		entry := memory.Entry{
			ID:        fmt.Sprintf("slack:cmd:%d", time.Now().UnixNano()),
			Timestamp: time.Now(),
			Content:   arg,
			Tags: []string{
				"source:slack",
				fmt.Sprintf("slack:user:%s", cmd.UserID),
				fmt.Sprintf("date:%s", time.Now().Format("2006-01-02")),
			},
		}
		if err := store.Put(ctx, entry); err != nil {
			responseText = fmt.Sprintf("❌ Store error: %s", err)
			break
		}
		responseText = fmt.Sprintf("✅ Stored: \"%s\"", util.Truncate(arg, 100))

	case "forget":
		if arg == "" {
			responseText = "Usage: `/hippo forget <#>` (number from last search/recent)"
			break
		}
		// Check for confirmation: "forget 2 confirm"
		argParts := strings.Fields(arg)
		var idx int
		fmt.Sscanf(argParts[0], "%d", &idx)
		ids := lastResults[cmd.UserID]
		if idx < 1 || idx > len(ids) {
			responseText = fmt.Sprintf("❌ Invalid reference `[%d]`. Run `/hippo search` or `/hippo recent` first.", idx)
			break
		}
		entryID := ids[idx-1]

		if len(argParts) >= 2 && argParts[1] == "confirm" {
			// Actually delete
			if err := store.Delete(ctx, entryID); err != nil {
				responseText = fmt.Sprintf("❌ Delete error: %s", err)
				break
			}
			responseText = fmt.Sprintf("🗑️ Deleted entry `[%d]`", idx)
		} else {
			// Show what will be deleted and ask for confirmation
			entry, err := store.Get(ctx, entryID)
			if err != nil {
				responseText = fmt.Sprintf("❌ Entry not found: %s", err)
				break
			}
			content := util.Truncate(entry.Content, 300)
			responseText = fmt.Sprintf("⚠️ *About to delete:*\n\n`[%d]` %s\n_Tags: %s_\n\n→ Run `/hippo forget %d confirm` to delete permanently.", idx, content, strings.Join(entry.Tags, ", "), idx)
		}

	case "link":
		// /hippo link <#a> <#b> <score>
		linkParts := strings.Fields(arg)
		if len(linkParts) < 3 {
			responseText = "Usage: `/hippo link <#a> <#b> <score>` (numbers from last search)"
			break
		}
		var idxA, idxB int
		var score float64
		fmt.Sscanf(linkParts[0], "%d", &idxA)
		fmt.Sscanf(linkParts[1], "%d", &idxB)
		fmt.Sscanf(linkParts[2], "%f", &score)
		ids := lastResults[cmd.UserID]
		if idxA < 1 || idxA > len(ids) || idxB < 1 || idxB > len(ids) {
			responseText = "❌ Invalid references. Run `/hippo search` first."
			break
		}
		if score < -1.0 || score > 1.0 {
			responseText = "❌ Score must be between -1.0 and +1.0"
			break
		}
		if err := store.Link(ctx, ids[idxA-1], ids[idxB-1], score, ""); err != nil {
			responseText = fmt.Sprintf("❌ Link error: %s", err)
			break
		}
		responseText = fmt.Sprintf("🔗 Linked `[%d]` ↔ `[%d]` (score: %.2f)", idxA, idxB, score)

	case "ingest":
		if arg == "" {
			responseText = "Usage: `/hippo ingest <url>`"
			break
		}
		url := strings.Fields(arg)[0] // take first token as URL
		opts := ingest.DefaultOptions()
		opts.Tags = []string{"source:slack", fmt.Sprintf("slack:user:%s", cmd.UserID)}
		opts.RejectThreshold = cfg.Ingest.RejectThreshold
		opts.SanitizeThreshold = cfg.Ingest.SanitizeThreshold
		opts.WebContentWeight = cfg.Ingest.WebContentWeight
		opts.StubWeight = cfg.Ingest.StubWeight

		result, err := ingest.Pipeline(ctx, store, url, opts)
		if err != nil {
			responseText = fmt.Sprintf("❌ Ingest failed: %s", err)
			break
		}
		responseText = fmt.Sprintf("✅ Ingested: *%s*\n%d words, %d chunks\nStub ID: `%s`",
			result.Title, result.WordCount, result.ChunkCount, result.StubID)

	case "status":
		infos, _ := store.ListTags(ctx)
		totalTags := len(infos)
		responseText = fmt.Sprintf("🧠 Hippocampus is online.\n• Tags: %d\n• Listening on %d channel(s)",
			totalTags, len(cfg.Slack.Channels))

	default:
		responseText = "*Available commands:*\n" +
			"• `/hippo search <query>` — Search memory\n" +
			"• `/hippo recent [N]` — Last N entries (default 5)\n" +
			"• `/hippo tags [filter]` — List tags (optional filter)\n" +
			"• `/hippo store <text>` — Store a memory\n" +
			"• `/hippo forget <#>` — Delete entry by number from last results\n" +
			"• `/hippo link <#a> <#b> <score>` — Link two entries (-1.0 to +1.0)\n" +
			"• `/hippo ingest <url>` — Ingest a web page\n" +
			"• `/hippo status` — Bot status"
	}

	// Respond to slash command
	client.Ack(*evt.Request, map[string]interface{}{
		"response_type": "ephemeral",
		"text":          responseText,
	})
}



// userNameCache lazily resolves Slack user IDs to display names.
type userNameCache struct {
	api   *slack.Client
	cache map[string]string
	mu    sync.RWMutex
}

// parseSlackTimestamp converts a Slack message timestamp (e.g. "1749792296.077709")
// to a time.Time. Falls back to time.Now() on parse failure.
func parseSlackTimestamp(ts string) time.Time {
	if ts == "" {
		return time.Now()
	}
	// Slack ts format: "seconds.microseconds" (dot-separated)
	parts := strings.SplitN(ts, ".", 2)
	var sec, usec int64
	fmt.Sscanf(parts[0], "%d", &sec)
	if len(parts) > 1 {
		fmt.Sscanf(parts[1], "%d", &usec)
	}
	if sec == 0 {
		return time.Now()
	}
	return time.Unix(sec, usec*1000) // microseconds → nanoseconds
}

// Resolve returns the display name for a Slack user ID.
// Falls back to the raw ID if the API call fails.
func (c *userNameCache) Resolve(userID string) string {
	if userID == "" {
		return ""
	}

	c.mu.RLock()
	if name, ok := c.cache[userID]; ok {
		c.mu.RUnlock()
		return name
	}
	c.mu.RUnlock()

	// Cache miss — call API
	user, err := c.api.GetUserInfo(userID)
	if err != nil {
		slog.Debug("failed to resolve user name", "user_id", userID, "error", err)
		return userID // fall back to raw ID
	}

	name := user.RealName
	if name == "" {
		name = user.Name // username fallback
	}

	c.mu.Lock()
	c.cache[userID] = name
	c.mu.Unlock()

	return name
}

// runBackfill incrementally pages through channel history, storing messages
// and tracking progress via a Redis cursor key. Resumes from where it left off.
func runBackfill(ctx context.Context, rdb *redis.Client, store memory.Store, api *slack.Client, ch config.SlackChannel, users *userNameCache) {
	cursorKey := fmt.Sprintf("meta:slack:backfill:%s", ch.ID)

	// Read our progress cursor (oldest timestamp we've reached)
	latestProcessed, _ := rdb.Get(ctx, cursorKey).Result()

	// "done" sentinel means backfill previously completed — do an opportunistic tag-patch pass
	if latestProcessed == "done" {
		slog.Info("backfill complete, running tag-patch pass", "channel", ch.Name)
		patchExistingTags(ctx, rdb, store, api, ch, users)
		return
	}

	slog.Info("backfill starting",
		"channel", ch.Name,
		"cursor", latestProcessed,
	)

	for {
		select {
		case <-ctx.Done():
			slog.Info("backfill cancelled", "channel", ch.Name)
			return
		default:
		}

		params := &slack.GetConversationHistoryParameters{
			ChannelID: ch.ID,
			Limit:     100,
		}
		// If we have a cursor, get messages OLDER than our oldest
		if latestProcessed != "" {
			params.Latest = latestProcessed
		}

		history, err := api.GetConversationHistory(params)
		if err != nil {
			slog.Warn("backfill API error, will retry",
				"channel", ch.Name,
				"error", err,
			)
			time.Sleep(60 * time.Second)
			continue
		}

		if len(history.Messages) == 0 {
			slog.Info("backfill complete (reached beginning)", "channel", ch.Name)
			rdb.Set(ctx, cursorKey, "done", 0)
			return
		}

		count := 0
		patched := 0
		var oldestTs string
		for _, msg := range history.Messages {
			// Track oldest for cursor
			oldestTs = msg.Timestamp

			if msg.Text == "" {
				continue
			}

			// Resolve user name
			userName := users.Resolve(msg.User)

			// Build tags
			tags := []string{
				"content:slack",
				"source:slack",
				fmt.Sprintf("slack:channel:%s", ch.ID),
				fmt.Sprintf("slack:channel_name:%s", ch.Name),
				fmt.Sprintf("slack:user:%s", msg.User),
				fmt.Sprintf("slack:user_name:%s", userName),
			}
			if len(ch.Tags) > 0 {
				tags = append(tags, ch.Tags...)
			}
			if msg.ThreadTimestamp != "" && msg.ThreadTimestamp != msg.Timestamp {
				tags = append(tags, fmt.Sprintf("slack:thread:%s", msg.ThreadTimestamp))
			}

			id := fmt.Sprintf("slack:%s:%s", ch.ID, msg.Timestamp)

			// Check if entry already exists — if so, opportunistically patch tags
			existing, err := store.Get(ctx, id)
			if err == nil && existing.ID != "" {
				// Entry exists — check if it's missing the human-readable tags
				needsPatch := true
				for _, t := range existing.Tags {
					if strings.HasPrefix(t, "slack:user_name:") {
						needsPatch = false
						break
					}
				}
				if needsPatch {
					// Add the human-readable tags to existing entry
					patchTags := []string{
						fmt.Sprintf("slack:channel_name:%s", ch.Name),
						fmt.Sprintf("slack:user_name:%s", userName),
					}
					for _, pt := range patchTags {
						existing.Tags = append(existing.Tags, pt)
					}
					existing.Tags = dedup(existing.Tags)
					store.Put(ctx, existing)
					patched++
				}
				continue // don't re-store content
			}

			entry := memory.Entry{
				ID:        id,
				Timestamp: parseSlackTimestamp(msg.Timestamp),
				Content:   msg.Text,
				Tags:      tags,
			}

			if err := store.Put(ctx, entry); err != nil {
				continue
			}

			// Thread links
			if msg.ThreadTimestamp != "" && msg.ThreadTimestamp != msg.Timestamp {
				parentID := fmt.Sprintf("slack:%s:%s", ch.ID, msg.ThreadTimestamp)
				store.Link(ctx, parentID, id, 0.7, "extends")
			}

			count++
		}

		// Save progress
		if oldestTs != "" {
			rdb.Set(ctx, cursorKey, oldestTs, 0)
			latestProcessed = oldestTs
		}

		slog.Info("backfill page ingested",
			"channel", ch.Name,
			"new", count,
			"patched", patched,
			"cursor", oldestTs,
		)

		if !history.HasMore {
			slog.Info("backfill complete", "channel", ch.Name)
			rdb.Set(ctx, cursorKey, "done", 0)
			return
		}

		// Rate limit: ~1 page per 2 seconds (Tier 3)
		time.Sleep(2 * time.Second)
	}
}

// patchExistingTags scans entries for a channel and rewrites human-readable tags
// (user_name, channel_name) on all entries, re-resolving names from the API.
func patchExistingTags(ctx context.Context, rdb *redis.Client, store memory.Store, api *slack.Client, ch config.SlackChannel, users *userNameCache) {
	// Find all entries for this channel via the tag set
	tagKey := fmt.Sprintf("tag:slack:channel:%s", ch.ID)
	ids, err := rdb.SMembers(ctx, tagKey).Result()
	if err != nil || len(ids) == 0 {
		slog.Info("tag-patch: no entries found for channel", "channel", ch.Name)
		return
	}

	patched := 0
	scanned := 0
	for _, id := range ids {
		select {
		case <-ctx.Done():
			slog.Info("tag-patch cancelled", "channel", ch.Name, "patched", patched)
			return
		default:
		}

		entry, err := store.Get(ctx, id)
		if err != nil || entry.ID == "" {
			continue
		}
		scanned++

		if scanned <= 3 {
			slog.Debug("tag-patch sample entry",
				"id", entry.ID,
				"tags_before", strings.Join(entry.Tags, ", "),
			)
		}

		// Find the user ID and strip any existing user_name/channel_name tags
		var userID string
		filtered := make([]string, 0, len(entry.Tags))
		for _, t := range entry.Tags {
			if strings.HasPrefix(t, "slack:user_name:") {
				continue // strip — will re-add
			}
			if strings.HasPrefix(t, "slack:channel_name:") {
				continue // strip — will re-add
			}
			if strings.HasPrefix(t, "slack:user:") {
				userID = strings.TrimPrefix(t, "slack:user:")
			}
			filtered = append(filtered, t)
		}

		// Re-add with current resolved values
		filtered = append(filtered, fmt.Sprintf("slack:channel_name:%s", ch.Name))
		if userID != "" {
			userName := users.Resolve(userID)
			filtered = append(filtered, fmt.Sprintf("slack:user_name:%s", userName))
		}

		entry.Tags = dedup(filtered)

		// Fix timestamp: extract from entry ID (slack:<channel>:<ts>) if current ts looks wrong
		// "Wrong" = within a few seconds of another entry (batch-ingested) or clearly not the message time
		parts := strings.SplitN(entry.ID, ":", 3)
		if len(parts) == 3 {
			slackTs := parts[2]
			correctTime := parseSlackTimestamp(slackTs)
			if !correctTime.IsZero() && correctTime.Year() > 2000 {
				entry.Timestamp = correctTime
			}
		}

		if err := store.Put(ctx, entry); err != nil {
			continue
		}
		patched++
	}

	slog.Info("tag-patch complete",
		"channel", ch.Name,
		"scanned", scanned,
		"patched", patched,
	)
}

// dedup removes duplicate strings from a slice, preserving order.
func dedup(ss []string) []string {
	seen := make(map[string]bool, len(ss))
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
