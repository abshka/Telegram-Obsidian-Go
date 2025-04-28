package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/tg"

	"telegram-obsidian/cache"
	"telegram-obsidian/config"
	"telegram-obsidian/media"
	"telegram-obsidian/notes"
	telegramClient "telegram-obsidian/telegram"
	"telegram-obsidian/utils"
)

func main() {
	// Catch interrupts for graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Start the exporter in a separate goroutine
	errChan := make(chan error, 1)
	go func() {
		errChan <- runExporter()
	}()

	// Wait for completion or interrupt
	select {
	case err := <-errChan:
		if err != nil {
			log.Fatalf("Exporter error: %v", err)
		}
	case sig := <-sigChan:
		log.Printf("Received signal %v, shutting down...", sig)
		// Cleanup could be done here
	}

	fmt.Println("Exiting.")
}

// runExporter is the main function that runs the export process
func runExporter() error {
	// Load configuration
	cfg, err := config.LoadConfig()
	if err != nil {
		return fmt.Errorf("failed to load configuration: %v", err)
	}

	// Set up logging
	utils.SetupLogging(cfg.Verbose)
	defer utils.CloseLogger()

	utils.LogInfo("Starting Telegram to Obsidian exporter")

	// Create cache manager
	cacheManager := cache.NewCacheManager(cfg.CacheFile)
	err = cacheManager.LoadCache()
	if err != nil {
		return fmt.Errorf("failed to load cache: %v", err)
	}
	defer cacheManager.SaveCache()

	// Create context with cancel for cleanup
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Authenticate with Telegram and run the export
	return authenticateAndExport(ctx, cfg, cacheManager)
}

// authenticateAndExport handles authentication and runs the export process
func authenticateAndExport(ctx context.Context, cfg *config.Config, cacheManager *cache.CacheManager) error {
	// Set up session storage
	sessionPath := fmt.Sprintf("%s/%s.json", cfg.SessionDir, cfg.SessionName)
	storage := &session.FileStorage{Path: sessionPath}

	// Create Telegram client
	client := telegram.NewClient(cfg.APIID, cfg.APIHash, telegram.Options{
		SessionStorage: storage,
	})

	// Run the client
	return client.Run(ctx, func(ctx context.Context) error {
		// Check authentication status
		status, err := client.Auth().Status(ctx)
		if err != nil {
			return fmt.Errorf("failed to get auth status: %w", err)
		}

		// Authenticate if needed
		if !status.Authorized {
			if err := authenticate(ctx, client, cfg.PhoneNumber); err != nil {
				return fmt.Errorf("authentication failed: %w", err)
			}
		} else {
			utils.LogInfo("Already authenticated")

			// Print user info
			if err := printSelf(ctx, client); err != nil {
				utils.LogWarn("Failed to get user info: %v", err)
			}
		}

		// Create telegram manager and other components
		telegramMgr := telegramClient.NewTelegramManager(cfg)

		// Connect to Telegram API
		if err := telegramMgr.Connect(ctx); err != nil {
			return fmt.Errorf("failed to connect to Telegram API: %v", err)
		}
		defer telegramMgr.Disconnect()

		// Create media processor and note generator
		mediaProcessor := media.NewMediaProcessor(cfg, telegramMgr)
		noteGenerator := notes.NewNoteGenerator(cfg)

		// Use the cache manager directly as it already implements the required interface
		replyLinker := notes.NewReplyLinker(cfg, cacheManager)

		// Run interactive selection if needed
		if cfg.InteractiveMode && len(cfg.ExportTargets) == 0 {
			if err := telegramMgr.RunInteractiveSelection(ctx); err != nil {
				// Check if user chose to exit
				if err.Error() == "user chose to exit without exporting" {
					utils.LogInfo("User exited interactive mode without selecting export targets")
					return nil
				}
				return fmt.Errorf("interactive selection failed: %v", err)
			}
		}

		// Check if we have any targets
		if len(cfg.ExportTargets) == 0 {
			return fmt.Errorf("no export targets specified")
		}

		// Process each export target
		for _, target := range cfg.ExportTargets {
			utils.LogInfo("Processing export target: %s", target.ID)

			// Skip targets with empty IDs
			if target.ID == "" {
				utils.LogWarn("Skipping target with empty ID")
				continue
			}

			if err := exportSingleTarget(ctx, target, cfg, telegramMgr, cacheManager, mediaProcessor, noteGenerator, replyLinker); err != nil {
				utils.LogWarn("Failed to export target %s: %v", target.ID, err)
				// Continue with other targets
				continue
			}
		}

		return nil
	})
}

// authenticate handles the authentication process
func authenticate(ctx context.Context, client *telegram.Client, phone string) error {
	utils.LogInfo("Starting authentication for %s", phone)
	reader := bufio.NewReader(os.Stdin)

	// 1) Send code request
	sent, err := client.Auth().SendCode(ctx, phone, auth.SendCodeOptions{})
	if err != nil {
		return fmt.Errorf("failed to send code: %w", err)
	}

	// Extract code hash
	var codeHash string
	switch v := sent.(type) {
	case *tg.AuthSentCode:
		codeHash = v.PhoneCodeHash
	case *tg.AuthSentCodeSuccess:
		utils.LogInfo("Authentication successful after sending code (unexpected for phone login)")
		return nil
	default:
		return fmt.Errorf("unexpected SendCode response type %T", v)
	}

	// 2) Ask user for confirmation code
	fmt.Print("Enter confirmation code: ")
	code, _ := reader.ReadString('\n')
	code = strings.TrimSpace(code)
	code = strings.ReplaceAll(code, " ", "")

	// 3) Sign in
	_, err = client.Auth().SignIn(ctx, phone, code, codeHash)
	if err != nil {
		// Check if 2FA is required
		if errors.Is(err, auth.ErrPasswordAuthNeeded) {
			// 2FA required
			fmt.Print("Enter 2FA password: ")
			pass, _ := reader.ReadString('\n')
			pass = strings.TrimSpace(pass)
			if _, err = client.Auth().Password(ctx, pass); err != nil {
				return fmt.Errorf("2FA authentication failed: %w", err)
			}
			// Authentication successful after Password() if no errors occurred
		} else {
			// Other login errors
			return fmt.Errorf("login failed: %w", err)
		}
	}

	utils.LogInfo("Authentication successful")
	return nil
}

// printSelf gets and prints user information
func printSelf(ctx context.Context, client *telegram.Client) error {
	// Get user information
	me, err := client.Self(ctx)
	if err != nil {
		return fmt.Errorf("failed to get user info: %w", err)
	}

	fmt.Println("--- User Information ---")
	fmt.Printf("ID: %d\n", me.ID)
	fmt.Printf("Name: %s %s\n", me.FirstName, me.LastName)
	if me.Username != "" {
		fmt.Printf("Username: @%s\n", me.Username)
	}
	if me.Phone != "" {
		fmt.Printf("Phone: %s\n", me.Phone)
	}
	fmt.Println("-----------------------")

	return nil
}

// exportSingleTarget exports a single target entity
func exportSingleTarget(
	ctx context.Context,
	target config.ExportTarget,
	cfg *config.Config,
	telegramMgr *telegramClient.TelegramManager,
	cacheManager *cache.CacheManager,
	mediaProcessor *media.MediaProcessor,
	noteGenerator *notes.NoteGenerator,
	replyLinker *notes.ReplyLinker,
) error {
	entityIDStr := target.ID
	utils.LogInfo("Exporting entity: %s", entityIDStr)

	// Resolve entity
	peer, err := telegramMgr.ResolveEntity(ctx, entityIDStr)
	if err != nil {
		return fmt.Errorf("failed to resolve entity %s: %v", entityIDStr, err)
	}

	// Get entity info
	title, entityType, err := telegramMgr.GetEntityInfo(ctx, *peer)
	if err != nil {
		return fmt.Errorf("failed to get entity info for %s: %v", entityIDStr, err)
	}

	// Update entity info in cache
	cacheManager.UpdateEntityInfo(entityIDStr, title, entityType)
	utils.LogInfo("Entity info: %s (%s)", title, entityType)

	// Get export and media paths
	exportPath := cfg.GetExportPathForEntity(entityIDStr)
	mediaPath := cfg.GetMediaPathForEntity(entityIDStr)

	utils.LogInfo("Export path: %s", exportPath)
	utils.LogInfo("Media path: %s", mediaPath)

	// Get last processed message ID if only new messages are requested
	var lastProcessedID int64 = 0
	if cfg.OnlyNew {
		lastProcessedID = cacheManager.GetLastProcessedMessageID(entityIDStr)
		utils.LogInfo("Processing only new messages, last ID: %d", lastProcessedID)
	} else {
		utils.LogInfo("Processing all messages (ONLY_NEW=false)")
	}

	// Process entity messages
	err = processEntityMessages(ctx, *peer, entityIDStr, exportPath, mediaPath, lastProcessedID, cfg.MessageBatchSize, telegramMgr, cacheManager, mediaProcessor, noteGenerator)
	if err != nil {
		return fmt.Errorf("failed to process messages for entity %s: %v", entityIDStr, err)
	}

	// Link replies
	utils.LogInfo("Linking replies for entity %s", entityIDStr)
	err = replyLinker.LinkReplies(entityIDStr, exportPath)
	if err != nil {
		utils.LogWarn("Failed to link replies for entity %s: %v", entityIDStr, err)
	}

	// Save cache
	utils.LogInfo("Saving cache for entity %s", entityIDStr)
	err = cacheManager.SaveCache()
	if err != nil {
		utils.LogWarn("Failed to save cache: %v", err)
	}

	utils.LogInfo("Finished exporting entity: %s (%s)", entityIDStr, title)
	return nil
}

// processEntityMessages processes messages for a single entity
func processEntityMessages(
	ctx context.Context,
	peer tg.InputPeerClass,
	entityIDStr string,
	exportPath string,
	mediaPath string,
	lastProcessedID int64,
	batchSize int,
	telegramMgr *telegramClient.TelegramManager,
	cacheManager *cache.CacheManager,
	mediaProcessor *media.MediaProcessor,
	noteGenerator *notes.NoteGenerator,
) error {
	utils.LogInfo("Processing messages for entity %s, starting from ID %d", entityIDStr, lastProcessedID)

	// Process messages in batches
	var processed int = 0
	var skipped int = 0
	var wg sync.WaitGroup
	semaphore := make(chan struct{}, 10) // Limit concurrent message processing
	groupedMessages := make(map[int64][]*telegramClient.Message)
	processedGroupIDs := make(map[int64]bool)

	const GROUP_TIMEOUT = 10 // seconds
	var lastMessageTime time.Time = time.Now()

	for {
		// Fetch a batch of messages
		messages, err := telegramMgr.FetchMessages(ctx, peer, lastProcessedID, batchSize)
		if err != nil {
			return fmt.Errorf("failed to fetch messages: %v", err)
		}

		if len(messages) == 0 {
			utils.LogInfo("No more messages to process")
			break
		}

		utils.LogInfo("Fetched batch of %d messages", len(messages))

		// Process each message
		for _, msg := range messages {
			// Skip already processed messages
			if cacheManager.IsProcessed(msg.ID, entityIDStr) {
				skipped++
				continue
			}

			// Process messages with grouped_id (media albums)
			if msg.GroupedID != 0 {
				groupID := msg.GroupedID

				// Skip if we've already processed this group
				if processedGroupIDs[groupID] {
					skipped++
					continue
				}

				// Add to group
				groupedMessages[groupID] = append(groupedMessages[groupID], msg)

				// If this is the first message in the group, record the time
				if len(groupedMessages[groupID]) == 1 {
					lastMessageTime = time.Now()
				}
				continue
			}

			// Process single message
			wg.Add(1)
			semaphore <- struct{}{} // Acquire semaphore

			go func(message *telegramClient.Message) {
				defer wg.Done()
				defer func() { <-semaphore }() // Release semaphore

				processMessageGroup(ctx, []*telegramClient.Message{message}, entityIDStr, exportPath, mediaPath, telegramMgr, cacheManager, mediaProcessor, noteGenerator)
			}(msg)

			processed++
		}

		// Process accumulated groups that haven't received new messages for GROUP_TIMEOUT
		for groupID, groupMessages := range groupedMessages {
			// Skip if we've already processed this group
			if processedGroupIDs[groupID] {
				continue
			}

			// If enough time has passed or we have a large enough group
			if time.Since(lastMessageTime) > GROUP_TIMEOUT*time.Second || len(groupMessages) >= 10 {
				wg.Add(1)
				semaphore <- struct{}{} // Acquire semaphore

				go func(messages []*telegramClient.Message) {
					defer wg.Done()
					defer func() { <-semaphore }() // Release semaphore

					processMessageGroup(ctx, messages, entityIDStr, exportPath, mediaPath, telegramMgr, cacheManager, mediaProcessor, noteGenerator)
				}(groupMessages)

				processedGroupIDs[groupID] = true
				processed++

				// Remove from map to save memory
				delete(groupedMessages, groupID)
			}
		}

		// Schedule cache save periodically
		if (processed+skipped)%100 == 0 {
			cacheManager.ScheduleBackgroundSave()
			utils.LogInfo("Progress: processed %d messages, skipped %d messages", processed, skipped)
		}

		// If we got fewer messages than requested, we've reached the end
		if len(messages) < batchSize {
			break
		}

		// Update lastProcessedID for the next batch
		lastProcessedID = messages[len(messages)-1].ID
	}

	// Process any remaining groups
	for groupID, groupMessages := range groupedMessages {
		if !processedGroupIDs[groupID] {
			wg.Add(1)
			semaphore <- struct{}{} // Acquire semaphore

			go func(messages []*telegramClient.Message) {
				defer wg.Done()
				defer func() { <-semaphore }() // Release semaphore

				processMessageGroup(ctx, messages, entityIDStr, exportPath, mediaPath, telegramMgr, cacheManager, mediaProcessor, noteGenerator)
			}(groupMessages)

			processed++
		}
	}

	// Wait for all message processing to complete
	wg.Wait()
	utils.LogInfo("Completed: processed %d messages/groups, skipped %d messages for entity %s", processed, skipped, entityIDStr)

	return nil
}

// processMessageGroup processes a group of messages (single message or album)
func processMessageGroup(
	ctx context.Context,
	messages []*telegramClient.Message,
	entityID string,
	exportPath string,
	mediaPath string,
	_ *telegramClient.TelegramManager,
	cacheManager *cache.CacheManager,
	mediaProcessor *media.MediaProcessor,
	noteGenerator *notes.NoteGenerator,
) {
	if len(messages) == 0 {
		return
	}

	// Sort messages by ID to ensure deterministic order (important for albums)
	sort.Slice(messages, func(i, j int) bool {
		return messages[i].ID < messages[j].ID
	})

	// Use the first message for text and metadata
	firstMessage := messages[0]

	// Skip if already processed (double-check)
	if cacheManager.IsProcessed(firstMessage.ID, entityID) {
		utils.LogDebug("Message %d already processed, skipping", firstMessage.ID)
		return
	}

	groupType := "single message"
	if len(messages) > 1 {
		groupType = fmt.Sprintf("message group with %d items", len(messages))
	}
	utils.LogDebug("Processing %s, ID: %d", groupType, firstMessage.ID)

	// Download and optimize media for all messages in the group
	var allMediaPaths []string
	var mediaErrors []error

	for _, msg := range messages {
		mediaPaths, err := mediaProcessor.DownloadAndOptimizeMedia(ctx, msg, mediaPath)
		if err != nil {
			utils.LogWarn("Error processing media for message %d: %v", msg.ID, err)
			mediaErrors = append(mediaErrors, err)
		}
		if len(mediaPaths) > 0 {
			allMediaPaths = append(allMediaPaths, mediaPaths...)
		}
	}

	// If all media processing failed and there's no text, log warning and move on
	if len(allMediaPaths) == 0 && len(mediaErrors) > 0 && firstMessage.Text == "" {
		utils.LogWarn("All media processing failed for message %d and message has no text, skipping", firstMessage.ID)
		return
	}

	// Create note
	notePath, err := noteGenerator.CreateNote(firstMessage, allMediaPaths, exportPath)
	if err != nil {
		utils.LogWarn("Error creating note for message %d: %v", firstMessage.ID, err)
		return
	}

	// Extract filename from path
	noteFilename := filepath.Base(notePath)

	// Mark all messages in the group as processed
	for _, msg := range messages {
		cacheManager.AddProcessedMessage(msg.ID, noteFilename, firstMessage.ReplyToID, entityID)
	}

	mediaDesc := ""
	if len(allMediaPaths) > 0 {
		mediaDesc = fmt.Sprintf(" with %d media files", len(allMediaPaths))
	}
	utils.LogInfo("Created note %s for message %d%s", noteFilename, firstMessage.ID, mediaDesc)
}
