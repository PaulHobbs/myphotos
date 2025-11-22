package main

import (
	"database/sql"
	"os"
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
	relPath := "2023/vacation/img1.jpg"
	if err := upsertLocal(db, relPath); err != nil {
		t.Fatalf("upsertLocal failed: %v", err)
	}

	// Verify state: Local=1, Remote=0
	assertFileState(t, db, relPath, true, false)

	// 2. Add the SAME file found REMOTELY (This simulates a backup existing)
	// This should update the row, not error out, and not overwrite on_local
	if err := upsertRemote(db, relPath); err != nil {
		t.Fatalf("upsertRemote failed: %v", err)
	}

	// Verify state: Local=1, Remote=1
	assertFileState(t, db, relPath, true, true)

	// 3. Add a NEW file found REMOTELY only
	remoteOnlyPath := "2022/old/img2.jpg"
	if err := upsertRemote(db, remoteOnlyPath); err != nil {
		t.Fatalf("upsertRemote new file failed: %v", err)
	}

	// Verify state: Local=0, Remote=1
	assertFileState(t, db, remoteOnlyPath, false, true)
}

func TestReportingLogic(t *testing.T) {
	db, dbPath := setupTestDB(t)
	defer os.Remove(dbPath)
	defer db.Close()

	// Scenario:
	// File A: Local only (Needs Backup)
	// File B: Remote only (Maybe deleted locally, or archived)
	// File C: Both (Safe)

	upsertLocal(db, "fileA.jpg")
	upsertRemote(db, "fileB.jpg")
	upsertLocal(db, "fileC.jpg")
	upsertRemote(db, "fileC.jpg")

	// Query for "Missing from Remote" (The logic used in runReport)
	rows, err := db.Query("SELECT rel_path FROM files WHERE on_local = 1 AND on_remote = 0")
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
	if len(results) > 0 && results[0] != "fileA.jpg" {
		t.Errorf("Expected fileA.jpg to be missing, got %s", results[0])
	}
}

// --- Helper Functions ---

func assertFileState(t *testing.T, db *sql.DB, relPath string, expectLocal, expectRemote bool) {
	t.Helper()
	var onLocal, onRemote bool
	
	// sqlite stores booleans as integers (0 or 1)
	row := db.QueryRow("SELECT on_local, on_remote FROM files WHERE rel_path = ?", relPath)
	err := row.Scan(&onLocal, &onRemote)
	
	if err == sql.ErrNoRows {
		t.Fatalf("File %s not found in DB", relPath)
	}
	if err != nil {
		t.Fatalf("Error scanning row: %v", err)
	}

	if onLocal != expectLocal {
		t.Errorf("File %s: expected on_local=%v, got %v", relPath, expectLocal, onLocal)
	}
	if onRemote != expectRemote {
		t.Errorf("File %s: expected on_remote=%v, got %v", relPath, expectRemote, onRemote)
	}
}
