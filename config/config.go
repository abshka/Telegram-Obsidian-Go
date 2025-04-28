package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

// ExportTarget represents a single export target (chat/channel/user)
type ExportTarget struct {
	ID       string // ID, username, or link
	Name     string // Name (filled later)
	Type     string // Type: 'channel', 'chat', 'user', 'unknown'
	ExportPath string // Path where notes will be exported
	MediaPath  string // Path where media files will be stored
}

// Config holds all configuration parameters
type Config struct {
	// Telegram API settings
	APIID        int
	APIHash      string
	PhoneNumber  string
	SessionName  string
	SessionDir   string

	// Path settings
	ObsidianPath    string
	MediaSubdir     string
	UseEntityFolders bool

	// Export targets
	ExportTargets []ExportTarget

	// Processing settings
	OnlyNew           bool
	MediaDownload     bool
	Verbose           bool
	InteractiveMode   bool

	// Parallelism
	MaxWorkers          int
	MaxProcessWorkers   int
	ConcurrentDownloads int

	// Cache settings
	CacheSaveInterval time.Duration
	CacheFile         string

	// API limits
	RequestDelay      time.Duration
	MessageBatchSize  int

	// Media optimization
	ImageQuality    int
	VideoCRF        int
	VideoPreset     string
	HWAcceleration  string
	UseH265         bool
}

// LoadConfig loads configuration from .env file and environment variables
func LoadConfig() (*Config, error) {
	// Load .env file
	err := godotenv.Load()
	if err != nil {
		fmt.Println("Warning: Could not load .env file:", err)
	}

	config := &Config{}

	// Load Telegram API settings
	apiIDStr := os.Getenv("APP_ID")
	if apiIDStr == "" {
		return nil, errors.New("APP_ID environment variable is not set or empty")
	}
	apiID, err := strconv.Atoi(apiIDStr)
	if err != nil {
		return nil, fmt.Errorf("invalid APP_ID format: %v", err)
	}
	config.APIID = apiID

	config.APIHash = os.Getenv("APP_HASH")
	if config.APIHash == "" {
		return nil, errors.New("APP_HASH environment variable is not set or empty")
	}

	config.PhoneNumber = os.Getenv("PHONE_NUMBER")
	config.SessionName = os.Getenv("SESSION_NAME")
	if config.SessionName == "" {
		config.SessionName = "default" // Default value if not set
	}

	config.SessionDir = os.Getenv("SESSION_DIR")
	if config.SessionDir == "" {
		config.SessionDir = ".td_session" // Default value if not set
	}

	// Load path settings
	config.ObsidianPath = os.Getenv("OBSIDIAN_PATH")
	if config.ObsidianPath == "" {
		return nil, errors.New("OBSIDIAN_PATH environment variable is not set or empty")
	}
	config.ObsidianPath = filepath.Clean(config.ObsidianPath)

	config.MediaSubdir = os.Getenv("MEDIA_SUBDIR")
	if config.MediaSubdir == "" {
		config.MediaSubdir = "media" // Default value if not set
	}

	config.UseEntityFolders = parseBool(os.Getenv("USE_ENTITY_FOLDERS"), true)

	// Load export targets
	exportTargetsStr := os.Getenv("EXPORT_TARGETS")
	if exportTargetsStr == "" {
		// Legacy support for TELEGRAM_CHANNEL
		exportTargetsStr = os.Getenv("TELEGRAM_CHANNEL")
	}
	
	if exportTargetsStr != "" {
		targets := strings.Split(exportTargetsStr, ",")
		for _, target := range targets {
			target = strings.TrimSpace(target)
			if target != "" {
				config.ExportTargets = append(config.ExportTargets, ExportTarget{
					ID:   target,
					Type: "unknown", // Will be determined later
				})
			}
		}
	}

	// Load processing settings
	config.OnlyNew = parseBool(os.Getenv("ONLY_NEW"), true)
	config.MediaDownload = parseBool(os.Getenv("MEDIA_DOWNLOAD"), true)
	config.Verbose = parseBool(os.Getenv("VERBOSE"), false)
	config.InteractiveMode = parseBool(os.Getenv("INTERACTIVE_MODE"), false)

	// Load parallelism settings
	config.MaxWorkers = parseInt(os.Getenv("MAX_WORKERS"), 4)
	config.MaxProcessWorkers = parseInt(os.Getenv("MAX_PROCESS_WORKERS"), 2)
	config.ConcurrentDownloads = parseInt(os.Getenv("CONCURRENT_DOWNLOADS"), 3)

	// Load cache settings
	cacheInterval := parseInt(os.Getenv("CACHE_SAVE_INTERVAL"), 30)
	config.CacheSaveInterval = time.Duration(cacheInterval) * time.Second

	config.CacheFile = os.Getenv("CACHE_FILE")
	if config.CacheFile == "" {
		config.CacheFile = "export_cache.json" // Default value if not set
	}
	config.CacheFile = filepath.Clean(config.CacheFile)

	// Load API limits
	requestDelayMs := parseInt(os.Getenv("REQUEST_DELAY"), 500)
	config.RequestDelay = time.Duration(requestDelayMs) * time.Millisecond
	config.MessageBatchSize = parseInt(os.Getenv("MESSAGE_BATCH_SIZE"), 100)

	// Load media optimization settings
	config.ImageQuality = parseInt(os.Getenv("IMAGE_QUALITY"), 85)
	config.VideoCRF = parseInt(os.Getenv("VIDEO_CRF"), 28)
	config.VideoPreset = getEnvWithDefault(os.Getenv("VIDEO_PRESET"), "medium")
	config.HWAcceleration = getEnvWithDefault(os.Getenv("HW_ACCELERATION"), "none")
	config.UseH265 = parseBool(os.Getenv("USE_H265"), false)

	// Create necessary directories
	err = os.MkdirAll(config.SessionDir, 0700)
	if err != nil {
		return nil, fmt.Errorf("failed to create session directory '%s': %v", config.SessionDir, err)
	}

	// Create Obsidian path if it doesn't exist
	err = os.MkdirAll(config.ObsidianPath, 0755)
	if err != nil {
		return nil, fmt.Errorf("failed to create Obsidian directory '%s': %v", config.ObsidianPath, err)
	}

	// Update target paths
	err = config.UpdateTargetPaths()
	if err != nil {
		return nil, fmt.Errorf("failed to update target paths: %v", err)
	}

	// Check if we have any export targets
	if len(config.ExportTargets) == 0 && !config.InteractiveMode {
		fmt.Println("Warning: No export targets specified and interactive mode is disabled")
	}

	return config, nil
}

// UpdateTargetPaths calculates and updates the export and media paths for each target
func (c *Config) UpdateTargetPaths() error {
	for i := range c.ExportTargets {
		target := &c.ExportTargets[i]
		
		// Generate entity folder name based on ID and type if UseEntityFolders is true
		var entityPath string
		if c.UseEntityFolders {
			entityPath = SanitizeFilename(target.ID, 255, "_")
		} else {
			entityPath = ""
		}

		// Set export path
		if entityPath != "" {
			target.ExportPath = filepath.Join(c.ObsidianPath, entityPath)
		} else {
			target.ExportPath = c.ObsidianPath
		}

		// Set media path
		if entityPath != "" {
			target.MediaPath = filepath.Join(c.ObsidianPath, c.MediaSubdir, entityPath)
		} else {
			target.MediaPath = filepath.Join(c.ObsidianPath, c.MediaSubdir)
		}

		// Create directories
		err := os.MkdirAll(target.ExportPath, 0755)
		if err != nil {
			return fmt.Errorf("failed to create export directory '%s': %v", target.ExportPath, err)
		}

		err = os.MkdirAll(target.MediaPath, 0755)
		if err != nil {
			return fmt.Errorf("failed to create media directory '%s': %v", target.MediaPath, err)
		}
	}

	return nil
}

// AddExportTarget adds a new export target to the config
func (c *Config) AddExportTarget(target ExportTarget) error {
	c.ExportTargets = append(c.ExportTargets, target)
	return c.UpdateTargetPaths()
}

// GetExportPathForEntity returns the export path for the given entity ID
func (c *Config) GetExportPathForEntity(entityID string) string {
	for _, target := range c.ExportTargets {
		if target.ID == entityID {
			return target.ExportPath
		}
	}
	return c.ObsidianPath
}

// GetMediaPathForEntity returns the media path for the given entity ID
func (c *Config) GetMediaPathForEntity(entityID string) string {
	for _, target := range c.ExportTargets {
		if target.ID == entityID {
			return target.MediaPath
		}
	}
	return filepath.Join(c.ObsidianPath, c.MediaSubdir)
}

// Helper functions

// SanitizeFilename sanitizes a string for use as a filename
func SanitizeFilename(text string, maxLength int, replacement string) string {
	// Replace invalid characters with replacement
	invalid := []string{"<", ">", ":", "\"", "/", "\\", "|", "?", "*"}
	result := text
	for _, char := range invalid {
		result = strings.ReplaceAll(result, char, replacement)
	}

	// Replace spaces with replacement
	result = strings.ReplaceAll(result, " ", replacement)

	// Trim the resulting string to maxLength
	if len(result) > maxLength {
		result = result[:maxLength]
	}

	// Handle reserved Windows filenames
	reserved := []string{"CON", "PRN", "AUX", "NUL", "COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9", "LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9"}
	resultUpper := strings.ToUpper(result)
	for _, name := range reserved {
		if resultUpper == name || strings.HasPrefix(resultUpper, name+".") {
			return "_" + result
		}
	}

	return result
}

// parseBool parses a string to bool with a default value
func parseBool(value string, defaultValue bool) bool {
	if value == "" {
		return defaultValue
	}
	value = strings.ToLower(value)
	return value == "true" || value == "1" || value == "yes" || value == "y"
}

// parseInt parses a string to int with a default value
func parseInt(value string, defaultValue int) int {
	if value == "" {
		return defaultValue
	}
	i, err := strconv.Atoi(value)
	if err != nil {
		return defaultValue
	}
	return i
}

// getEnvWithDefault returns the environment variable value or a default
func getEnvWithDefault(value, defaultValue string) string {
	if value == "" {
		return defaultValue
	}
	return value
}