package main

import (
	"archive/zip"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

// --- Test Configuration & Extensions ---

func TestIsExtensionAllowed(t *testing.T) {
	extensions := []string{".jpg", ".JPG", ".mp4"}

	tests := []struct {
		filename string
		expected bool
	}{
		{"photo.jpg", true},
		{"photo.JPG", true},
		{"video.mp4", true},
		{"document.txt", false},
		{"image.png", false},
		{"folder.jpg/real_file.txt", false}, // Path ends in .txt
		{"/absolute/path/to/photo.jpg", true},
	}

	for _, tt := range tests {
		result := isExtensionAllowed(tt.filename, extensions)
		if result != tt.expected {
			t.Errorf("isExtensionAllowed(%q) = %v; want %v", tt.filename, result, tt.expected)
		}
	}
}

// --- Test Database Logic ---

func setupTestDB(t *testing.T) (*sql.DB, string) {
	// Create a temporary file for the database
	tempFile, err := os.CreateTemp("", "test_photos_*.db")
	if err != nil {
		t.Fatalf("Failed to create temp db file: %v", err)
	}
	tempFile.Close() // Close it so sqlite can open it

	db, err := initDB(tempFile.Name())
	if err != nil {
		t.Fatalf("Failed to init DB: %v", err)
	}

	return db, tempFile.Name()
}

func TestDatabaseUpsertFlow(t *testing.T) {
	db, dbPath := setupTestDB(t)
	defer os.Remove(dbPath)
	defer db.Close()

	// 1. Add a file found LOCALLY
	filename := "img1.jpg"
	size := int64(1024)
	localPath := "/local/2023/vacation/img1.jpg"
	if err := upsertLocal(db, filename, size, localPath); err != nil {
		t.Fatalf("upsertLocal failed: %v", err)
	}

	// Verify state: Local=1, Remote=0
	assertFileState(t, db, filename, size, true, false)

	// 2. Add the SAME file found REMOTELY (This simulates a backup existing)
	// This should update the row, not error out, and not overwrite on_local
	remotePath := "/remote/backup/img1.jpg"
	if err := upsertRemote(db, filename, size, remotePath); err != nil {
		t.Fatalf("upsertRemote failed: %v", err)
	}

	// Verify state: Local=1, Remote=1
	assertFileState(t, db, filename, size, true, true)

	// 3. Add a NEW file found REMOTELY only
	if err := upsertRemote(db, "img2.jpg", 2048, "/remote/old/img2.jpg"); err != nil {
		t.Fatalf("upsertRemote new file failed: %v", err)
	}

	// Verify state: Local=0, Remote=1
	assertFileState(t, db, "img2.jpg", 2048, false, true)
}

func TestReportingLogic(t *testing.T) {
	db, dbPath := setupTestDB(t)
	defer os.Remove(dbPath)
	defer db.Close()

	// Scenario:
	// File A: Local only (Needs Backup)
	// File B: Remote only (Maybe deleted locally, or archived)
	// File C: Both (Safe)

	upsertLocal(db, "fileA.jpg", 100, "/local/fileA.jpg")
	upsertRemote(db, "fileB.jpg", 200, "/remote/fileB.jpg")
	upsertLocal(db, "fileC.jpg", 300, "/local/fileC.jpg")
	upsertRemote(db, "fileC.jpg", 300, "/remote/fileC.jpg")

	// Query for "Missing from Remote" (The logic used in runReport)
	rows, err := db.Query("SELECT local_path FROM photos WHERE local_path IS NOT NULL AND (remote_path IS NULL OR remote_path = '')")
	if err != nil {
		t.Fatalf("Query failed: %v", err)
	}
	defer rows.Close()

	var results []string
	for rows.Next() {
		var p string
		rows.Scan(&p)
		results = append(results, p)
	}

	// Assertions
	if len(results) != 1 {
		t.Errorf("Expected 1 missing file, got %d", len(results))
	}
	if len(results) > 0 && results[0] != "/local/fileA.jpg" {
		t.Errorf("Expected /local/fileA.jpg to be missing, got %s", results[0])
	}
}

// --- Helper Functions ---

func assertFileState(t *testing.T, db *sql.DB, filename string, size int64, expectLocal, expectRemote bool) {
	t.Helper()
	var localPath, remotePath sql.NullString
	
	row := db.QueryRow("SELECT local_path, remote_path FROM photos WHERE filename = ? AND size = ?", filename, size)
	err := row.Scan(&localPath, &remotePath)
	
	if err == sql.ErrNoRows {
		t.Fatalf("File %s not found in DB", filename)
	}
	if err != nil {
		t.Fatalf("Error scanning row: %v", err)
	}

	onLocal := localPath.Valid && localPath.String != ""
	onRemote := remotePath.Valid && remotePath.String != ""

	if onLocal != expectLocal {
		t.Errorf("File %s: expected on_local=%v, got %v", filename, expectLocal, onLocal)
	}
	if onRemote != expectRemote {
		t.Errorf("File %s: expected on_remote=%v, got %v", filename, expectRemote, onRemote)
	}
}

func TestZipMissing(t *testing.T) {
	db, dbPath := setupTestDB(t)
	defer os.Remove(dbPath)
	defer db.Close()

	// Create a fake home directory
	fakeHome, err := os.MkdirTemp("", "fake_home")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(fakeHome)

	// Create a dummy file that is "missing" from remote
	relPath := "Pictures/vacation/missing.jpg"
	fullPath := filepath.Join(fakeHome, relPath)
	if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
		t.Fatal(err)
	}
	content := []byte("image data")
	if err := os.WriteFile(fullPath, content, 0644); err != nil {
		t.Fatal(err)
	}

	// Add to DB as a local file
	if err := upsertLocal(db, "missing.jpg", int64(len(content)), fullPath); err != nil {
		t.Fatal(err)
	}

	zipPath := filepath.Join(fakeHome, "output.zip")
	if err := zipMissingFiles(db, zipPath, fakeHome); err != nil {
		t.Fatalf("zipMissingFiles failed: %v", err)
	}

	// Verify Zip contents
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		t.Fatalf("Failed to open zip: %v", err)
	}
	defer r.Close()

	if len(r.File) != 1 {
		t.Errorf("Expected 1 file in zip, got %d", len(r.File))
	} else {
		f := r.File[0]
		// We expect the path in zip to be relative to home, and use forward slashes
		expectedName := "Pictures/vacation/missing.jpg"
		if f.Name != expectedName {
			t.Errorf("Expected zip entry name %s, got %s", expectedName, f.Name)
		}

		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		defer rc.Close()
		// We could verify content here if needed
	}
}
