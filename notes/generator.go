package notes

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"telegram-obsidian/config"
	"telegram-obsidian/telegram"
	"telegram-obsidian/utils"
)

// NoteGenerator handles creation of markdown notes
type NoteGenerator struct {
	config     *config.Config
	fileLocks  map[string]*sync.Mutex
	locksMutex sync.Mutex
}

// NewNoteGenerator creates a new note generator
func NewNoteGenerator(cfg *config.Config) *NoteGenerator {
	return &NoteGenerator{
		config:    cfg,
		fileLocks: make(map[string]*sync.Mutex),
	}
}

// CreateNote creates a markdown note from a message
func (ng *NoteGenerator) CreateNote(message *telegram.Message, mediaPaths []string, entityExportPath string) (string, error) {
	utils.LogDebug("Creating note for message %d", message.ID)
	// Prepare note path and filename
	notePath, err := ng.prepareNotePath(message, entityExportPath)
	if err != nil {
		return "", fmt.Errorf("failed to prepare note path: %v", err)
	}
	// Generate note content
	content, err := ng.generateNoteContent(message, mediaPaths, notePath)
	if err != nil {
		return "", fmt.Errorf("failed to generate note content: %v", err)
	}
	// Write to file
	err = ng.writeNoteFile(notePath, content)
	if err != nil {
		return "", fmt.Errorf("failed to write note file: %v", err)
	}
	utils.LogInfo("Created note: %s", notePath)
	return notePath, nil
}

// prepareNotePath prepares the path for the note file
func (ng *NoteGenerator) prepareNotePath(message *telegram.Message, entityExportPath string) (string, error) {
	// Format date as YYYY-MM-DD
	dateStr := message.Date.Format("2006-01-02")
	// Extract title from first line of message text
	title := "Media-only" // Default for media-only messages
	if message.Text != "" {
		// Split by lines and get first non-empty line
		lines := strings.Split(message.Text, "\n")
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line != "" {
				title = line
				break
			}
		}
	}

	// Sanitize title for filename
	safeTitle := utils.SanitizeFilename(title, 100, "_")
	// Create filename with date and title
	filename := fmt.Sprintf("%s_%s.md", dateStr, safeTitle)
	// Create year directory (YYYY)
	yearStr := message.Date.Format("2006")
	yearDir := filepath.Join(entityExportPath, yearStr)
	err := utils.EnsureDirExists(yearDir)
	if err != nil {
		return "", fmt.Errorf("failed to create year directory: %v", err)
	}
	// Full path with year directory
	return filepath.Join(yearDir, filename), nil
}

// generateNoteContent generates markdown content for the note
func (ng *NoteGenerator) generateNoteContent(message *telegram.Message, mediaPaths []string, notePath string) (string, error) {
	var sb strings.Builder
	// Add date and time as title
	sb.WriteString(fmt.Sprintf("# %s\n\n", message.Date.Format("2006-01-02 15:04:05")))
	// Add message text
	if message.Text != "" {
		// Escape markdown special characters
		text := escapeMarkdown(message.Text)
		sb.WriteString(text)
		sb.WriteString("\n\n")
	}
	// Add media links
	if len(mediaPaths) > 0 {
		for _, mediaPath := range mediaPaths {
			// Calculate relative path from note to media
			relPath := utils.GetRelativePath(mediaPath, filepath.Dir(notePath))
			if relPath != "" {
				// Add obsidian-style link
				sb.WriteString(fmt.Sprintf("![[%s]]\n", relPath))
			}
		}
	}
	return sb.String(), nil
}

// writeNoteFile writes the content to the note file
func (ng *NoteGenerator) writeNoteFile(path string, content string) error {
	// Get or create a lock for this file
	fileLock := ng.getFileLock(path)
	fileLock.Lock()
	defer fileLock.Unlock()

	// Create parent directory if it doesn't exist
	dir := filepath.Dir(path)
	err := utils.EnsureDirExists(dir)
	if err != nil {
		return fmt.Errorf("failed to create directory: %v", err)
	}
	// Write the file
	err = os.WriteFile(path, []byte(content), 0644)
	if err != nil {
		return fmt.Errorf("failed to write file: %v", err)
	}
	return nil
}

// getFileLock gets or creates a mutex for a file
func (ng *NoteGenerator) getFileLock(path string) *sync.Mutex {
	ng.locksMutex.Lock()
	defer ng.locksMutex.Unlock()
	if _, exists := ng.fileLocks[path]; !exists {
		ng.fileLocks[path] = &sync.Mutex{}
	}
	return ng.fileLocks[path]
}

// escapeMarkdown escapes markdown special characters
func escapeMarkdown(text string) string {
	// Characters to escape: * _ [ ] ( ) ~
	replacer := strings.NewReplacer(
		"*", "\\*",
		"_", "\\_",
		"[", "\\[",
		"]", "\\]",
		"(", "\\(",
		")", "\\)",
		"~", "\\~",
	)
	return replacer.Replace(text)
}

// ReplyLinker handles linking replies between notes
type ReplyLinker struct {
	config       *config.Config
	cacheManager CacheManager
	fileLocks    map[string]*sync.Mutex
	locksMutex   sync.Mutex
}

// CacheManager interface defines the methods needed from the cache manager
type CacheManager interface {
	GetAllProcessedMessages(entityID string) map[int64]MessageCache
	GetNoteFilename(messageID int64, entityID string) string
}

// MessageCache represents cache data for a message, matching the cache.MessageCache struct
type MessageCache struct {
	NoteFilename string
	ReplyToID    int64
}

// NewReplyLinker creates a new reply linker
func NewReplyLinker(cfg *config.Config, cm CacheManager) *ReplyLinker {
	return &ReplyLinker{
		config:       cfg,
		cacheManager: cm,
		fileLocks:    make(map[string]*sync.Mutex),
	}
}

// LinkReplies links replies between notes
func (rl *ReplyLinker) LinkReplies(entityID string, entityExportPath string) error {
	utils.LogInfo("Linking replies for entity %s", entityID)
	// Get all processed messages for this entity
	processedMessages := rl.cacheManager.GetAllProcessedMessages(entityID)
	if processedMessages == nil || len(processedMessages) == 0 {
		utils.LogInfo("No processed messages found for entity %s", entityID)
		return nil
	}

	// Map to store parent-child relationships
	linksToAdd := make(map[string][]string)
	unresolvedChildFiles := make(map[string]bool)
	// Identify parent-child relationships
	utils.LogDebug("Processing %d messages to find reply relationships", len(processedMessages))
	// Process all messages to build reply links
	for messageID, messageCache := range processedMessages {
		// Get reply_to_id from the message cache
		replyToID := messageCache.ReplyToID
		if replyToID == 0 {
			continue
		}

		// Get filenames
		childFilename := rl.cacheManager.GetNoteFilename(messageID, entityID)
		parentFilename := rl.cacheManager.GetNoteFilename(replyToID, entityID)
		if childFilename == "" {
			utils.LogWarn("Child message %d has no filename", messageID)
			continue
		}
		if parentFilename == "" {
			utils.LogDebug("Parent message %d not found, marking as unresolved", replyToID)
			unresolvedChildFiles[childFilename] = true
			continue
		}

		// Add to links map
		linksToAdd[parentFilename] = append(linksToAdd[parentFilename], childFilename)
	}
	// Process links in parallel
	var wg sync.WaitGroup
	semaphore := make(chan struct{}, 10) // Limit concurrent file operations
	// Process parent-child links
	for parentFilename, childFilenames := range linksToAdd {
		wg.Add(1)
		semaphore <- struct{}{}
		go func(parent string, children []string) {
			defer wg.Done()
			defer func() { <-semaphore }()
			err := rl.linkParentToChildren(parent, children, entityExportPath)
			if err != nil {
				utils.LogWarn("Failed to link parent %s to children: %v", parent, err)
			}
		}(parentFilename, childFilenames)
	}
	// Process unresolved replies
	for childFilename := range unresolvedChildFiles {
		wg.Add(1)
		semaphore <- struct{}{}

		go func(child string) {
			defer wg.Done()
			defer func() { <-semaphore }()
			err := rl.markAsUnresolved(child, entityExportPath)
			if err != nil {
				utils.LogWarn("Failed to mark %s as unresolved: %v", child, err)
			}
		}(childFilename)
	}
	wg.Wait()
	return nil
}

// linkParentToChildren adds reply links to the parent file
func (rl *ReplyLinker) linkParentToChildren(parentFilename string, childFilenames []string, basePath string) error {
	// Find parent file
	parentPath, err := rl.findNotePath(basePath, parentFilename)
	if err != nil {
		return fmt.Errorf("failed to find parent note: %v", err)
	}
	// Process each child
	for _, childFilename := range childFilenames {
		// Find child file
		childPath, err := rl.findNotePath(basePath, childFilename)
		if err != nil {
			utils.LogWarn("Failed to find child note %s: %v", childFilename, err)
			continue
		}
		// Calculate relative path from parent to child
		relPath := utils.GetRelativePath(childPath, filepath.Dir(parentPath))
		if relPath == "" {
			utils.LogWarn("Failed to calculate relative path from %s to %s", parentPath, childPath)
			continue
		}

		// Create link line
		linkLine := fmt.Sprintf("Reply to: [[%s]]\n", relPath)

		// Add to parent file
		err = rl.addLineToFile(parentPath, linkLine, "Reply to")
		if err != nil {
			utils.LogWarn("Failed to add link to parent file: %v", err)
		}
	}
	return nil
}

// markAsUnresolved marks a child as replying to an unresolved message
func (rl *ReplyLinker) markAsUnresolved(childFilename string, basePath string) error {
	// Find child file
	childPath, err := rl.findNotePath(basePath, childFilename)
	if err != nil {
		return fmt.Errorf("failed to find child note: %v", err)
	}

	// Create unresolved line
	unresolvedLine := "Replied to: [Unresolved Message]\n"
	// Add to child file
	return rl.addLineToFile(childPath, unresolvedLine, "Replied to")
}

// findNotePath finds the path to a note file
func (rl *ReplyLinker) findNotePath(basePath string, filename string) (string, error) {
	// Check if file exists directly in base path
	directPath := filepath.Join(basePath, filename)
	if _, err := os.Stat(directPath); err == nil {
		return directPath, nil
	}
	// Check in year folders
	currentYear := time.Now().Year()
	startYear := 2010 // Telegram was launched in 2013
	for year := currentYear; year >= startYear; year-- {
		yearPath := filepath.Join(basePath, fmt.Sprintf("%d", year), filename)
		if _, err := os.Stat(yearPath); err == nil {
			return yearPath, nil
		}
	}
	return "", fmt.Errorf("note file %s not found", filename)
}

// addLineToFile adds a line to the beginning of a file if it doesn't exist
func (rl *ReplyLinker) addLineToFile(filePath string, line string, checkExisting string) error {
	// Get or create a lock for this file
	fileLock := rl.getFileLock(filePath)
	fileLock.Lock()
	defer fileLock.Unlock()
	// Read file content
	content, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("failed to read file: %v", err)
	}
	// Check if line already exists
	if strings.Contains(string(content), checkExisting) {
		// Line already exists, do nothing
		return nil
	}
	// Add line to the beginning
	newContent := line + string(content)
	// Write back to file
	err = os.WriteFile(filePath, []byte(newContent), 0644)
	if err != nil {
		return fmt.Errorf("failed to write file: %v", err)
	}
	return nil
}

// getFileLock gets or creates a mutex for a file
func (rl *ReplyLinker) getFileLock(path string) *sync.Mutex {
	rl.locksMutex.Lock()
	defer rl.locksMutex.Unlock()
	if _, exists := rl.fileLocks[path]; !exists {
		rl.fileLocks[path] = &sync.Mutex{}
	}
	return rl.fileLocks[path]
}
