package handlers

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"wiki-go/internal/auth"
	"wiki-go/internal/config"
	"wiki-go/internal/logger"
)

// ImportResponse represents the response for the import API
type ImportResponse struct {
	Success   bool   `json:"success"`
	Message   string `json:"message,omitempty"`
	StatusURL string `json:"statusUrl,omitempty"`
	JobID     string `json:"jobId,omitempty"`
}

// ImportStatusResponse represents the status of an import job
type ImportStatusResponse struct {
	Status       string       `json:"status"` // "processing", "completed", "failed"
	Progress     int          `json:"progress"`
	CurrentFile  string       `json:"currentFile,omitempty"`
	SuccessCount int          `json:"successCount"`
	ErrorCount   int          `json:"errorCount"`
	ImportedFiles []ImportedFile `json:"importedFiles,omitempty"`
	Errors       []string     `json:"errors,omitempty"`
	Message      string       `json:"message,omitempty"`
}

// ImportedFile represents a successfully imported file
type ImportedFile struct {
	OriginalPath string `json:"originalPath"`
	NewPath      string `json:"newPath"`
}

// importJobs stores the status of all import jobs
var importJobs = make(map[string]*ImportStatusResponse)
var importJobsMutex sync.RWMutex

// ImportHandler handles the import of documents from a ZIP file
func ImportHandler(w http.ResponseWriter, r *http.Request, cfg *config.Config) {
	// Set appropriate headers
	w.Header().Set("Content-Type", "application/json")

	// Check if user is authenticated and has admin role
	session := auth.GetSession(r)
	if session == nil || session.Role != config.RoleAdmin {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(ImportResponse{
			Success: false,
			Message: "Unauthorized. Admin access required.",
		})
		return
	}

	// Only allow POST method
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		json.NewEncoder(w).Encode(ImportResponse{
			Success: false,
			Message: "Method not allowed. Use POST for import.",
		})
		return
	}

	// Get the maximum upload size from the config
	maxUploadSize := config.GetMaxUploadSizeBytes(cfg)
	maxUploadSizeFormatted := config.GetMaxUploadSizeFormatted(cfg)

	// Parse the multipart form with a maximum size
	err := r.ParseMultipartForm(maxUploadSize)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(ImportResponse{
			Success: false,
			Message: "Failed to parse form or file too large. Maximum size is " + maxUploadSizeFormatted + ".",
		})
		return
	}

	// Get the uploaded file
	file, fileHeader, err := r.FormFile("zipFile")
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(ImportResponse{
			Success: false,
			Message: "Failed to get uploaded file.",
		})
		return
	}
	defer file.Close()

	// Validate file extension
	if !strings.HasSuffix(strings.ToLower(fileHeader.Filename), ".zip") {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(ImportResponse{
			Success: false,
			Message: "Invalid file type. Only ZIP files are allowed.",
		})
		return
	}

	// Read the entire file into memory
	fileBytes, err := io.ReadAll(file)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(ImportResponse{
			Success: false,
			Message: "Failed to read uploaded file.",
		})
		return
	}

	// Generate a unique job ID
	jobID := fmt.Sprintf("import-%d", time.Now().UnixNano())

	// Create initial job status
	importJobsMutex.Lock()
	importJobs[jobID] = &ImportStatusResponse{
		Status:       "processing",
		Progress:     0,
		SuccessCount: 0,
		ErrorCount:   0,
		ImportedFiles: []ImportedFile{},
		Errors:       []string{},
	}
	importJobsMutex.Unlock()

	// The request-level configuration read lock is released when this handler
	// returns, so give the background job its own immutable snapshot.
	configSnapshot := *cfg
	go processImportFromBytes(fileBytes, jobID, &configSnapshot, session.Username)

	// Return success response with job ID
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(ImportResponse{
		Success:   true,
		Message:   "Import started successfully.",
		StatusURL: "/api/import/status/" + jobID,
		JobID:     jobID,
	})
}

// ImportStatusHandler handles requests to check the status of an import job
func ImportStatusHandler(w http.ResponseWriter, r *http.Request, cfg *config.Config) {
	// Set appropriate headers
	w.Header().Set("Content-Type", "application/json")

	// Check if user is authenticated and has admin role
	session := auth.GetSession(r)
	if session == nil || session.Role != config.RoleAdmin {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(ImportResponse{
			Success: false,
			Message: "Unauthorized. Admin access required.",
		})
		return
	}

	// Only allow GET method
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		json.NewEncoder(w).Encode(ImportResponse{
			Success: false,
			Message: "Method not allowed. Use GET for status check.",
		})
		return
	}

	// Extract job ID from URL
	path := strings.TrimPrefix(r.URL.Path, "/api/import/status/")
	jobID := path

	// Check if job exists
	importJobsMutex.RLock()
	status, exists := importJobs[jobID]
	importJobsMutex.RUnlock()

	if !exists {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(ImportResponse{
			Success: false,
			Message: "Import job not found.",
		})
		return
	}

	// Return job status
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(status)
}

// processImportFromBytes processes the import of documents from ZIP file bytes
func processImportFromBytes(zipFileBytes []byte, jobID string, cfg *config.Config, admin string) {
	// Create a reader from the bytes
	zipReader, err := zip.NewReader(bytes.NewReader(zipFileBytes), int64(len(zipFileBytes)))
	if err != nil {
		updateImportStatus(jobID, "failed", 0, "", fmt.Sprintf("Failed to read ZIP file: %v", err))
		return
	}

	// Count total files for progress calculation
	var totalFiles int
	var markdownFiles int
	for _, file := range zipReader.File {
		if !file.FileInfo().IsDir() {
			totalFiles++
			if strings.HasSuffix(strings.ToLower(file.Name), ".md") {
				markdownFiles++
			}
		}
	}

	if markdownFiles == 0 {
		updateImportStatus(jobID, "failed", 0, "", "No markdown files found in the ZIP archive.")
		return
	}

	// Process each file in the ZIP
	processedFiles := 0
	for _, file := range zipReader.File {
		// Skip directories
		if file.FileInfo().IsDir() {
			continue
		}

		// Update current file in status
		updateImportStatusFile(jobID, file.Name)

		// Only process markdown files
		if !strings.HasSuffix(strings.ToLower(file.Name), ".md") {
			processedFiles++
			progress := int((float64(processedFiles) / float64(totalFiles)) * 100)
			updateImportStatusProgress(jobID, progress)
			continue
		}

		// Process the markdown file
		err := processMarkdownFile(file, jobID, cfg)
		if err != nil {
			// Add error but continue processing other files
			addImportError(jobID, fmt.Sprintf("Error processing %s: %v", file.Name, err))
		}

		// Update progress
		processedFiles++
		progress := int((float64(processedFiles) / float64(totalFiles)) * 100)
		updateImportStatusProgress(jobID, progress)
	}

	// Mark job as completed
	importJobsMutex.RLock()
	status := importJobs[jobID]
	importJobsMutex.RUnlock()

	if status.ErrorCount == 0 {
		updateImportStatus(jobID, "completed", 100, "", "Import completed successfully.")
		logger.Info("Admin %s imported %d documents successfully", admin, status.SuccessCount)
	} else if status.SuccessCount == 0 {
		updateImportStatus(jobID, "failed", 100, "", "Import failed. No files were imported successfully.")
		logger.Warn("Admin %s import failed: no files imported", admin)
	} else {
		updateImportStatus(jobID, "completed", 100, "", fmt.Sprintf("Import completed with %d errors.", status.ErrorCount))
		logger.Info("Admin %s imported %d documents (%d errors)", admin, status.SuccessCount, status.ErrorCount)
	}
}

// processMarkdownFile processes a single markdown file from the ZIP
func processMarkdownFile(file *zip.File, jobID string, cfg *config.Config) error {
	// Open the file from the ZIP
	fileReader, err := file.Open()
	if err != nil {
		return fmt.Errorf("failed to open file: %v", err)
	}
	defer fileReader.Close()

	// Read the file content
	content, err := io.ReadAll(fileReader)
	if err != nil {
		return fmt.Errorf("failed to read file: %v", err)
	}

	// Determine the target path based on the file's path in the ZIP
	originalPath := file.Name
	targetPath, err := determineTargetPath(originalPath)
	if err != nil {
		return fmt.Errorf("failed to determine target path: %v", err)
	}

	// Create the full path to the document directory
	docDir := filepath.Join(cfg.Wiki.RootDir, cfg.Wiki.DocumentsDir, targetPath)

	// Create the directory if it doesn't exist
	err = os.MkdirAll(docDir, 0755)
	if err != nil {
		return fmt.Errorf("failed to create directory: %v", err)
	}

	// Write the content to document.md in the target directory
	docPath := filepath.Join(docDir, "document.md")
	
	// Ensure the content has proper permissions
	err = os.WriteFile(docPath, content, 0644)
	if err != nil {
		return fmt.Errorf("failed to write file: %v", err)
	}
	
	// Explicitly set permissions to ensure it's readable and writable
	err = os.Chmod(docPath, 0644)
	if err != nil {
		return fmt.Errorf("failed to set file permissions: %v", err)
	}

	// Add to successful imports
	addImportedFile(jobID, originalPath, "/"+targetPath)

	return nil
}

// determineTargetPath converts an original file path to a target path
func determineTargetPath(originalPath string) (string, error) {
	// Remove file extension
	pathWithoutExt := strings.TrimSuffix(originalPath, filepath.Ext(originalPath))

	// Split path into components
	components := strings.Split(pathWithoutExt, "/")

	// Process each component
	for i, component := range components {
		// Normalize the component
		normalized := normalizePathComponent(component)
		components[i] = normalized
	}

	// Join components back together
	targetPath := strings.Join(components, "/")

	return targetPath, nil
}

// normalizePathComponent normalizes a path component
func normalizePathComponent(component string) string {
	// Convert to lowercase
	component = strings.ToLower(component)

	// Replace spaces with dashes
	component = strings.ReplaceAll(component, " ", "-")

	// Remove special characters
	reg := regexp.MustCompile(`[^a-z0-9-]`)
	component = reg.ReplaceAllString(component, "")

	return component
}

// updateImportStatus updates the status of an import job
func updateImportStatus(jobID, status string, progress int, currentFile, message string) {
	importJobsMutex.Lock()
	defer importJobsMutex.Unlock()

	if job, exists := importJobs[jobID]; exists {
		job.Status = status
		job.Progress = progress
		job.CurrentFile = currentFile
		job.Message = message
	}
}

// updateImportStatusProgress updates just the progress of an import job
func updateImportStatusProgress(jobID string, progress int) {
	importJobsMutex.Lock()
	defer importJobsMutex.Unlock()

	if job, exists := importJobs[jobID]; exists {
		job.Progress = progress
	}
}

// updateImportStatusFile updates the current file of an import job
func updateImportStatusFile(jobID, currentFile string) {
	importJobsMutex.Lock()
	defer importJobsMutex.Unlock()

	if job, exists := importJobs[jobID]; exists {
		job.CurrentFile = currentFile
	}
}

// addImportedFile adds a successfully imported file to the job status
func addImportedFile(jobID, originalPath, newPath string) {
	importJobsMutex.Lock()
	defer importJobsMutex.Unlock()

	if job, exists := importJobs[jobID]; exists {
		job.ImportedFiles = append(job.ImportedFiles, ImportedFile{
			OriginalPath: originalPath,
			NewPath:      newPath,
		})
		job.SuccessCount++
	}
}

// addImportError adds an error to the job status
func addImportError(jobID, errorMsg string) {
	importJobsMutex.Lock()
	defer importJobsMutex.Unlock()

	if job, exists := importJobs[jobID]; exists {
		job.Errors = append(job.Errors, errorMsg)
		job.ErrorCount++
	}
}
