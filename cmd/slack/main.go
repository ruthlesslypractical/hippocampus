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
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
	"github.com/ruthlesslypractical/hippocampus/internal/config"
	"github.com/ruthlesslypractical/hippocampus/internal/memory"
	"github.com/ruthlesslypractical/hippocampus/internal/util"
	"github.com/ruthlesslypractical/hippocampus/pkg/ingest"
	"github.com/ruthlesslypractical/hippocampus/pkg/safeguard"
)

func main() {
	configPath := flag.String("config", "", "path to config.json")
	verbose := flag.Bool("v", false, "verbose logging")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("hippocampus-slack v%s\n", config.Version)
		os.Exit(0)
	}

	// Find config
	path := *configPath
	if path == "" {
		home, _ := os.UserHomeDir()
		candidates := []string{
			home + "/Library/Application Support/Hippocampus/config.json",
			home + "/.config/hippocampus/config.json",
		}
		for _, c := range candidates {
			if _, err := os.Stat(c); err == nil {
				path = c
				break
			}
		}
	}
	if path == "" {
		log.Fatal("no config file found")
	}

	cfg, err := config.Load(path)
	if err != nil {
		log.Fatalf("loading config: %s", err)
	}

	if cfg.Slack.BotToken == "" || cfg.Slack.AppToken == "" {
		log.Fatal("slack.bot_token and slack.app_token are required in config")
	}

	// Connect to Redis
	rdb := cfg.Redis.NewRedisClient()
	defer rdb.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Fatalf("connecting to redis: %s", err)
	}

	store, err := memory.NewRedisStore(cfg.Redis, nil)
	if err != nil {
		log.Fatalf("creating store: %s", err)
	}

	// Build channel lookup set for filtering
	monitoredChannels := make(map[string]config.SlackChannel)
	for _, ch := range cfg.Slack.Channels {
		monitoredChannels[ch.ID] = ch
	}

	if len(monitoredChannels) == 0 {
		log.Fatal("no channels configured in slack.channels")
	}

	// Create Slack client
	api := slack.New(
		cfg.Slack.BotToken,
		slack.OptionAppLevelToken(cfg.Slack.AppToken),
	)

	client := socketmode.New(api,
		socketmode.OptionLog(log.New(os.Stderr, "[slack] ", log.LstdFlags)),
	)

	if *verbose {
		log.Printf("monitoring %d channels", len(monitoredChannels))
		for _, ch := range cfg.Slack.Channels {
			log.Printf("  %s (%s) [%s]", ch.Name, ch.ID, ch.Mode)
		}
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
				handleEvent(ctx, store, eventsAPIEvent, monitoredChannels, *verbose)

			case socketmode.EventTypeSlashCommand:
				cmd, ok := evt.Data.(slack.SlashCommand)
				if !ok {
					continue
				}
				handleSlashCommand(ctx, store, cfg, client, evt, cmd)

			case socketmode.EventTypeConnecting:
				if *verbose {
					log.Println("connecting to Slack...")
				}
			case socketmode.EventTypeConnected:
				log.Println("connected to Slack (Socket Mode)")
			case socketmode.EventTypeConnectionError:
				log.Println("connection error, will retry...")
			}
		}
	}()

	// Graceful shutdown
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		log.Println("shutting down...")
		cancel()
		os.Exit(0)
	}()

	// Start backfill goroutines for channels that have it enabled
	for _, ch := range cfg.Slack.Channels {
		if ch.Backfill {
			go runBackfill(ctx, rdb, store, slack.New(cfg.Slack.BotToken), ch, *verbose)
		}
	}

	if err := client.Run(); err != nil {
		log.Fatalf("socket mode error: %s", err)
	}
}

func handleEvent(ctx context.Context, store memory.Store, event slackevents.EventsAPIEvent, channels map[string]config.SlackChannel, verbose bool) {
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
			}

			if text == "" {
				return
			}
			if text == "" {
				return
			}

			if verbose {
				log.Printf("[%s] %s: %s", ch.Name, ev.User, util.Truncate(text, 80))
			}

			// Run safeguard scan
			result := safeguard.Scan(text)

			// Build entry
			tags := []string{
				"content:slack",
				"source:slack",
				fmt.Sprintf("slack:channel:%s", ev.Channel),
				fmt.Sprintf("slack:user:%s", ev.User),
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
				if verbose {
					log.Printf("  ⚠️ risk_score=%.2f, flags=%d", result.RiskScore, len(result.Flags))
				}
			}

			id := fmt.Sprintf("slack:%s:%s", ev.Channel, msgTs)

			entry := memory.Entry{
				ID:        id,
				Timestamp: time.Now(),
				Content:   text,
				Tags:      tags,
			}

			if err := store.Put(ctx, entry); err != nil {
				log.Printf("error storing message: %s", err)
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



// runBackfill incrementally pages through channel history, storing messages
// and tracking progress via a Redis cursor key. Resumes from where it left off.
func runBackfill(ctx context.Context, rdb *redis.Client, store memory.Store, api *slack.Client, ch config.SlackChannel, verbose bool) {
	cursorKey := fmt.Sprintf("meta:slack:backfill:%s", ch.ID)

	// Read our progress cursor (oldest timestamp we've reached)
	latestProcessed, _ := rdb.Get(ctx, cursorKey).Result()

	if verbose {
		log.Printf("[backfill] %s: starting (cursor: %s)", ch.Name, latestProcessed)
	}

	for {
		select {
		case <-ctx.Done():
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
			if verbose {
				log.Printf("[backfill] %s: error: %s (will retry in 60s)", ch.Name, err)
			}
			time.Sleep(60 * time.Second)
			continue
		}

		if len(history.Messages) == 0 {
			if verbose {
				log.Printf("[backfill] %s: complete (reached beginning)", ch.Name)
			}
			// Mark as done — store a sentinel
			rdb.Set(ctx, cursorKey, "done", 0)
			return
		}

		count := 0
		var oldestTs string
		for _, msg := range history.Messages {
			// Track oldest for cursor
			oldestTs = msg.Timestamp

			if msg.Text == "" {
				continue
			}

			// Build tags
			tags := []string{
				"content:slack",
				"source:slack",
				fmt.Sprintf("slack:channel:%s", ch.ID),
				fmt.Sprintf("slack:user:%s", msg.User),
			}
			if len(ch.Tags) > 0 {
				tags = append(tags, ch.Tags...)
			}
			if msg.ThreadTimestamp != "" && msg.ThreadTimestamp != msg.Timestamp {
				tags = append(tags, fmt.Sprintf("slack:thread:%s", msg.ThreadTimestamp))
			}

			id := fmt.Sprintf("slack:%s:%s", ch.ID, msg.Timestamp)

			entry := memory.Entry{
				ID:        id,
				Timestamp: time.Now(),
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

		if verbose {
			log.Printf("[backfill] %s: ingested %d messages (cursor now: %s)", ch.Name, count, oldestTs)
		}

		if !history.HasMore {
			if verbose {
				log.Printf("[backfill] %s: complete", ch.Name)
			}
			rdb.Set(ctx, cursorKey, "done", 0)
			return
		}

		// Rate limit: ~1 page per 2 seconds (Tier 3)
		time.Sleep(2 * time.Second)
	}
}
