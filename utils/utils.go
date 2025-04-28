package utils

import (
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Logging levels
const (
	LogLevelError = 0
	LogLevelWarn  = 1
	LogLevelInfo  = 2
	LogLevelDebug = 3
)

var (
	// Global logger
	logger     *log.Logger
	logLevel   int
	logFile    *os.File
	loggerOnce sync.Once
)

// SetupLogging configures the logging system
func SetupLogging(verbose bool) {
	loggerOnce.Do(func() {
		// Set log level based on verbose flag
		if verbose {
			logLevel = LogLevelDebug
		} else {
			logLevel = LogLevelInfo
		}

		// Create log file
		var err error
		logFile, err = os.OpenFile("telegram_exporter.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
		if err != nil {
			fmt.Printf("Error opening log file: %v\n", err)
			// Fall back to stdout only
			logger = log.New(os.Stdout, "", log.Ldate|log.Ltime)
			return
		}

		// Create multi-writer to log to both file and stdout
		multiWriter := io.MultiWriter(os.Stdout, logFile)
		logger = log.New(multiWriter, "", log.Ldate|log.Ltime)
	})
}

// LogError logs an error message
func LogError(format string, args ...any) {
	if logger == nil {
		SetupLogging(false)
	}
	if logLevel >= LogLevelError {
		logger.Printf("[ERROR] "+format, args...)
	}
}

// LogWarn logs a warning message
func LogWarn(format string, args ...any) {
	if logger == nil {
		SetupLogging(false)
	}
	if logLevel >= LogLevelWarn {
		logger.Printf("[WARN]  "+format, args...)
	}
}

// LogInfo logs an info message
func LogInfo(format string, args ...any) {
	if logger == nil {
		SetupLogging(false)
	}
	if logLevel >= LogLevelInfo {
		logger.Printf("[INFO]  "+format, args...)
	}
}

// LogDebug logs a debug message
func LogDebug(format string, args ...any) {
	if logger == nil {
		SetupLogging(false)
	}
	if logLevel >= LogLevelDebug {
		logger.Printf("[DEBUG] "+format, args...)
	}
}

// CloseLogger closes the log file
func CloseLogger() {
	if logFile != nil {
		logFile.Close()
	}
}

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

// GetRelativePath calculates the relative path from base to target
func GetRelativePath(targetPath, basePath string) string {
	// Convert to absolute paths to ensure correct calculation
	absTarget, err := filepath.Abs(targetPath)
	if err != nil {
		LogError("Failed to get absolute path for %s: %v", targetPath, err)
		return ""
	}

	absBase, err := filepath.Abs(basePath)
	if err != nil {
		LogError("Failed to get absolute path for %s: %v", basePath, err)
		return ""
	}

	// Calculate relative path
	relPath, err := filepath.Rel(absBase, absTarget)
	if err != nil {
		LogError("Failed to get relative path from %s to %s: %v", absBase, absTarget, err)
		return ""
	}

	// Convert to POSIX-style path
	relPath = filepath.ToSlash(relPath)

	// URL encode path components
	parts := strings.Split(relPath, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}

	return strings.Join(parts, "/")
}

// EnsureDirExists ensures a directory exists
func EnsureDirExists(path string) error {
	return os.MkdirAll(path, 0755)
}

// ProcessParallel processes items in parallel with a semaphore
func ProcessParallel(items []any, processor func(any) error, maxConcurrency int, description string) []error {
	var wg sync.WaitGroup
	semaphore := make(chan struct{}, maxConcurrency)
	errChan := make(chan error, len(items))

	for i, item := range items {
		wg.Add(1)
		semaphore <- struct{}{} // Acquire semaphore

		go func(idx int, itm any) {
			defer wg.Done()
			defer func() { <-semaphore }() // Release semaphore

			LogDebug("%s: Processing item %d/%d", description, idx+1, len(items))
			err := processor(itm)
			if err != nil {
				errChan <- err
			}
		}(i, item)
	}

	wg.Wait()
	close(errChan)

	// Collect any errors
	var errors []error
	for err := range errChan {
		errors = append(errors, err)
	}

	return errors
}
