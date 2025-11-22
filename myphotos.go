package main

import (
	"bufio"
	"bytes"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"

	_ "github.com/mattn/go-sqlite3"
	"github.com/pelletier/go-toml/v2"
)

// --- Configuration Structures ---

type Config struct {
	Extensions []string `toml:"extensions"`
}

func getDefaultConfig() Config {
	return Config{
		Extensions: []string{".jpg", ".JPG", ".ARW", ".mp4", ".MP4"},
	}
}

func getDefaultDBPath() string {
	usr, err := user.Current()
	if err != nil {
		return "db.sqlite"
	}
	return filepath.Join(usr.HomeDir, ".config", "myphotos", "db.sqlite")
}

// --- Database Logic ---

func initDB(dbPath string) (*sql.DB, error) {
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, err
	}

	// Create table if it doesn't exist
	// We use the relative path as the PRIMARY KEY to identify the file
	query := `
	CREATE TABLE IF NOT EXISTS files (
		rel_path TEXT PRIMARY KEY,
		on_local BOOLEAN DEFAULT 0,
		on_remote BOOLEAN DEFAULT 0
	);`

	_, err = db.Exec(query)
	if err != nil {
		return nil, err
	}

	return db, nil
}

func upsertLocal(db *sql.DB, relPath string) error {
	// Insert: set on_local=1. On Conflict: update on_local=1 (leave on_remote alone)
	query := `
	INSERT INTO files (rel_path, on_local, on_remote) VALUES (?, 1, 0)
	ON CONFLICT(rel_path) DO UPDATE SET on_local=1;
	`
	_, err := db.Exec(query, relPath)
	return err
}

func upsertRemote(db *sql.DB, relPath string) error {
	// Insert: set on_remote=1. On Conflict: update on_remote=1 (leave on_local alone)
	query := `
	INSERT INTO files (rel_path, on_local, on_remote) VALUES (?, 0, 1)
	ON CONFLICT(rel_path) DO UPDATE SET on_remote=1;
	`
	_, err := db.Exec(query, relPath)
	return err
}

// --- Configuration Management ---

func loadOrCreateConfig() (*Config, error) {
	usr, err := user.Current()
	if err != nil {
		return nil, fmt.Errorf("could not get user home: %v", err)
	}

	configDir := filepath.Join(usr.HomeDir, ".config", "myphotos")
	configFile := filepath.Join(configDir, "extensions.toml")

	// Check if file exists
	if _, err := os.Stat(configFile); os.IsNotExist(err) {
		// Create default
		if err := os.MkdirAll(configDir, 0755); err != nil {
			return nil, err
		}

		cfg := getDefaultConfig()
		data, err := toml.Marshal(cfg)
		if err != nil {
			return nil, err
		}

		if err := os.WriteFile(configFile, data, 0644); err != nil {
			return nil, err
		}
		fmt.Printf("Created default config at %s\n", configFile)
		return &cfg, nil
	}

	// Read existing
	data, err := os.ReadFile(configFile)
	if err != nil {
		return nil, err
	}

	var cfg Config
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// --- Main Logic ---

func isExtensionAllowed(path string, extensions []string) bool {
	for _, ext := range extensions {
		if strings.HasSuffix(path, ext) {
			return true
		}
	}
	return false
}

func runAdd(args []string, cfg *Config) {
	defaultDB := getDefaultDBPath()
	addCmd := flag.NewFlagSet("add", flag.ExitOnError)
	remotePtr := addCmd.String("remote", "", "The remote server address (e.g. user@192.168.1.100). If empty, scans local.")
	pathPtr := addCmd.String("path", "", "The directory path to scan")
	dbPtr := addCmd.String("db", defaultDB, "Path to the sqlite database file")

	addCmd.Parse(args)

	if *pathPtr == "" {
		fmt.Println("Error: -path is required")
		addCmd.PrintDefaults()
		os.Exit(1)
	}

	db, err := initDB(*dbPtr)
	if err != nil {
		log.Fatalf("Failed to initialize DB: %v", err)
	}
	defer db.Close()

	// --- Remote Scan (via SSH) ---
	if *remotePtr != "" {
		fmt.Printf("Scanning REMOTE [%s] at path [%s]...\n", *remotePtr, *pathPtr)
		
		// We construct a find command to run over SSH. 
		// We use -type f to only get files.
		sshCmd := exec.Command("ssh", *remotePtr, "find", *pathPtr, "-type", "f")
		
		// Capture output
		var out bytes.Buffer
		var stderr bytes.Buffer
		sshCmd.Stdout = &out
		sshCmd.Stderr = &stderr

		err := sshCmd.Run()
		if err != nil {
			log.Fatalf("SSH command failed: %v\nStderr: %s", err, stderr.String())
		}

		scanner := bufio.NewScanner(&out)
		count := 0
		tx, _ := db.Begin() // Use transaction for speed

		for scanner.Scan() {
			fullPath := scanner.Text()
			if !isExtensionAllowed(fullPath, cfg.Extensions) {
				continue
			}

			// Calculate relative path to store in DB
			relPath, err := filepath.Rel(*pathPtr, fullPath)
			if err != nil {
				// If rel path fails (weird mount issues), fallback to filename
				relPath = filepath.Base(fullPath)
			}

			if err := upsertRemote(db, relPath); err != nil {
				log.Printf("Error inserting remote file %s: %v", relPath, err)
			}
			count++
			if count%100 == 0 {
				fmt.Printf("\rProcessed %d remote files...", count)
			}
		}
		tx.Commit()
		fmt.Printf("\nComplete. Processed %d matching remote files.\n", count)

	} else {
		// --- Local Scan ---
		fmt.Printf("Scanning LOCAL path [%s]...\n", *pathPtr)

		count := 0
		err := filepath.WalkDir(*pathPtr, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}

			if !isExtensionAllowed(path, cfg.Extensions) {
				return nil
			}

			relPath, err := filepath.Rel(*pathPtr, path)
			if err != nil {
				return err
			}

			if err := upsertLocal(db, relPath); err != nil {
				return err
			}
			
			count++
			if count%100 == 0 {
				fmt.Printf("\rProcessed %d local files...", count)
			}
			return nil
		})

		if err != nil {
			log.Fatalf("Error walking local path: %v", err)
		}
		fmt.Printf("\nComplete. Processed %d matching local files.\n", count)
	}
}

func runReport(args []string) {
	defaultDB := getDefaultDBPath()
	reportCmd := flag.NewFlagSet("report", flag.ExitOnError)
	dbPtr := reportCmd.String("db", defaultDB, "Path to the sqlite database file")
	reportCmd.Parse(args)

	db, err := initDB(*dbPtr)
	if err != nil {
		log.Fatalf("Failed to open DB: %v", err)
	}
	defer db.Close()

	// Query for files that are local (backed up?) but NOT on remote, or vice versa.
	// Usually, backup verification means "Show me what is Local but NOT Remote"
	
	rows, err := db.Query("SELECT rel_path FROM files WHERE on_local = 1 AND on_remote = 0")
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()

	fmt.Println("--- Files on Local Machine but MISSING from Remote (Backup needed) ---")
	count := 0
	for rows.Next() {
		var p string
		rows.Scan(&p)
		fmt.Println(p)
		count++
	}
	fmt.Printf("---------------------------------------------------------------------\n")
	fmt.Printf("Total Missing from Remote: %d\n", count)
}

func printHelp() {
	fmt.Println("Usage: go run myphotos.go <command> [flags]")
	fmt.Println("\nCommands:")
	fmt.Println("  add     Scan and add files to the database")
	fmt.Println("  report  Generate a report of missing files")
	fmt.Println("  help    Show this help message")
}

func main() {
	cfg, err := loadOrCreateConfig()
	if err != nil {
		log.Fatalf("Config error: %v", err)
	}

	if len(os.Args) < 2 {
		printHelp()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "add":
		runAdd(os.Args[2:], cfg)
	case "report":
		runReport(os.Args[2:])
	case "help", "--help", "-h":
		printHelp()
	default:
		fmt.Printf("Unknown command: %s\n", os.Args[1])
		printHelp()
		os.Exit(1)
	}
}
